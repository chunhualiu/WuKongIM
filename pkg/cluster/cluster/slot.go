package cluster

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/cluster/cluster/clusterconfig/pb"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/replica"
	"github.com/WuKongIM/WuKongIM/pkg/trace"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type slot struct {
	rc      *replica.Replica
	slotId  uint32
	shardNo string
	wklog.Log
	opts         *Options
	destroy      bool        // 是否已经销毁
	lastActivity atomic.Time // 最后一次活跃时间

	doneC chan struct{}
	sync.Mutex

	next *slot
	prev *slot

	eventHandler *eventHandler
	advanceFnc   func() // 推进分布式进程

	appliedIndex        atomic.Uint64 // 当前频道已经应用的日志索引
	appliedIndexSetLock sync.Mutex    // 存储已经应用的日志索引锁
	lastIndexSetLock    sync.Mutex    // 存储最后一条日志索引锁
	lastIndex           atomic.Uint64 // 当前频道最后一条日志索引

	sync struct {
		syncingLogIndex uint64        // 正在同步的日志索引
		syncStatus      syncStatus    // 是否正在同步
		startSyncTime   time.Time     // 开始同步时间
		syncTimeout     time.Duration // 同步超时时间
		resp            replica.Message
	}
	s       *Server
	shardId uint32
}

func newSlot(st *pb.Slot, appliedIdx uint64, advance func(), s *Server) *slot {
	shardNo := GetSlotShardNo(st.Id)
	shardId := GetShardId(shardNo)
	rc := replica.New(s.opts.NodeID, shardNo, replica.WithAppliedIndex(appliedIdx), replica.WithReplicas(st.Replicas), replica.WithReplicaMaxCount(int(st.ReplicaCount)), replica.WithStorage(newProxyReplicaStorage(shardNo, s.opts.ShardLogStorage)))
	if st.Leader == s.opts.NodeID {
		rc.BecomeLeader(st.Term)
	} else {
		rc.BecomeFollower(st.Term, st.Leader)
	}
	stobj := &slot{
		slotId:     st.Id,
		shardNo:    shardNo,
		shardId:    shardId,
		rc:         rc,
		Log:        wklog.NewWKLog(fmt.Sprintf("slot[%d:%d]", s.opts.NodeID, st.Id)),
		opts:       s.opts,
		doneC:      make(chan struct{}),
		advanceFnc: advance,
		s:          s,
	}
	stobj.lastActivity.Store(time.Now())
	stobj.sync.syncTimeout = 5 * time.Second
	stobj.eventHandler = newEventHandler(shardNo, shardId, stobj, stobj.Log, s, stobj.doneC)
	return stobj
}

func (s *slot) BecomeAny(term uint32, leaderId uint64) {
	s.Lock()
	defer s.Unlock()
	if leaderId == s.opts.NodeID {
		s.rc.BecomeLeader(term)
	} else {
		s.rc.BecomeFollower(term, leaderId)
	}
}

func (s *slot) proposeAndWaitCommits(ctx context.Context, logs []replica.Log, timeout time.Duration) ([]*messageItem, error) {

	if s.destroy {
		return nil, errors.New("slot destroy, can not propose")
	}
	s.lastActivity.Store(time.Now())
	return s.eventHandler.proposeAndWaitCommits(ctx, logs, timeout)
}

func (s *slot) handleReadyMessages(msgs []replica.Message) {
	s.lastActivity.Store(time.Now())
	s.eventHandler.handleReadyMessages(msgs)
}

func (s *slot) hasReady() bool {
	if s.destroy {
		return false
	}
	return s.rc.HasReady()
}

func (s *slot) ready() replica.Ready {
	if s.destroy {
		return replica.Ready{}
	}
	return s.rc.Ready()
}

func (s *slot) isLeader() bool {
	return s.rc.IsLeader()
}

func (s *slot) leaderId() uint64 {
	return s.rc.LeaderId()
}

func (s *slot) step(msg replica.Message) error {
	if s.destroy {
		return errors.New("slot destroy, can not step")
	}
	s.lastActivity.Store(time.Now())
	return s.rc.Step(msg)
}

func (s *slot) isDestroy() bool {
	return s.destroy
}

func (s *slot) makeDestroy() {
	s.destroy = true
	close(s.doneC)
}

func (s *slot) handleEvents() (bool, error) {
	if s.destroy {
		return false, nil
	}
	return s.eventHandler.handleEvents()
}

func (s *slot) getAndResetMsgSyncResp() (replica.Message, bool) {

	switch s.sync.syncStatus {
	case syncStatusSynced:
		s.sync.syncStatus = syncStatusNone
		return s.sync.resp, true
	default:
		return replica.EmptyMessage, false

	}
}

func (s *slot) setMsgSyncResp(msg replica.Message) {
	if s.sync.syncStatus != syncStatusSyncing {
		s.Warn("setMsgSyncResp: syncStatus != syncStatusSyncing", zap.Uint8("syncStatus", uint8(s.sync.syncStatus)))
		return
	}

	if msg.MsgType != replica.MsgSyncResp {
		s.Warn("setMsgSyncResp: msgType != MsgSyncResp", zap.String("msgType", msg.MsgType.String()))
		return
	}

	if msg.Index != s.sync.syncingLogIndex {
		s.Warn("setMsgSyncResp: msg.Index != c.sync.syncingLogIndex", zap.Uint64("msgIndex", msg.Index), zap.Uint64("syncingLogIndex", s.sync.syncingLogIndex))
		return
	}
	s.sync.resp = msg
	s.sync.syncStatus = syncStatusSynced
}

func (s *slot) sendMessage(msg replica.Message) {
	shardNo := GetSlotShardNo(s.slotId)
	protMsg, err := NewMessage(shardNo, msg, MsgSlotMsg)
	if err != nil {
		s.Error("new message error", zap.Error(err))
		return
	}
	if msg.MsgType != replica.MsgSync && msg.MsgType != replica.MsgSyncResp && msg.MsgType != replica.MsgPing && msg.MsgType != replica.MsgPong {
		s.Info("发送消息", zap.String("msgType", msg.MsgType.String()), zap.Uint64("to", msg.To), zap.Uint32("term", msg.Term), zap.Uint64("index", msg.Index))
	}
	if msg.MsgType == replica.MsgSync {
		switch s.sync.syncStatus {
		case syncStatusNone:
			s.sync.startSyncTime = time.Now()
			s.sync.syncingLogIndex = msg.Index
			s.sync.syncStatus = syncStatusSyncing
		case syncStatusSyncing:
			if msg.Index > s.sync.syncingLogIndex || time.Since(s.sync.startSyncTime) > s.sync.syncTimeout {
				s.sync.syncingLogIndex = msg.Index
				s.sync.startSyncTime = time.Now()
				s.Warn("sync timeout...", zap.Uint64("index", msg.Index))
			} else {
				s.Warn("syncing...", zap.Uint64("index", msg.Index))
				return
			}
		case syncStatusSynced:
			s.Warn("synced...", zap.Uint64("index", msg.Index))
			return
		}
	}
	// trace
	traceOutgoingMessage(trace.ClusterKindSlot, msg)

	// 发送消息
	err = s.opts.Transport.Send(msg.To, protMsg, nil)
	if err != nil {
		s.Warn("send msg error", zap.String("msgType", msg.MsgType.String()), zap.Uint64("to", msg.To), zap.Error(err))
	}
}

func (s *slot) appendLogs(msg replica.Message) error {
	shardNo := GetSlotShardNo(s.slotId)

	firstLog := msg.Logs[0]
	lastLog := msg.Logs[len(msg.Logs)-1]

	messageWaitItems := s.eventHandler.messageWait.waitItemsWithRange(firstLog.Index, lastLog.Index+1)
	for _, messageWaitItem := range messageWaitItems {
		_, span := trace.GlobalTrace.StartSpan(messageWaitItem.ctx, fmt.Sprintf("logsAppend[node %d]", s.opts.NodeID))
		defer span.End()
		span.SetInt("logCount", len(msg.Logs))
		span.SetUint64("firstLogIndex", firstLog.Index)
		span.SetUint64("lastLogIndex", lastLog.Index)
	}

	start := time.Now()

	s.Debug("append log", zap.Uint64("lastLogIndex", lastLog.Index))
	err := s.opts.ShardLogStorage.AppendLog(shardNo, msg.Logs)
	if err != nil {
		s.Panic("append log error", zap.Error(err))
	}
	s.Debug("append log done", zap.Uint64("lastLogIndex", lastLog.Index), zap.Duration("cost", time.Since(start)))
	return nil

}

func (s *slot) applyLogs(msg replica.Message) error {
	if msg.ApplyingIndex > msg.CommittedIndex {
		return fmt.Errorf("applyingIndex > committedIndex, applyingIndex: %d, committedIndex: %d", msg.ApplyingIndex, msg.CommittedIndex)
	}
	// messageWaitItems := s.eventHandler.messageWait.waitItemsWithRange(msg.ApplyingIndex+1, msg.CommittedIndex+1)
	// spans := make([]trace.Span, 0, len(messageWaitItems))
	// for _, messageWaitItem := range messageWaitItems {
	// 	_, span := trace.GlobalTrace.StartSpan(messageWaitItem.ctx, fmt.Sprintf("logsCommit[node %d]", s.opts.NodeID))
	// 	defer span.End()
	// 	span.SetUint64("appliedIndex", msg.AppliedIndex)
	// 	span.SetUint64("committedIndex", msg.CommittedIndex)
	// 	spans = append(spans, span)
	// }

	if s.opts.OnSlotApply != nil {
		logs, err := s.getLogs(msg.ApplyingIndex+1, msg.CommittedIndex+1, 0)
		if err != nil {
			s.Panic("get logs error", zap.Error(err))
		}
		if len(logs) == 0 {
			s.Panic("logs is empty", zap.Uint64("applyingIndex", msg.ApplyingIndex), zap.Uint64("committedIndex", msg.CommittedIndex))
		}
		err = s.opts.OnSlotApply(s.slotId, logs)
		if err != nil {
			s.Panic("on slot apply error", zap.Error(err))
		}
	}
	return nil

}

func (s *slot) getAndMergeLogs(msg replica.Message) ([]replica.Log, error) {

	unstableLogs := msg.Logs
	startIndex := msg.Index
	if len(unstableLogs) > 0 {
		startIndex = unstableLogs[len(unstableLogs)-1].Index + 1
	}

	messageWaitItems := s.eventHandler.messageWait.waitItemsWithStartSeq(startIndex)
	spans := make([]trace.Span, 0, len(messageWaitItems))
	for _, messageWaitItem := range messageWaitItems {
		_, span := trace.GlobalTrace.StartSpan(messageWaitItem.ctx, fmt.Sprintf("logsGet[node %d]", s.opts.NodeID))
		defer span.End()
		span.SetUint64("startIndex", startIndex)
		span.SetInt("unstableLogs", len(unstableLogs))
		spans = append(spans, span)

	}

	shardNo := GetSlotShardNo(s.slotId)

	lastIndex := s.lastIndex.Load()
	var err error
	if lastIndex == 0 {
		lastIndex, err = s.opts.ShardLogStorage.LastIndex(shardNo)
		if err != nil {
			s.Error("handleSyncGet: get last index error", zap.Error(err))
			return nil, err
		}
	}

	var resultLogs []replica.Log
	if startIndex <= lastIndex {
		logs, err := s.getLogs(startIndex, lastIndex+1, uint64(s.opts.LogSyncLimitSizeOfEach))
		if err != nil {
			s.Error("get logs error", zap.Error(err), zap.Uint64("startIndex", startIndex), zap.Uint64("lastIndex", lastIndex))
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

func (s *slot) getLogs(startLogIndex uint64, endLogIndex uint64, limitSize uint64) ([]replica.Log, error) {
	shardNo := GetSlotShardNo(s.slotId)
	logs, err := s.opts.ShardLogStorage.Logs(shardNo, startLogIndex, endLogIndex, limitSize)
	if err != nil {
		s.Error("get logs error", zap.Error(err))
		return nil, err
	}
	return logs, nil
}

func (s *slot) advance() {
	s.advanceFnc()
}

func (s *slot) newMsgApplyLogsRespMessage(index uint64) replica.Message {
	return s.rc.NewMsgApplyLogsRespMessage(index)
}

func (s *slot) newProposeMessageWithLogs(logs []replica.Log) replica.Message {
	return s.rc.NewProposeMessageWithLogs(logs)
}

func (s *slot) newMsgSyncGetResp(to uint64, startIndex uint64, logs []replica.Log) replica.Message {
	return s.rc.NewMsgSyncGetResp(to, startIndex, logs)
}

func (s *slot) newMsgStoreAppendResp(index uint64) replica.Message {
	return s.rc.NewMsgStoreAppendResp(index)
}

func (c *slot) lastLogIndexNoLock() uint64 {
	return c.rc.LastLogIndex()
}

func (s *slot) termNoLock() uint32 {
	return s.rc.Term()
}

func (s *slot) tick() {
	s.rc.Tick()
}

func (s *slot) setLastIndex(lastIndex uint64) error {
	s.lastIndex.Store(lastIndex)
	return s.s.defaultPool.Submit(s.setLastIndexForLatest)
}

func (s *slot) setLastIndexForLatest() {
	s.lastIndexSetLock.Lock()
	defer s.lastIndexSetLock.Unlock()
	lastIndex := s.lastIndex.Load()
	shardNo := GetSlotShardNo(s.slotId)
	err := s.opts.ShardLogStorage.SetLastIndex(shardNo, lastIndex)
	if err != nil {
		s.Error("set last index error", zap.Error(err))
	}
}

func (s *slot) setAppliedIndex(appliedIndex uint64) error {
	s.appliedIndex.Store(appliedIndex)
	return s.s.defaultPool.Submit(s.setAppliedIndexForLatest)
}

func (s *slot) setAppliedIndexForLatest() {
	s.appliedIndexSetLock.Lock()
	defer s.appliedIndexSetLock.Unlock()
	shardNo := GetSlotShardNo(s.slotId)
	appliedIndex := s.appliedIndex.Load()
	err := s.opts.ShardLogStorage.SetAppliedIndex(shardNo, appliedIndex)
	if err != nil {
		s.Error("set applied index error", zap.Error(err))
	}
}

func GetSlotShardNo(slotID uint32) string {
	return fmt.Sprintf("slot-%d", slotID)
}

func GetSlotId(shardNo string, lg wklog.Log) uint32 {
	var slotID uint64
	var err error
	strs := strings.Split(shardNo, "-")
	if len(strs) == 2 {
		slotID, err = strconv.ParseUint(strs[1], 10, 32)
		if err != nil {
			lg.Panic("parse slotID error", zap.Error(err))
		}
		return uint32(slotID)
	} else {
		lg.Panic("parse slotID error", zap.Error(err))
	}
	return 0
}
