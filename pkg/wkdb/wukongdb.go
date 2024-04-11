package wkdb

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"path/filepath"

	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	"github.com/bwmarrin/snowflake"
	"github.com/cockroachdb/pebble"
	"go.uber.org/zap"
)

var _ DB = (*wukongDB)(nil)

type wukongDB struct {
	dbs      []*pebble.DB
	shardNum uint32 // 分区数量，这个一但设置就不能修改
	opts     *Options
	wo       *pebble.WriteOptions
	endian   binary.ByteOrder
	wklog.Log
	prmaryKeyGen *snowflake.Node // 消息ID生成器
}

func NewWukongDB(opts *Options) *wukongDB {
	prmaryKeyGen, err := snowflake.NewNode(int64(opts.NodeId))
	if err != nil {
		panic(err)
	}
	return &wukongDB{
		opts:         opts,
		shardNum:     16,
		prmaryKeyGen: prmaryKeyGen,
		endian:       binary.BigEndian,
		wo:           &pebble.WriteOptions{},
		Log:          wklog.NewWKLog("wukongDB"),
	}
}

func (wk *wukongDB) Open() error {

	for i := 0; i < int(wk.shardNum); i++ {
		db, err := pebble.Open(filepath.Join(wk.opts.DataDir, "wukongimdb", fmt.Sprintf("shard%03d", i)), &pebble.Options{
			FormatMajorVersion: pebble.FormatNewest,
		})
		if err != nil {
			return err
		}
		wk.dbs = append(wk.dbs, db)
	}
	return nil
}

func (wk *wukongDB) Close() error {
	for _, db := range wk.dbs {
		if err := db.Close(); err != nil {
			wk.Error("close db error", zap.Error(err))
		}
	}
	return nil
}

func (wk *wukongDB) shardDB(v string) *pebble.DB {
	shardId := crc32.ChecksumIEEE([]byte(v)) % wk.shardNum
	return wk.dbs[shardId]
}

func (wk *wukongDB) defaultShardDB() *pebble.DB {
	return wk.dbs[0]
}

func (wk *wukongDB) channelSlotId(channelId string, channelType uint8) uint32 {
	return wkutil.GetSlotNum(int(wk.opts.SlotCount), channelId)
}
