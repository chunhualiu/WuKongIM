package server

import (
	"fmt"
	"strings"
	"sync"

	"github.com/WuKongIM/WuKongIM/pkg/wkdb"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	wkproto "github.com/WuKongIM/WuKongIMGoProto"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type channel struct {
	key         string
	channelId   string
	channelType uint8

	info wkdb.ChannelInfo // 频道基础信息

	msgQueue *channelMsgQueue // 消息队列

	actions []*ChannelAction

	// 缓存的订阅者 （不是全部的频道订阅者，是比较活跃的订阅者）
	cacheSubscribers map[string]struct{}

	// options
	stroageMaxSize uint64 // 每次存储的最大字节数量
	deliverMaxSize uint64 // 每次投递的最大字节数量

	forwardMaxSize uint64 // 每次转发消息的最大自己数量

	r   *channelReactor
	sub *channelReactorSub

	mu sync.Mutex

	status   channelStatus // 频道状态
	role     channelRole   // 频道角色
	leaderId uint64        // 频道领导节点

	receiverTagKey atomic.String // 当前频道的接受者的tag key

	wklog.Log

	stepFnc func(*ChannelAction) error

	payloadDecrypting  bool // 是否正在解密
	permissionChecking bool // 是否正在检查权限
	storaging          bool // 是否正在存储
	sendacking         bool // 是否正在发送回执
	delivering         bool // 是否正在投递
	forwarding         bool // 是否正在转发
}

func newChannel(sub *channelReactorSub, channelId string, channelType uint8) *channel {
	key := ChannelToKey(channelId, channelType)
	return &channel{
		key:              key,
		channelId:        channelId,
		channelType:      channelType,
		msgQueue:         newChannelMsgQueue(channelId),
		cacheSubscribers: make(map[string]struct{}),
		stroageMaxSize:   1024 * 1024 * 2,
		deliverMaxSize:   1024 * 1024 * 2,
		forwardMaxSize:   1024 * 1024 * 2,
		Log:              wklog.NewWKLog(fmt.Sprintf("channelHandler[%s]", key)),
		r:                sub.r,
		sub:              sub,
	}

}

func (c *channel) hasReady() bool {
	if !c.isInitialized() { // 是否初始化
		return c.status != channelStatusInitializing
	}

	if c.hasPayloadUnDecrypt() { // 有未解密的消息
		return true
	}

	if c.role == channelRoleLeader { // 领导者
		if c.hasPermissionUnCheck() { // 是否有未检查权限的消息
			return true
		}
		if c.hasUnstorage() { // 是否有未存储的消息
			return true
		}

		if c.hasUnDeliver() { // 是否有未投递的消息
			return true
		}
	} else if c.role == channelRoleProxy { // 代理者
		if c.hasUnforward() {
			return true
		}
	}
	return len(c.actions) > 0
}

func (c *channel) ready() ready {

	if !c.isInitialized() {
		if c.status == channelStatusInitializing {
			return ready{}
		}
		c.status = channelStatusInitializing
		c.exec(&ChannelAction{ActionType: ChannelActionInit})
	} else {

		if c.hasPayloadUnDecrypt() {
			c.payloadDecrypting = true
			msgs := c.msgQueue.sliceWithSize(c.msgQueue.payloadDecryptingIndex+1, c.msgQueue.lastIndex+1, 0)
			if len(msgs) > 0 {
				c.exec(&ChannelAction{ActionType: ChannelActionPayloadDecrypt, Messages: msgs})
			}
		}

		if c.role == channelRoleLeader {

			// 如果没有权限检查的则去检查权限
			if c.hasPermissionUnCheck() {
				c.permissionChecking = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.permissionCheckingIndex+1, c.msgQueue.payloadDecryptingIndex+1, 0)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionPermissionCheck, Messages: msgs})
				}
				// c.Info("permissionChecking...", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))
			}

			// 如果有未存储的消息，则继续存储
			if c.hasUnstorage() {
				c.storaging = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.storagingIndex+1, c.msgQueue.permissionCheckingIndex+1, c.stroageMaxSize)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionStorage, Messages: msgs})
				}
				// c.Info("storaging...", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))

			}

			// 如果有未发送回执的消息
			if c.hasSendack() {
				c.sendacking = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.sendackingIndex+1, c.msgQueue.storagingIndex+1, 0)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionSendack, Messages: msgs})
				}
			}

			// 投递消息
			if c.hasUnDeliver() {
				c.delivering = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.deliveringIndex+1, c.msgQueue.storagingIndex+1, c.deliverMaxSize)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionDeliver, Messages: msgs})
				}
				// c.Info("delivering...", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))

			}
		} else if c.role == channelRoleProxy {
			// 转发消息
			if c.hasUnforward() {
				c.forwarding = true
				msgs := c.msgQueue.sliceWithSize(c.msgQueue.forwardingIndex+1, c.msgQueue.payloadDecryptingIndex+1, c.deliverMaxSize)
				if len(msgs) > 0 {
					c.exec(&ChannelAction{ActionType: ChannelActionForward, LeaderId: c.leaderId, Messages: msgs})
				}
				// c.Info("forwarding...", zap.String("channelId", c.channelId), zap.Uint8("channelType", c.channelType))
			}
		}

	}

	actions := c.actions
	c.actions = nil
	return ready{
		actions: actions,
	}
}

func (c *channel) hasPayloadUnDecrypt() bool {
	if c.payloadDecrypting {
		return false
	}
	return c.msgQueue.payloadDecryptingIndex < c.msgQueue.lastIndex
}

// 有未权限检查的消息
func (c *channel) hasPermissionUnCheck() bool {
	if c.permissionChecking {
		return false
	}
	return c.msgQueue.permissionCheckingIndex < c.msgQueue.payloadDecryptingIndex
}

// 有未存储的消息
func (c *channel) hasUnstorage() bool {
	if c.storaging {
		return false
	}
	return c.msgQueue.storagingIndex < c.msgQueue.permissionCheckingIndex
}

// 有未发送回执的消息
func (c *channel) hasSendack() bool {
	if c.sendacking {
		return false
	}
	return c.msgQueue.sendackingIndex < c.msgQueue.storagingIndex
}

// 有未投递的消息
func (c *channel) hasUnDeliver() bool {
	if c.delivering {
		return false
	}
	return c.msgQueue.deliveringIndex < c.msgQueue.storagingIndex
}

// 有未转发的消息
func (c *channel) hasUnforward() bool {
	if c.forwarding { // 在转发中
		return false
	}
	return c.msgQueue.forwardingIndex < c.msgQueue.payloadDecryptingIndex
}

// 是否已初始化
func (c *channel) isInitialized() bool {

	return c.status == channelStatusInitialized
}

func (c *channel) tick() {

}

func (c *channel) proposeSend(fromUid string, fromDeviceId string, fromConnId int64, fromNodeId uint64, isEncrypt bool, sendPacket *wkproto.SendPacket) (int64, error) {

	messageId := c.r.messageIDGen.Generate().Int64() // 生成唯一消息ID
	message := ReactorChannelMessage{
		FromConnId:   fromConnId,
		FromUid:      fromUid,
		FromDeviceId: fromDeviceId,
		FromNodeId:   fromNodeId,
		SendPacket:   sendPacket,
		MessageId:    messageId,
		IsEncrypt:    isEncrypt,
	}

	err := c.sub.stepWait(c, &ChannelAction{
		ActionType: ChannelActionSend,
		Messages:   []ReactorChannelMessage{message},
	})
	if err != nil {
		return messageId, err
	}
	return messageId, nil
}

func (c *channel) becomeLeader() {
	c.resetIndex()
	c.leaderId = 0
	c.role = channelRoleLeader
	c.stepFnc = c.stepLeader
	c.Info("become logic leader")

}

func (c *channel) becomeProxy(leaderId uint64) {
	c.resetIndex()
	c.role = channelRoleProxy
	c.leaderId = leaderId
	c.stepFnc = c.stepProxy
	c.Info("become logic proxy", zap.Uint64("leaderId", c.leaderId))
}

func (c *channel) resetIndex() {
	c.msgQueue.forwardingIndex = 0
	c.msgQueue.deliveringIndex = 0
	c.msgQueue.storagingIndex = 0
	c.msgQueue.permissionCheckingIndex = 0

	c.permissionChecking = false
	c.storaging = false
	c.delivering = false
	c.forwarding = false

}

func (c *channel) advance() {
	c.sub.advance()
}

// 是否是缓存中的订阅者
func (c *channel) isCacheSubscriber(uid string) bool {
	_, ok := c.cacheSubscribers[uid]
	return ok
}

// 设置为缓存订阅者
func (c *channel) setCacheSubscriber(uid string) {
	c.cacheSubscribers[uid] = struct{}{}
}

type ready struct {
	actions []*ChannelAction
}

func (c *channel) makeReceiverTag() (*tag, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var subscribers []string
	var err error
	if c.channelType == wkproto.ChannelTypePerson {
		if c.r.s.opts.IsFakeChannel(c.channelId) { // fake个人频道

			if c.r.s.opts.IsCmdChannel(c.channelId) {
				orginChannelId := c.r.opts.CmdChannelConvertOrginalChannel(c.channelId)
				personSubscribers := strings.Split(orginChannelId, "@")
				for _, personSubscriber := range personSubscribers {
					if personSubscriber == c.r.opts.SystemUID { // 系统账号忽略
						continue
					}
					subscribers = append(subscribers, personSubscriber)
				}
			} else {
				subscribers = strings.Split(c.channelId, "@")
			}

		}
	} else {
		subscribers, err = c.r.s.store.GetSubscribers(c.channelId, c.channelType)
		if err != nil {
			return nil, err
		}
	}

	var nodeUserList = make([]*nodeUsers, 0, 20)
	for _, subscriber := range subscribers {
		leaderInfo, err := c.r.s.cluster.SlotLeaderOfChannel(subscriber, wkproto.ChannelTypePerson) // 获取频道的槽领导节点
		if err != nil {
			c.Error("获取频道所在节点失败！", zap.Error(err), zap.String("channelID", subscriber), zap.Uint8("channelType", wkproto.ChannelTypePerson))
			return nil, err
		}
		exist := false
		for _, nodeUser := range nodeUserList {
			if nodeUser.nodeId == leaderInfo.Id {
				nodeUser.uids = append(nodeUser.uids, subscriber)
				exist = true
				break
			}
		}
		if !exist {
			nodeUserList = append(nodeUserList, &nodeUsers{
				nodeId: leaderInfo.Id,
				uids:   []string{subscriber},
			})
		}
	}
	if c.receiverTagKey.Load() != "" {
		// 释放掉之前的tag
		c.r.s.tagManager.releaseReceiverTag(c.receiverTagKey.Load())
	}
	receiverTagKey := wkutil.GenUUID()
	newTag := c.r.s.tagManager.addOrUpdateReceiverTag(receiverTagKey, nodeUserList)
	newTag.ref.Inc() // tag引用计数加1
	c.receiverTagKey.Store(receiverTagKey)
	return newTag, nil
}
