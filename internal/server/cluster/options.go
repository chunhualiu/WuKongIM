package cluster

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	sm "github.com/lni/dragonboat/v4/statemachine"
)

type Options struct {
	PeerID          uint64
	Addr            string // 监听地址 例如： ip:port
	GRPCAddr        string // grpc监听地址 例如： ip:port
	GRPCServerAddr  string // 服务地址 例如： ip:port 节点之间可以互相访问的地址，如果为空则使用GRPCAddr
	APIServerAddr   string // 服务地址 例如： http://ip:port 节点之间可以互相访问的地址
	ServerAddr      string // 服务地址 例如： ip:port 节点之间可以互相访问的地址，如果为空则使用Addr
	SlotCount       int    // 槽位数量
	ReplicaCount    int    // 副本数量(包含主节点)
	DataDir         string
	Join            string // 新加入的集群节点的地址 例如： peerID@ip:port
	Peers           []Peer
	LeaderChange    func(leaderID uint64)
	GRPCEvent       CMDEvent
	GRPCSendTimeout time.Duration
	OnSlotApply     func(slotID uint32, entries []sm.Entry) ([]sm.Entry, error)

	grpcPortOffset int
}

func NewOptions() *Options {
	return &Options{
		SlotCount:       256,
		ReplicaCount:    3,
		Addr:            "0.0.0.0:11000",
		DataDir:         "./raftdata",
		GRPCSendTimeout: time.Second * 5,
		grpcPortOffset:  1234,
	}
}

func (o *Options) load() error {
	if strings.HasPrefix(o.Addr, "tcp://") {
		o.Addr = strings.ReplaceAll(o.Addr, "tcp://", "")
	}
	if o.GRPCAddr == "" {
		o.GRPCAddr = GetGRPCAddr(o.Addr, o.grpcPortOffset)
	}
	fmt.Println("o.GRPCAddr--->", o.GRPCAddr)
	if o.ServerAddr == "" {
		o.ServerAddr = o.Addr
	}
	if o.GRPCServerAddr == "" {
		o.GRPCServerAddr = o.GRPCAddr
	}

	return nil
}

func GetGRPCAddr(addr string, grpcPortOffset int) string {
	if strings.TrimSpace(addr) == "" {
		return ""
	}
	grpcAddrPort := getPort(addr) + uint32(grpcPortOffset)
	host := getHost(addr)
	return host + ":" + strconv.FormatUint(uint64(grpcAddrPort), 10)
}

func getPort(addr string) uint32 {
	strs := strings.Split(addr, ":")
	if len(strs) != 2 {
		return 0
	}
	port, _ := strconv.ParseUint(strs[1], 10, 64)
	return uint32(port)
}

func getHost(addr string) string {
	strs := strings.Split(addr, ":")
	if len(strs) != 2 {
		return ""
	}
	return strs[0]
}

func WithAddr(addr string) Option {
	return func(opts *Options) {
		opts.Addr = addr
	}
}

func WithSlotCount(slotCount int) Option {
	return func(opts *Options) {
		opts.SlotCount = slotCount
	}
}

func WithPeers(peers []Peer) Option {
	return func(opts *Options) {
		opts.Peers = peers
	}
}

func WithJoin(join string) Option {
	return func(opts *Options) {
		opts.Join = join
	}
}

type Option func(opts *Options)

type ClusterManagerOptions struct {
	PeerID         uint64                        // 节点ID
	SlotCount      int                           // 槽位数量
	ReplicaCount   int                           // 副本数量
	ConfigPath     string                        // 集群配置文件路径
	GetSlotState   func(slotID uint32) SlotState // 获取槽位状态
	ServerAddr     string                        // 服务地址 例如： ip:port 节点之间可以互相访问的地址
	GRPCServerAddr string                        // GRPC服务地址 例如： ip:port 节点之间可以互相访问的地址
	APIServerAddr  string                        // 服务地址 例如： http://ip:port 节点之间可以互相访问的地址
	Join           string                        // 加入集群的节点地址 例如： peerID@ip:port
}

func NewClusterManagerOptions() *ClusterManagerOptions {
	return &ClusterManagerOptions{
		SlotCount:    128,
		ReplicaCount: 3,
	}
}

type SlotOptions struct {
	PeerIDs         []uint64
	MaxReplicaCount uint32
	DataDir         string
	// *RaftOptions
}

func NewSlotOptions() *SlotOptions {
	return &SlotOptions{
		MaxReplicaCount: 3,
	}
}
