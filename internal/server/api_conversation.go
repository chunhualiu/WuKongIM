package server

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/WuKongIM/WuKongIM/pkg/wkdb"
	"github.com/WuKongIM/WuKongIM/pkg/wkhttp"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	wkproto "github.com/WuKongIM/WuKongIMGoProto"
	"github.com/sendgrid/rest"
	"go.uber.org/zap"
)

// ConversationAPI ConversationAPI
type ConversationAPI struct {
	s *Server
	wklog.Log
}

// NewConversationAPI NewConversationAPI
func NewConversationAPI(s *Server) *ConversationAPI {
	return &ConversationAPI{
		s:   s,
		Log: wklog.NewWKLog("ConversationAPI"),
	}
}

// Route 路由
func (s *ConversationAPI) Route(r *wkhttp.WKHttp) {
	// r.GET("/conversations", s.conversationsList)                    // 获取会话列表 （此接口作废，使用/conversation/sync）
	r.POST("/conversations/clearUnread", s.clearConversationUnread) // 清空会话未读数量
	r.POST("/conversations/setUnread", s.setConversationUnread)     // 设置会话未读数量
	r.POST("/conversations/delete", s.deleteConversation)           // 删除会话
	r.POST("/conversation/sync", s.syncUserConversation)            // 同步会话
	r.POST("/conversation/syncMessages", s.syncRecentMessages)      // 同步会话最近消息
}

// // Get a list of recent conversations
// func (s *ConversationAPI) conversationsList(c *wkhttp.Context) {
// 	uid := c.Query("uid")
// 	if strings.TrimSpace(uid) == "" {
// 		c.ResponseError(errors.New("uid cannot be empty"))
// 		return
// 	}

// 	conversations := s.s.conversationManager.GetConversations(uid, 0, nil)
// 	conversationResps := make([]conversationResp, 0)
// 	if len(conversations) > 0 {
// 		for _, conversation := range conversations {
// 			fakeChannelID := conversation.ChannelID
// 			if conversation.ChannelType == wkproto.ChannelTypePerson {
// 				fakeChannelID = GetFakeChannelIDWith(uid, conversation.ChannelID)
// 			}
// 			// 获取到偏移位内的指定最大条数的最新消息
// 			message, err := s.s.store.LoadMsg(fakeChannelID, conversation.ChannelType, conversation.LastMsgSeq)
// 			if err != nil {
// 				s.Error("Failed to query recent news", zap.Error(err))
// 				c.ResponseError(err)
// 				return
// 			}
// 			messageResp := &MessageResp{}
// 			if message != nil {
// 				messageResp.from(message.(*Message), s.s.store)
// 			}
// 			conversationResps = append(conversationResps, conversationResp{
// 				ChannelID:   conversation.ChannelID,
// 				ChannelType: conversation.ChannelType,
// 				Unread:      conversation.UnreadCount,
// 				Timestamp:   conversation.Timestamp,
// 				LastMessage: messageResp,
// 			})
// 		}
// 	}
// 	c.JSON(http.StatusOK, conversationResps)
// }

// 清楚会话未读数量
func (s *ConversationAPI) clearConversationUnread(c *wkhttp.Context) {
	var req clearConversationUnreadReq
	bodyBytes, err := BindJSON(&req, c)
	if err != nil {
		s.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if err := req.Check(); err != nil {
		c.ResponseError(err)
		return
	}

	if s.s.opts.ClusterOn() {
		leaderInfo, err := s.s.cluster.SlotLeaderOfChannel(req.UID, wkproto.ChannelTypePerson) // 获取频道的领导节点
		if err != nil {
			s.Error("获取频道所在节点失败！", zap.Error(err), zap.String("channelID", req.UID), zap.Uint8("channelType", wkproto.ChannelTypePerson))
			c.ResponseError(errors.New("获取频道所在节点失败！"))
			return
		}
		leaderIsSelf := leaderInfo.Id == s.s.opts.Cluster.NodeId
		if !leaderIsSelf {
			s.Debug("转发请求：", zap.String("url", fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path)))
			c.ForwardWithBody(fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path), bodyBytes)
			return
		}
	}

	fakeChannelId := req.ChannelID
	if req.ChannelType == wkproto.ChannelTypePerson {
		fakeChannelId = GetFakeChannelIDWith(req.UID, req.ChannelID)

	}

	conversation, err := s.s.store.GetConversation(req.UID, fakeChannelId, req.ChannelType)
	if err != nil && err != wkdb.ErrNotFound {
		s.Error("Failed to query conversation", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if wkdb.IsEmptyConversation(conversation) {
		conversation = wkdb.Conversation{
			Uid:         req.UID,
			ChannelId:   fakeChannelId,
			ChannelType: req.ChannelType,
		}
	}

	// 获取此频道最新的消息
	msgSeq, err := s.s.store.GetLastMsgSeq(fakeChannelId, req.ChannelType)
	if err != nil {
		s.Error("Failed to query last message", zap.Error(err))
		c.ResponseError(err)
		return
	}

	if conversation.ReadedToMsgSeq < msgSeq {
		conversation.ReadedToMsgSeq = msgSeq

	}

	err = s.s.store.AddOrUpdateConversations(req.UID, []wkdb.Conversation{conversation})
	if err != nil {
		s.Error("Failed to add conversation", zap.Error(err))
		c.ResponseError(err)
		return
	}

	s.s.conversationManager.DeleteUserConversationFromCache(req.UID, fakeChannelId, req.ChannelType)

	c.ResponseOK()
}

func (s *ConversationAPI) setConversationUnread(c *wkhttp.Context) {
	var req struct {
		UID         string `json:"uid"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
		Unread      int    `json:"unread"`
		MessageSeq  uint32 `json:"message_seq"` // messageSeq 只有超大群才会传 因为超大群最近会话服务器不会维护，需要客户端传递messageSeq进行主动维护
	}
	bodyBytes, err := BindJSON(&req, c)
	if err != nil {
		s.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(err)
		return
	}

	if req.UID == "" {
		c.ResponseError(errors.New("UID cannot be empty"))
		return
	}
	if req.ChannelID == "" || req.ChannelType == 0 {
		c.ResponseError(errors.New("channel_id or channel_type cannot be empty"))
		return
	}

	if s.s.opts.ClusterOn() {
		leaderInfo, err := s.s.cluster.SlotLeaderOfChannel(req.UID, wkproto.ChannelTypePerson) // 获取频道的领导节点
		if err != nil {
			s.Error("获取频道所在节点失败！", zap.Error(err), zap.String("channelID", req.UID), zap.Uint8("channelType", wkproto.ChannelTypePerson))
			c.ResponseError(errors.New("获取频道所在节点失败！"))
			return
		}
		leaderIsSelf := leaderInfo.Id == s.s.opts.Cluster.NodeId
		if !leaderIsSelf {
			s.Debug("转发请求：", zap.String("url", fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path)))
			c.ForwardWithBody(fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path), bodyBytes)
			return
		}
	}

	fakeChannelId := req.ChannelID
	if req.ChannelType == wkproto.ChannelTypePerson {
		fakeChannelId = GetFakeChannelIDWith(req.UID, req.ChannelID)

	}
	// 获取此频道最新的消息
	msgSeq, err := s.s.store.GetLastMsgSeq(fakeChannelId, req.ChannelType)
	if err != nil {
		s.Error("Failed to query last message", zap.Error(err))
		c.ResponseError(err)
		return
	}

	conversation, err := s.s.store.GetConversation(req.UID, fakeChannelId, req.ChannelType)
	if err != nil && err != wkdb.ErrNotFound {
		s.Error("Failed to query conversation", zap.Error(err))
		c.ResponseError(err)
		return
	}

	if wkdb.IsEmptyConversation(conversation) {
		conversation = wkdb.Conversation{
			Uid:         req.UID,
			ChannelId:   fakeChannelId,
			ChannelType: req.ChannelType,
		}

	}

	var unread uint32 = 0
	var readedMsgSeq uint64 = msgSeq

	if uint64(req.Unread) > msgSeq {
		unread = 1
		readedMsgSeq = msgSeq - 1
	} else if req.Unread > 0 {
		unread = uint32(req.Unread)
		readedMsgSeq = msgSeq - uint64(req.Unread)
	}

	conversation.ReadedToMsgSeq = readedMsgSeq
	conversation.UnreadCount = unread

	err = s.s.store.AddOrUpdateConversations(req.UID, []wkdb.Conversation{conversation})
	if err != nil {
		s.Error("Failed to add conversation", zap.Error(err))
		c.ResponseError(err)
		return
	}

	s.s.conversationManager.DeleteUserConversationFromCache(req.UID, fakeChannelId, req.ChannelType)

	c.ResponseOK()
}

func (s *ConversationAPI) deleteConversation(c *wkhttp.Context) {
	var req deleteChannelReq
	bodyBytes, err := BindJSON(&req, c)
	if err != nil {
		s.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if err := req.Check(); err != nil {
		c.ResponseError(err)
		return
	}

	if s.s.opts.ClusterOn() {
		leaderInfo, err := s.s.cluster.SlotLeaderOfChannel(req.UID, wkproto.ChannelTypePerson) // 获取频道的领导节点
		if err != nil {
			s.Error("获取频道所在节点失败！", zap.Error(err), zap.String("channelID", req.UID), zap.Uint8("channelType", wkproto.ChannelTypePerson))
			c.ResponseError(errors.New("获取频道所在节点失败！"))
			return
		}
		leaderIsSelf := leaderInfo.Id == s.s.opts.Cluster.NodeId

		if !leaderIsSelf {
			s.Debug("转发请求：", zap.String("url", fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path)))
			c.ForwardWithBody(fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path), bodyBytes)
			return
		}
	}
	fakeChannelId := req.ChannelID
	if req.ChannelType == wkproto.ChannelTypePerson {
		fakeChannelId = GetFakeChannelIDWith(req.UID, req.ChannelID)

	}

	err = s.s.store.DeleteConversation(req.UID, fakeChannelId, req.ChannelType)
	if err != nil {
		s.Error("删除会话！", zap.Error(err))
		c.ResponseError(err)
		return
	}

	s.s.conversationManager.DeleteUserConversationFromCache(req.UID, fakeChannelId, req.ChannelType)

	c.ResponseOK()
}

func (s *ConversationAPI) syncUserConversation(c *wkhttp.Context) {
	var req struct {
		UID         string             `json:"uid"`
		Version     int64              `json:"version"`       // 当前客户端的会话最大版本号(客户端最新会话的时间戳)
		LastMsgSeqs string             `json:"last_msg_seqs"` // 客户端所有会话的最后一条消息序列号 格式： channelID:channelType:last_msg_seq|channelID:channelType:last_msg_seq
		MsgCount    int64              `json:"msg_count"`     // 每个会话消息数量
		Larges      []*wkproto.Channel `json:"larges"`        // 超大频道集合
	}
	bodyBytes, err := BindJSON(&req, c)
	if err != nil {
		s.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(err)
		return
	}

	if s.s.opts.ClusterOn() {
		leaderInfo, err := s.s.cluster.SlotLeaderOfChannel(req.UID, wkproto.ChannelTypePerson) // 获取频道的领导节点
		if err != nil {
			s.Error("获取频道所在节点失败！!", zap.Error(err), zap.String("channelID", req.UID), zap.Uint8("channelType", wkproto.ChannelTypePerson))
			c.ResponseError(errors.New("获取频道所在节点失败！"))
			return
		}
		leaderIsSelf := leaderInfo.Id == s.s.opts.Cluster.NodeId

		if !leaderIsSelf {
			s.Debug("转发请求：", zap.String("url", fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path)))
			c.ForwardWithBody(fmt.Sprintf("%s%s", leaderInfo.ApiServerAddr, c.Request.URL.Path), bodyBytes)
			return
		}
	}

	var (
		channelLastMsgMap        = s.getChannelLastMsgSeqMap(req.LastMsgSeqs) // 获取频道对应的最后一条消息的messageSeq
		channelRecentMessageReqs = make([]*channelRecentMessageReq, 0, len(channelLastMsgMap))
	)

	// 获取用户最近会话基础数据

	var (
		resps []*syncUserConversationResp
	)

	// ==================== 获取用户活跃的最近会话 ====================
	conversations, err := s.s.store.GetLastConversations(req.UID, wkdb.ConversationTypeChat, uint64(req.Version), s.s.opts.Conversation.UserMaxCount)
	if err != nil {
		s.Error("获取conversation失败！", zap.Error(err), zap.String("uid", req.UID))
		c.ResponseError(errors.New("获取conversation失败！"))
		return
	}

	fmt.Println("conversations--->", len(conversations), conversations)

	// 获取用户缓存的最近会话
	cacheConversations := s.s.conversationManager.GetUserConversationFromCache(req.UID, wkdb.ConversationTypeChat)

	fmt.Println("cacheConversations---->", len(cacheConversations), cacheConversations)
	cacheConversationMap := map[string]uint64{}
	for _, cacheConversation := range cacheConversations {
		if cacheConversation.ReadedToMsgSeq > 0 {
			cacheConversationMap[fmt.Sprintf("%s-%d", cacheConversation.ChannelId, cacheConversation.ChannelType)] = cacheConversation.ReadedToMsgSeq
		}
	}
	for _, cacheConversation := range cacheConversations {
		exist := false
		for _, conversation := range conversations {
			if cacheConversation.ChannelId == conversation.ChannelId && cacheConversation.ChannelType == conversation.ChannelType {
				conversation.ReadedToMsgSeq = cacheConversation.ReadedToMsgSeq
				exist = true
				break
			}
		}
		if !exist {
			conversations = append(conversations, cacheConversation)
		}
	}

	for _, conversation := range conversations {
		realChannelId := conversation.ChannelId
		if conversation.ChannelType == wkproto.ChannelTypePerson {
			from, to := GetFromUIDAndToUIDWith(conversation.ChannelId)
			if req.UID == from {
				realChannelId = to
			} else {
				realChannelId = from
			}
		}
		msgSeq := channelLastMsgMap[fmt.Sprintf("%s-%d", realChannelId, conversation.ChannelType)]
		cacheMsgSeq := cacheConversationMap[fmt.Sprintf("%s-%d", conversation.ChannelId, conversation.ChannelType)]
		if cacheMsgSeq > msgSeq {
			msgSeq = cacheMsgSeq
		}

		if conversation.ReadedToMsgSeq > msgSeq {
			msgSeq = conversation.ReadedToMsgSeq
		}
		channelRecentMessageReqs = append(channelRecentMessageReqs, &channelRecentMessageReq{
			ChannelId:   realChannelId,
			ChannelType: conversation.ChannelType,
			LastMsgSeq:  msgSeq,
		})
		conversation.ReadedToMsgSeq = msgSeq
		syncUserConversationR := newSyncUserConversationResp(conversation)
		resps = append(resps, syncUserConversationR)
	}

	// ==================== 获取最近会话的最近的消息列表 ====================
	if req.MsgCount > 0 {
		var channelRecentMessages []*channelRecentMessage
		if s.s.opts.ClusterOn() {
			channelRecentMessages, err = s.s.getRecentMessagesForCluster(req.UID, int(req.MsgCount), channelRecentMessageReqs, true)
			if err != nil {
				s.Error("获取最近消息失败！", zap.Error(err), zap.String("uid", req.UID))
				c.ResponseError(errors.New("获取最近消息失败！"))
				return
			}
		} else {
			channelRecentMessages, err = s.s.getRecentMessages(req.UID, int(req.MsgCount), channelRecentMessageReqs, true)
			if err != nil {
				s.Error("获取最近消息失败！", zap.Error(err), zap.String("uid", req.UID))
				c.ResponseError(errors.New("获取最近消息失败！"))
				return
			}
		}

		if len(channelRecentMessages) > 0 {
			for i := 0; i < len(resps); i++ {
				resp := resps[i]
				for _, channelRecentMessage := range channelRecentMessages {
					if resp.ChannelID == channelRecentMessage.ChannelId && resp.ChannelType == channelRecentMessage.ChannelType {
						if len(channelRecentMessage.Messages) > 0 {
							lastMsg := channelRecentMessage.Messages[len(channelRecentMessage.Messages)-1]
							resp.LastMsgSeq = uint32(lastMsg.MessageSeq)
							resp.LastClientMsgNo = lastMsg.ClientMsgNo
							resp.Timestamp = int64(lastMsg.Timestamp)
							if lastMsg.MessageSeq > uint64(resp.ReadedToMsgSeq) {
								resp.Unread = int(lastMsg.MessageSeq - uint64(resp.ReadedToMsgSeq))
							}

						}
						resp.Recents = channelRecentMessage.Messages
					}
				}
			}
		}
	}
	c.JSON(http.StatusOK, resps)
}

func (s *ConversationAPI) getChannelLastMsgSeqMap(lastMsgSeqs string) map[string]uint64 {
	channelLastMsgSeqStrList := strings.Split(lastMsgSeqs, "|")
	channelLastMsgMap := map[string]uint64{} // 频道对应的messageSeq
	for _, channelLastMsgSeqStr := range channelLastMsgSeqStrList {
		channelLastMsgSeqs := strings.Split(channelLastMsgSeqStr, ":")
		if len(channelLastMsgSeqs) != 3 {
			continue
		}
		channelID := channelLastMsgSeqs[0]
		channelTypeI, _ := strconv.Atoi(channelLastMsgSeqs[1])
		lastMsgSeq, _ := strconv.ParseUint(channelLastMsgSeqs[2], 10, 64)
		channelLastMsgMap[fmt.Sprintf("%s-%d", channelID, channelTypeI)] = lastMsgSeq
	}
	return channelLastMsgMap
}

func (s *ConversationAPI) syncRecentMessages(c *wkhttp.Context) {
	var req struct {
		UID      string                     `json:"uid"`
		Channels []*channelRecentMessageReq `json:"channels"`
		MsgCount int                        `json:"msg_count"`
	}
	if err := c.BindJSON(&req); err != nil {
		s.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	msgCount := req.MsgCount
	if msgCount <= 0 {
		msgCount = 15
	}
	channelRecentMessages, err := s.s.getRecentMessages(req.UID, msgCount, req.Channels, true)
	if err != nil {
		s.Error("获取最近消息失败！", zap.Error(err))
		c.ResponseError(errors.New("获取最近消息失败！"))
		return
	}
	c.JSON(http.StatusOK, channelRecentMessages)
}

func (s *Server) getRecentMessagesForCluster(uid string, msgCount int, channels []*channelRecentMessageReq, orderByLast bool) ([]*channelRecentMessage, error) {
	if len(channels) == 0 {
		return nil, nil
	}
	channelRecentMessages := make([]*channelRecentMessage, 0)
	var (
		err error
	)
	localPeerChannelRecentMessageReqs := make([]*channelRecentMessageReq, 0)
	peerChannelRecentMessageReqsMap := make(map[uint64][]*channelRecentMessageReq)
	for _, channelRecentMsgReq := range channels {
		fakeChannelId := channelRecentMsgReq.ChannelId
		if channelRecentMsgReq.ChannelType == wkproto.ChannelTypePerson {
			fakeChannelId = GetFakeChannelIDWith(uid, channelRecentMsgReq.ChannelId)
		}
		leaderInfo, err := s.cluster.LeaderOfChannelForRead(fakeChannelId, channelRecentMsgReq.ChannelType) // 获取频道的领导节点
		if err != nil {
			s.Error("获取频道所在节点失败！", zap.Error(err), zap.String("channelID", fakeChannelId), zap.Uint8("channelType", channelRecentMsgReq.ChannelType))
			return nil, err
		}
		leaderIsSelf := leaderInfo.Id == s.opts.Cluster.NodeId
		if leaderIsSelf {
			localPeerChannelRecentMessageReqs = append(localPeerChannelRecentMessageReqs, channelRecentMsgReq)
		} else {
			peerChannelRecentMessageReqs := peerChannelRecentMessageReqsMap[leaderInfo.Id]
			if peerChannelRecentMessageReqs == nil {
				peerChannelRecentMessageReqs = make([]*channelRecentMessageReq, 0)
			}
			peerChannelRecentMessageReqs = append(peerChannelRecentMessageReqs, channelRecentMsgReq)
			peerChannelRecentMessageReqsMap[leaderInfo.Id] = peerChannelRecentMessageReqs
		}

	}

	// 请求远程的消息列表
	if len(peerChannelRecentMessageReqsMap) > 0 {
		var reqErr error
		wg := &sync.WaitGroup{}
		for nodeId, peerChannelRecentMessageReqs := range peerChannelRecentMessageReqsMap {
			wg.Add(1)
			go func(pID uint64, reqs []*channelRecentMessageReq, uidStr string, msgCt int) {
				results, err := s.requestSyncMessage(pID, reqs, uidStr, msgCt)
				if err != nil {
					s.Error("请求同步消息失败！", zap.Error(err))
					reqErr = err
				} else {
					channelRecentMessages = append(channelRecentMessages, results...)
				}
				wg.Done()
			}(nodeId, peerChannelRecentMessageReqs, uid, msgCount)
		}
		wg.Wait()
		if reqErr != nil {
			s.Error("请求同步消息失败！!", zap.Error(err))
			return nil, reqErr
		}
	}
	// 请求本地的最近消息列表
	if len(localPeerChannelRecentMessageReqs) > 0 {
		results, err := s.getRecentMessages(uid, msgCount, localPeerChannelRecentMessageReqs, orderByLast)
		if err != nil {
			return nil, err
		}
		channelRecentMessages = append(channelRecentMessages, results...)
	}
	return channelRecentMessages, nil
}

func (s *Server) requestSyncMessage(nodeID uint64, reqs []*channelRecentMessageReq, uid string, msgCount int) ([]*channelRecentMessage, error) {

	nodeInfo, err := s.cluster.NodeInfoById(nodeID) // 获取频道的领导节点
	if err != nil {
		s.Error("通过节点ID获取节点失败！", zap.Uint64("nodeID", nodeID))
		return nil, err
	}
	reqURL := fmt.Sprintf("%s/%s", nodeInfo.ApiServerAddr, "conversation/syncMessages")
	request := rest.Request{
		Method:  rest.Method("POST"),
		BaseURL: reqURL,
		Body: []byte(wkutil.ToJSON(map[string]interface{}{
			"uid":       uid,
			"msg_count": msgCount,
			"channels":  reqs,
		})),
	}
	s.Debug("同步会话消息!", zap.String("apiURL", reqURL), zap.String("uid", uid), zap.Any("channels", reqs))
	resp, err := rest.API(request)
	if err != nil {
		return nil, err
	}
	if err := handlerIMError(resp); err != nil {
		return nil, err
	}
	var results []*channelRecentMessage
	if err := wkutil.ReadJSONByByte([]byte(resp.Body), &results); err != nil {
		return nil, err
	}
	return results, nil
}

// getRecentMessages 获取频道最近消息
// orderByLast: true 按照最新的消息排序 false 按照最旧的消息排序
func (s *Server) getRecentMessages(uid string, msgCount int, channels []*channelRecentMessageReq, orderByLast bool) ([]*channelRecentMessage, error) {
	channelRecentMessages := make([]*channelRecentMessage, 0)
	if len(channels) > 0 {
		var (
			recentMessages []wkdb.Message
			err            error
		)
		for _, channel := range channels {
			fakeChannelID := channel.ChannelId
			if channel.ChannelType == wkproto.ChannelTypePerson {
				fakeChannelID = GetFakeChannelIDWith(uid, channel.ChannelId)
			}
			msgSeq := channel.LastMsgSeq
			if msgSeq > 0 {
				msgSeq = msgSeq - 1
			}
			messageResps := MessageRespSlice{}
			if orderByLast {
				recentMessages, err = s.store.LoadLastMsgsWithEnd(fakeChannelID, channel.ChannelType, msgSeq, msgCount)
				if err != nil {
					s.Error("查询最近消息失败！", zap.Error(err), zap.String("uid", uid), zap.String("fakeChannelID", fakeChannelID), zap.Uint8("channelType", channel.ChannelType), zap.Uint64("LastMsgSeq", channel.LastMsgSeq))
					return nil, err
				}
				if len(recentMessages) > 0 {
					for _, recentMessage := range recentMessages {
						messageResp := &MessageResp{}
						messageResp.from(recentMessage)
						messageResps = append(messageResps, messageResp)
					}
				}
				sort.Sort(sort.Reverse(messageResps))
			} else {
				recentMessages, err = s.store.LoadNextRangeMsgs(uid, channel.ChannelType, msgSeq, 0, msgCount)
				if err != nil {
					s.Error("查询最近消息失败！", zap.Error(err), zap.String("uid", uid), zap.String("fakeChannelID", fakeChannelID), zap.Uint8("channelType", channel.ChannelType), zap.Uint64("LastMsgSeq", channel.LastMsgSeq))
					return nil, err
				}
				if len(recentMessages) > 0 {
					for _, recentMessage := range recentMessages {
						messageResp := &MessageResp{}
						messageResp.from(recentMessage)
						messageResps = append(messageResps, messageResp)
					}
				}
			}

			channelRecentMessages = append(channelRecentMessages, &channelRecentMessage{
				ChannelId:   channel.ChannelId,
				ChannelType: channel.ChannelType,
				Messages:    messageResps,
			})
		}
	}
	return channelRecentMessages, nil
}
