package shardingconfig

import (
	"sync"
)

type LocalnetConfig struct {
	BlocksPerEpoch   uint64
	BlocksPerEpochV2 uint64
	NumShards        uint32
}

var lnc *LocalnetConfig
var once sync.Once

// GetLocalnetConfig returns localnet config
func GetLocalnetConfig() *LocalnetConfig {
	if lnc == nil {
		panic("localnet config is not set")
	}
	return lnc
}

// NormalizeLocalnetNumShards returns a supported shard count (2, 4, 8, or 16); unknown values become 2.
func NormalizeLocalnetNumShards(n uint32) uint32 {
	switch n {
	case 2, 4, 8, 16:
		return n
	default:
		return 2
	}
}

// TryGetLocalnetNumShards returns configured shard count, or 2 if localnet config is not initialized yet.
func TryGetLocalnetNumShards() uint32 {
	if lnc == nil {
		return 2
	}
	return NormalizeLocalnetNumShards(lnc.NumShards)
}

// InitLocalnetConfig initialize localnet config (once). numShards 0 or unsupported values become 2.
func InitLocalnetConfig(blocksPerEpoch, blocksPerEpochV2 uint64, numShards uint32) {
	once.Do(func() {
		ns := NormalizeLocalnetNumShards(numShards)
		lnc = &LocalnetConfig{
			BlocksPerEpoch:   blocksPerEpoch,
			BlocksPerEpochV2: blocksPerEpochV2,
			NumShards:        ns,
		}
		InitLocalnetInstances()
	})
}
