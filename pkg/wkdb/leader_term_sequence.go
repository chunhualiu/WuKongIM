package wkdb

import (
	"math"

	"github.com/WuKongIM/WuKongIM/pkg/wkdb/key"
	"github.com/cockroachdb/pebble"
)

func (wk *wukongDB) SetLeaderTermStartIndex(shardNo string, term uint32, index uint64) error {
	indexBytes := make([]byte, 8)
	wk.endian.PutUint64(indexBytes, index)
	return wk.shardDB(shardNo).Set(key.NewLeaderTermSequenceTermKey(shardNo, term), indexBytes, wk.sync)
}

func (wk *wukongDB) LeaderLastTerm(shardNo string) (uint32, error) {

	iter := wk.shardDB(shardNo).NewIter(&pebble.IterOptions{
		LowerBound: key.NewLeaderTermSequenceTermKey(shardNo, 0),
		UpperBound: key.NewLeaderTermSequenceTermKey(shardNo, math.MaxUint32),
	})
	defer iter.Close()

	if iter.Last() && iter.Valid() && iter.Prev() {
		term, err := key.ParseLeaderTermSequenceTermKey(iter.Key())
		if err != nil {
			return 0, err
		}
		return term, nil
	}
	return 0, nil
}

func (wk *wukongDB) LeaderTermStartIndex(shardNo string, term uint32) (uint64, error) {
	indexBytes, closer, err := wk.shardDB(shardNo).Get(key.NewLeaderTermSequenceTermKey(shardNo, term))
	if err != nil {
		if err == pebble.ErrNotFound {
			return 0, nil
		}
		return 0, err
	}
	defer closer.Close()
	return wk.endian.Uint64(indexBytes), nil
}

func (wk *wukongDB) DeleteLeaderTermStartIndexGreaterThanTerm(shardNo string, term uint32) error {

	return wk.shardDB(shardNo).DeleteRange(key.NewLeaderTermSequenceTermKey(shardNo, term+1), key.NewLeaderTermSequenceTermKey(shardNo, math.MaxUint32), wk.sync)
}
