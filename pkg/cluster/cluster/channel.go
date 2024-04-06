package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/cluster/replica"
	"github.com/WuKongIM/WuKongIM/pkg/trace"
	"github.com/WuKongIM/WuKongIM/pkg/wkdb"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/sasha-s/go-deadlock"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type channel struct {
	channelID                  string
	channelType                uint8
	rc                         *replica.Replica          // 副本服务
	destroy                    bool                      // 是否已经销毁
	clusterConfig              wkdb.ChannelClusterConfig // 分布式配置
	maxHandleReadyCountOfBatch int                       // 每批次处理ready的最大数量
	opts                       *Options
	lastActivity               atomic.Time // 最后一次活跃时间
	traceRecord                *traceRecord
	doneC                      chan struct{}
	wklog.Log
	prev *channel
	next *channel

	mu           deadlock.Mutex
	localstorage *localStorage
	eventHandler *eventHandler

	leaderId   atomic.Uint64
	advanceFnc func() // 推进分布式进程
}

func newChannel(clusterConfig wkdb.ChannelClusterConfig, appliedIdx uint64, localstorage *localStorage, advance func(), opts *Options) *channel {
	shardNo := ChannelKey(clusterConfig.ChannelId, clusterConfig.ChannelType)
	rc := replica.New(opts.NodeID, shardNo, replica.WithAppliedIndex(appliedIdx), replica.WithReplicas(clusterConfig.Replicas), replica.WithStorage(newProxyReplicaStorage(shardNo, opts.MessageLogStorage, localstorage)))
	ch := &channel{
		maxHandleReadyCountOfBatch: 50,
		rc:                         rc,
		opts:                       opts,
		Log:                        wklog.NewWKLog(fmt.Sprintf("Channel[%s]", shardNo)),
		channelID:                  clusterConfig.ChannelId,
		channelType:                clusterConfig.ChannelType,
		clusterConfig:              clusterConfig,
		doneC:                      make(chan struct{}),
		localstorage:               localstorage,
		traceRecord:                newTraceRecord(),
		advanceFnc:                 advance,
	}
	ch.lastActivity.Store(time.Now())
	ch.eventHandler = newEventHandler(ch, ch.Log, opts, ch.doneC)
	return ch
}

func (c *channel) updateClusterConfig(clusterConfig wkdb.ChannelClusterConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusterConfig = clusterConfig
	c.rc.SetReplicas(clusterConfig.Replicas)
	c.setLeaderId(clusterConfig.LeaderId)

	if clusterConfig.LeaderId == c.opts.NodeID {
		c.rc.BecomeLeader(clusterConfig.Term)
	} else {
		c.rc.BecomeFollower(clusterConfig.Term, clusterConfig.LeaderId)
	}
}

func (c *channel) updateClusterConfigLeaderIdAndTerm(term uint32, leaderId uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusterConfig.Term = term
	c.clusterConfig.LeaderId = leaderId
}

func (c *channel) ready() replica.Ready {
	if c.destroy {
		return replica.Ready{}
	}
	return c.rc.Ready()
}

func (c *channel) hasReady() bool {
	if c.destroy {
		return false
	}
	return c.rc.HasReady()
}

// // 任命为领导
// func (c *channel) appointLeader(term uint32) error {

// 	return c.stepLock(replica.Message{
// 		MsgType:           replica.MsgAppointLeaderReq,
// 		AppointmentLeader: c.opts.NodeID,
// 		Term:              term,
// 	})

// }

// // 任命指定节点为领导
// func (c *channel) appointLeaderTo(term uint32, to uint64) error {
// 	return c.stepLock(replica.Message{
// 		MsgType:           replica.MsgAppointLeaderReq,
// 		AppointmentLeader: to,
// 		Term:              term,
// 	})
// }

// func (c *channel) stepLock(msg replica.Message) error {
// 	c.mu.Lock()
// 	err := c.step(msg)
// 	c.mu.Unlock()
// 	return err

// }

func (c *channel) step(msg replica.Message) error {
	if c.destroy {
		return errors.New("channel destroy, can not step")
	}
	c.lastActivity.Store(time.Now())
	return c.rc.Step(msg)
}

// 提案数据，并等待数据提交给大多数节点
func (c *channel) proposeAndWaitCommits(ctx context.Context, logs []replica.Log, timeout time.Duration) ([]messageItem, error) {

	if c.destroy {
		return nil, errors.New("channel destroy, can not propose")
	}
	c.lastActivity.Store(time.Now())
	return c.eventHandler.proposeAndWaitCommits(ctx, logs, timeout)
}

func (c *channel) channelKey() string {
	return ChannelKey(c.channelID, c.channelType)
}

func (c *channel) makeDestroy() {
	c.destroy = true
	close(c.doneC)
}

func (c *channel) isDestroy() bool {
	return c.destroy
}

func (c *channel) getLastActivity() time.Time {
	return c.lastActivity.Load()
}

func (c *channel) getAndMergeLogs(msg replica.Message) ([]replica.Log, error) {

	unstableLogs := msg.Logs
	startIndex := msg.Index
	if len(unstableLogs) > 0 {
		startIndex = unstableLogs[len(unstableLogs)-1].Index + 1
	}

	messageWaitItems := c.eventHandler.messageWait.waitItemsWithStartSeq(startIndex)
	spans := make([]trace.Span, 0, len(messageWaitItems))
	for _, messageWaitItem := range messageWaitItems {
		_, span := trace.GlobalTrace.StartSpan(messageWaitItem.ctx, fmt.Sprintf("logsGet[node %d]", c.opts.NodeID))
		defer span.End()
		span.SetUint64("startIndex", startIndex)
		span.SetInt("unstableLogs", len(unstableLogs))
		spans = append(spans, span)

	}

	lastIndex, err := c.opts.MessageLogStorage.LastIndex(ChannelKey(c.channelID, c.channelType))
	if err != nil {
		c.Error("handleSyncGet: get last index error", zap.Error(err))
		return nil, err
	}
	var resultLogs []replica.Log
	if startIndex <= lastIndex {
		logs, err := c.getLogs(startIndex, lastIndex+1, uint64(c.opts.LogSyncLimitSizeOfEach))
		if err != nil {
			c.Error("get logs error", zap.Error(err), zap.Uint64("startIndex", startIndex), zap.Uint64("lastIndex", lastIndex))
			return nil, err
		}
		resultLogs = extend(logs, unstableLogs)
	} else {
		// c.Warn("handleSyncGet: startIndex > lastIndex", zap.Uint64("startIndex", startIndex), zap.Uint64("lastIndex", lastIndex))
	}
	for _, span := range spans {
		span.SetUint64("lastIndex", lastIndex)
		span.SetInt("resultLogs", len(resultLogs))
	}
	return resultLogs, nil
}

func extend(dst, vals []replica.Log) []replica.Log {
	need := len(dst) + len(vals)
	if need <= cap(dst) {
		return append(dst, vals...) // does not allocate
	}
	buf := make([]replica.Log, need) // allocates precisely what's needed
	copy(buf, dst)
	copy(buf[len(dst):], vals)
	return buf
}

func (c *channel) getLogs(startLogIndex uint64, endLogIndex uint64, limitSize uint64) ([]replica.Log, error) {
	logs, err := c.opts.MessageLogStorage.Logs(ChannelKey(c.channelID, c.channelType), startLogIndex, endLogIndex, limitSize)
	if err != nil {
		c.Error("get logs error", zap.Error(err))
		return nil, err
	}
	return logs, nil
}

func (c *channel) appendLogs(msg replica.Message) error {
	shardNo := ChannelKey(c.channelID, c.channelType)

	firstLog := msg.Logs[0]
	lastLog := msg.Logs[len(msg.Logs)-1]

	messageWaitItems := c.eventHandler.messageWait.waitItemsWithRange(firstLog.Index, lastLog.Index+1)
	for _, messageWaitItem := range messageWaitItems {
		_, span := trace.GlobalTrace.StartSpan(messageWaitItem.ctx, fmt.Sprintf("logsAppend[node %d]", c.opts.NodeID))
		defer span.End()
		span.SetInt("logCount", len(msg.Logs))
		span.SetUint64("firstLogIndex", firstLog.Index)
		span.SetUint64("lastLogIndex", lastLog.Index)
	}

	start := time.Now()

	c.Debug("append log", zap.Uint64("lastLogIndex", lastLog.Index))
	err := c.opts.MessageLogStorage.AppendLog(shardNo, msg.Logs)
	if err != nil {
		c.Panic("append log error", zap.Error(err))
	}
	c.Debug("append log done", zap.Uint64("lastLogIndex", lastLog.Index), zap.Duration("cost", time.Since(start)))

	return nil

}

func (c *channel) applyLogs(msg replica.Message) error {
	if msg.ApplyingIndex > msg.CommittedIndex {
		return fmt.Errorf("ApplyingIndex > CommittedIndex, applyingIndex: %d, committedIndex: %d", msg.ApplyingIndex, msg.CommittedIndex)
	}
	messageWaitItems := c.eventHandler.messageWait.waitItemsWithRange(msg.ApplyingIndex+1, msg.CommittedIndex+1)
	spans := make([]trace.Span, 0, len(messageWaitItems))
	for _, messageWaitItem := range messageWaitItems {
		_, span := trace.GlobalTrace.StartSpan(messageWaitItem.ctx, fmt.Sprintf("logsCommit[node %d]", c.opts.NodeID))
		defer span.End()
		span.SetUint64("appliedIndex", msg.AppliedIndex)
		span.SetUint64("committedIndex", msg.CommittedIndex)
		spans = append(spans, span)
	}

	startNow := time.Now()
	c.Debug("commit wait", zap.Uint64("committedIndex", msg.CommittedIndex))
	c.eventHandler.messageWait.didCommit(msg.ApplyingIndex+1, msg.CommittedIndex+1)
	for _, span := range spans {
		span.End()
	}
	c.Debug("commit wait done", zap.Duration("cost", time.Since(startNow)), zap.Uint64("applyingIndex", msg.ApplyingIndex), zap.Uint64("committedIndex", msg.CommittedIndex))
	return nil

}

func (c *channel) handleReadyMessages(msgs []replica.Message) {
	c.lastActivity.Store(time.Now())
	c.eventHandler.handleReadyMessages(msgs)
}

func (c *channel) handleRecvMessage(msg replica.Message) error {
	if c.destroy {
		return errors.New("channel destroy, can not handle message")
	}
	// if msg.MsgType == replica.MsgSync {
	// 	c.Debug("接收MsgSync消息", zap.Uint64("index", msg.Index), zap.Uint64("from", msg.From))
	// }
	c.lastActivity.Store(time.Now())
	return c.eventHandler.handleRecvMessage(msg)

}

func (c *channel) sendMessage(msg replica.Message) {
	shardNo := c.channelKey()
	protMsg, err := NewMessage(shardNo, msg, MsgChannelMsg)
	if err != nil {
		c.Error("new message error", zap.String("channelID", c.channelID), zap.Uint8("channelType", c.channelType), zap.Error(err))
		return
	}
	if msg.MsgType != replica.MsgSync && msg.MsgType != replica.MsgSyncResp && msg.MsgType != replica.MsgPing && msg.MsgType != replica.MsgPong {
		c.Info("发送消息", zap.Uint64("id", msg.Id), zap.String("msgType", msg.MsgType.String()), zap.String("channelID", c.channelID), zap.Uint8("channelType", c.channelType), zap.Uint64("to", msg.To), zap.Uint32("term", msg.Term), zap.Uint64("index", msg.Index))
	}

	if msg.MsgType == replica.MsgSync {
		task := newSyncTask(msg.To, msg.Index)
		if !c.eventHandler.existsSyncTask(task.taskKey()) {
			c.eventHandler.addSyncTask(task)
		} else {
			c.Debug("sync task exists", zap.Uint64("to", msg.To), zap.Uint64("index", msg.Index))
		}

	}
	// trace
	traceOutgoingMessage(trace.ClusterKindChannel, msg)

	// 发送消息
	err = c.opts.Transport.Send(msg.To, protMsg, nil)
	if err != nil {
		c.Warn("send msg error", zap.String("msgType", msg.MsgType.String()), zap.Uint64("to", msg.To), zap.String("channelID", c.channelID), zap.Uint8("channelType", c.channelType), zap.Error(err))
	}
}

func (c *channel) handleEvents() (bool, error) {
	return c.eventHandler.handleEvents()
}

func (c *channel) advance() {
	c.advanceFnc()
}

func (c *channel) newMsgApplyLogsRespMessage(index uint64) replica.Message {
	return c.rc.NewMsgApplyLogsRespMessage(index)
}

func (c *channel) newProposeMessageWithLogs(logs []replica.Log) replica.Message {
	return c.rc.NewProposeMessageWithLogs(logs)
}

func (c *channel) newMsgSyncGetResp(to uint64, startIndex uint64, logs []replica.Log) replica.Message {
	return c.rc.NewMsgSyncGetResp(to, startIndex, logs)
}

func (c *channel) newMsgStoreAppendResp(index uint64) replica.Message {
	return c.rc.NewMsgStoreAppendResp(index)
}

func (c *channel) lastLogIndexNoLock() uint64 {
	return c.rc.LastLogIndex()
}

func (c *channel) termNoLock() uint32 {
	return c.rc.Term()
}

func (c *channel) setLastIndex(lastIndex uint64) error {
	shardNo := ChannelKey(c.channelID, c.channelType)
	err := c.opts.MessageLogStorage.SetLastIndex(shardNo, lastIndex)
	if err != nil {
		c.Error("set last index error", zap.Error(err))
		return err
	}
	return nil
}

func (c *channel) setAppliedIndex(appliedIndex uint64) error {
	shardNo := ChannelKey(c.channelID, c.channelType)
	err := c.localstorage.setAppliedIndex(shardNo, appliedIndex)
	if err != nil {
		c.Error("set applied index error", zap.Error(err))
		return err
	}
	return nil
}

// func (c *channel) LastLogIndex() uint64 {
// 	c.mu.Lock()
// 	defer c.mu.Unlock()
// 	return c.rc.LastLogIndex()
// }

// func (c *channel) Term() uint32 {
// 	c.mu.Lock()
// 	defer c.mu.Unlock()
// 	return c.rc.Term()
// }

func (c *channel) setLeaderId(leaderId uint64) {
	c.leaderId.Store(leaderId)
}

func (c *channel) IsLeader() bool {

	return c.opts.NodeID == c.LeaderId()
}

func (c *channel) LeaderId() uint64 {
	return c.leaderId.Load()
}

func (c *channel) getClusterConfig() wkdb.ChannelClusterConfig {
	return c.clusterConfig
}

type ichannel interface {
	IsLeader() bool
	proposeAndWaitCommits(ctx context.Context, logs []replica.Log, timeout time.Duration) ([]messageItem, error)
	LeaderId() uint64
	handleRecvMessage(msg replica.Message) error
	getClusterConfig() wkdb.ChannelClusterConfig
}

type proxyChannel struct {
	nodeId     uint64
	clusterCfg wkdb.ChannelClusterConfig
	mu         sync.Mutex
}

func newProxyChannel(nodeId uint64, clusterCfg wkdb.ChannelClusterConfig) *proxyChannel {
	return &proxyChannel{
		nodeId:     nodeId,
		clusterCfg: clusterCfg,
	}
}

func (p *proxyChannel) IsLeader() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clusterCfg.LeaderId == p.nodeId
}

func (p *proxyChannel) proposeAndWaitCommits(ctx context.Context, logs []replica.Log, timeout time.Duration) ([]messageItem, error) {
	panic("proposeAndWaitCommits: implement me")
}

func (p *proxyChannel) LeaderId() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clusterCfg.LeaderId
}

func (p *proxyChannel) handleRecvMessage(msg replica.Message) error {
	panic("handleMessage: implement me")
}

func (p *proxyChannel) getClusterConfig() wkdb.ChannelClusterConfig {
	return p.clusterCfg
}
