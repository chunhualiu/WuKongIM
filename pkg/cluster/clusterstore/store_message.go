package clusterstore

import (
	"context"
	"strings"

	cluster "github.com/WuKongIM/WuKongIM/pkg/cluster/clusterserver"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/icluster"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/reactor"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/replica"
	"github.com/WuKongIM/WuKongIM/pkg/wkdb"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	wkproto "github.com/WuKongIM/WuKongIMGoProto"
	"go.uber.org/zap"
)

func (s *Store) AppendMessages(ctx context.Context, channelID string, channelType uint8, msgs []wkdb.Message) ([]icluster.ProposeResult, error) {

	if len(msgs) == 0 {
		return nil, nil
	}
	logs := make([]replica.Log, len(msgs))
	for i, msg := range msgs {
		data, err := msg.Marshal()
		if err != nil {
			return nil, err
		}
		logs[i] = replica.Log{
			Id:   uint64(msg.MessageID),
			Data: data,
		}
	}

	results, err := s.opts.Cluster.ProposeChannelMessages(ctx, channelID, channelType, logs)
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Store) LoadNextRangeMsgs(channelID string, channelType uint8, startMessageSeq, endMessageSeq uint64, limit int) ([]wkdb.Message, error) {
	return s.wdb.LoadNextRangeMsgs(channelID, channelType, startMessageSeq, endMessageSeq, limit)
}

func (s *Store) LoadMsg(channelID string, channelType uint8, seq uint64) (wkdb.Message, error) {
	return s.wdb.LoadMsg(channelID, channelType, seq)
}

func (s *Store) LoadLastMsgs(channelID string, channelType uint8, limit int) ([]wkdb.Message, error) {
	return s.wdb.LoadLastMsgs(channelID, channelType, limit)
}

func (s *Store) LoadLastMsgsWithEnd(channelID string, channelType uint8, end uint64, limit int) ([]wkdb.Message, error) {
	return s.wdb.LoadLastMsgsWithEnd(channelID, channelType, end, limit)
}

func (s *Store) LoadPrevRangeMsgs(channelID string, channelType uint8, start, end uint64, limit int) ([]wkdb.Message, error) {
	return s.wdb.LoadPrevRangeMsgs(channelID, channelType, start, end, limit)
}

func (s *Store) GetLastMsgSeq(channelID string, channelType uint8) (uint64, error) {
	seq, _, err := s.wdb.GetChannelLastMessageSeq(channelID, channelType)
	return seq, err
}

func (s *Store) GetMessagesOfNotifyQueue(count int) ([]wkdb.Message, error) {
	return s.wdb.GetMessagesOfNotifyQueue(count)
}

func (s *Store) AppendMessageOfNotifyQueue(messages []wkdb.Message) error {
	return s.wdb.AppendMessageOfNotifyQueue(messages)
}

func (s *Store) RemoveMessagesOfNotifyQueue(messageIDs []int64) error {
	return s.wdb.RemoveMessagesOfNotifyQueue(messageIDs)
}

func (s *Store) GetMessageShardLogStorage() *MessageShardLogStorage {
	return s.messageShardLogStorage
}

// // SaveStreamMeta 保存消息流元数据
// func (s *Store) SaveStreamMeta(meta *wkstore.StreamMeta) error {
// 	return nil
// }

// // StreamEnd 结束流
// func (s *Store) StreamEnd(channelID string, channelType uint8, streamNo string) error {
// 	return nil
// }

// func (s *Store) GetStreamMeta(channelID string, channelType uint8, streamNo string) (*wkstore.StreamMeta, error) {
// 	return nil, nil
// }

// func (s *Store) GetStreamItems(channelID string, channelType uint8, streamNo string) ([]*wkstore.StreamItem, error) {
// 	return nil, nil
// }

// // AppendStreamItem 追加消息流
// func (s *Store) AppendStreamItem(channelID string, channelType uint8, streamNo string, item *wkstore.StreamItem) (uint32, error) {
// 	return 0, nil
// }

func (s *Store) UpdateMessageOfUserCursorIfNeed(uid string, messageSeq uint64) error {
	return nil
}

func (s *Store) GetMessageOfUserCursor(uid string) (uint64, error) {
	return 0, nil
}

func (s *Store) AppendMessagesOfUser(subscriber string, messages []wkdb.Message) ([]icluster.ProposeResult, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	logs := make([]replica.Log, 0, len(messages))
	for _, message := range messages {
		data, err := EncodeCMDAppendMessagesOfUser(subscriber, []wkdb.Message{message})
		if err != nil {
			return nil, err
		}
		cmd := NewCMD(CMDAppendMessagesOfUser, data)
		cmdData, err := cmd.Marshal()
		if err != nil {
			return nil, err
		}
		logs = append(logs, replica.Log{
			Id:   uint64(message.MessageID),
			Data: cmdData,
		})

	}

	slotId := s.getChannelSlotId(subscriber)
	return s.opts.Cluster.ProposeToSlot(s.ctx, slotId, logs)
}

func (s *Store) SyncMessageOfUser(uid string, messageSeq uint64, limit uint32) ([]wkdb.Message, error) {
	return s.wdb.LoadNextRangeMsgs(uid, wkproto.ChannelTypePerson, messageSeq, 0, int(limit))
}

func (s *Store) DeleteChannelAndClearMessages(channelID string, channelType uint8) error {
	return nil
}

// 获取频道的槽id
func (s *Store) getChannelSlotId(channelId string) uint32 {
	return wkutil.GetSlotNum(int(s.opts.SlotCount), channelId)
}

type MessageShardLogStorage struct {
	db wkdb.DB
	wklog.Log
}

func NewMessageShardLogStorage(db wkdb.DB) *MessageShardLogStorage {
	return &MessageShardLogStorage{
		db:  db,
		Log: wklog.NewWKLog("MessageShardLogStorage"),
	}
}

func (m *MessageShardLogStorage) Open() error {

	return nil
}

func (m *MessageShardLogStorage) Close() error {
	return nil
}

func (s *MessageShardLogStorage) AppendLogBatch(reqs []reactor.AppendLogReq) error {
	dbReqs := make([]wkdb.AppendMessagesReq, 0, len(reqs))
	for _, req := range reqs {
		channelId, channelType := cluster.ChannelFromlKey(req.HandleKey)
		msgs := make([]wkdb.Message, len(req.Logs))
		for idx, log := range req.Logs {
			msg := wkdb.Message{}
			err := msg.Unmarshal(log.Data)
			if err != nil {
				return err
			}
			msg.MessageSeq = uint32(log.Index)
			msg.Term = uint64(log.Term)
			msgs[idx] = msg
		}
		dbReqs = append(dbReqs, wkdb.AppendMessagesReq{
			ChannelId:   channelId,
			ChannelType: channelType,
			Messages:    msgs,
		})
	}
	if len(dbReqs) == 0 {
		return nil
	}
	return s.db.AppendMessagesBatch(dbReqs)
}

// 获取日志
func (s *MessageShardLogStorage) Logs(shardNo string, startLogIndex, endLogIndex uint64, limitSize uint64) ([]replica.Log, error) {

	channelID, channelType := cluster.ChannelFromlKey(shardNo)
	var (
		messages []wkdb.Message
		err      error
	)

	lastIdx, err := s.LastIndex(shardNo)
	if err != nil {
		return nil, err
	}

	if endLogIndex == 0 || endLogIndex > lastIdx+1 {
		endLogIndex = lastIdx + 1
	}

	messages, err = s.db.LoadNextRangeMsgsForSize(channelID, channelType, startLogIndex, endLogIndex, limitSize)
	if err != nil {
		return nil, err
	}

	if len(messages) == 0 {
		return nil, nil
	}
	logs := make([]replica.Log, len(messages))
	for i, msg := range messages {
		data, err := msg.Marshal()
		if err != nil {
			return nil, err
		}
		logs[i] = replica.Log{
			Id:    uint64(msg.MessageID),
			Index: uint64(msg.MessageSeq),
			Term:  uint32(msg.Term),
			Data:  data,
		}
	}
	return logs, nil
}

func (s *MessageShardLogStorage) TruncateLogTo(shardNo string, index uint64) error {
	channelId, channelType := cluster.ChannelFromlKey(shardNo)
	return s.db.TruncateLogTo(channelId, channelType, index)
}

// 最后一条日志的索引
func (s *MessageShardLogStorage) LastIndex(shardNo string) (uint64, error) {
	channelId, channelType := cluster.ChannelFromlKey(shardNo)

	lastMsgSeq, _, err := s.db.GetChannelLastMessageSeq(channelId, channelType)
	if err != nil {
		return 0, err
	}
	return uint64(lastMsgSeq), nil

}

func (s *MessageShardLogStorage) LastIndexAndTerm(shardNo string) (uint64, uint32, error) {
	lastIndex, err := s.LastIndex(shardNo)
	if err != nil {
		return 0, 0, err
	}
	if lastIndex == 0 {
		return 0, 0, nil
	}
	channelId, channelType := cluster.ChannelFromlKey(shardNo)

	queryIndex := lastIndex
	var lastMsg wkdb.Message
	for queryIndex > 0 {
		lastMsg, err = s.db.LoadMsg(channelId, channelType, queryIndex)
		if err != nil {
			if err == wkdb.ErrNotFound {
				queryIndex--
				s.Warn("load last msg not found", zap.String("shardNo", shardNo), zap.Uint64("queryIndex", queryIndex))
				continue
			}
			s.Error("load last msg err", zap.Error(err), zap.String("shardNo", shardNo), zap.Uint64("lastIndex", lastIndex))
			return 0, 0, err
		}
		break

	}

	return uint64(lastMsg.MessageSeq), uint32(lastMsg.Term), nil
}

// func (s *MessageShardLogStorage) SetLastIndex(shardNo string, index uint64) error {
// 	channelId, channelType := cluster.ChannelFromlKey(shardNo)
// 	return s.db.SetChannelLastMessageSeq(channelId, channelType, index)
// }

// 获取第一条日志的索引
func (s *MessageShardLogStorage) FirstIndex(shardNo string) (uint64, error) {
	return 0, nil
}

// 设置成功被状态机应用的日志索引
func (s *MessageShardLogStorage) SetAppliedIndex(shardNo string, index uint64) error {
	channelId, channelType := cluster.ChannelFromlKey(shardNo)
	return s.db.UpdateChannelAppliedIndex(channelId, channelType, index)
}

func (s *MessageShardLogStorage) AppliedIndex(shardNo string) (uint64, error) {
	channelId, channelType := cluster.ChannelFromlKey(shardNo)
	return s.db.GetChannelAppliedIndex(channelId, channelType)
}

func (s *MessageShardLogStorage) SetLeaderTermStartIndex(shardNo string, term uint32, index uint64) error {
	return s.db.SetLeaderTermStartIndex(shardNo, term, index)
}

func (s *MessageShardLogStorage) LeaderLastTerm(shardNo string) (uint32, error) {
	return s.db.LeaderLastTerm(shardNo)
}

func (s *MessageShardLogStorage) LeaderTermStartIndex(shardNo string, term uint32) (uint64, error) {
	return s.db.LeaderTermStartIndex(shardNo, term)
}

func (s *MessageShardLogStorage) DeleteLeaderTermStartIndexGreaterThanTerm(shardNo string, term uint32) error {
	return s.db.DeleteLeaderTermStartIndexGreaterThanTerm(shardNo, term)
}

func (s *MessageShardLogStorage) LastIndexAndAppendTime(shardNo string) (uint64, uint64, error) {
	channelId, channelType := cluster.ChannelFromlKey(shardNo)
	lastMsgSeq, appendTime, err := s.db.GetChannelLastMessageSeq(channelId, channelType)
	if err != nil {
		return 0, 0, err
	}
	return uint64(lastMsgSeq), appendTime, nil
}

// 是用户自己的频道
func IsUserOwnChannel(channelId string, channelType uint8) bool {
	if channelType == wkproto.ChannelTypePerson {
		return !strings.Contains(channelId, "@")
	}
	return false
}
