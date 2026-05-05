package shardingconfig

import (
	"fmt"
	"math/big"
	"strings"

	ethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/internal/params"
	"github.com/harmony-one/harmony/numeric"

	"github.com/harmony-one/harmony/internal/genesis"
)

var (
	localnetReshardingEpoch = []*big.Int{
		big.NewInt(0), big.NewInt(localnetV1Epoch), params.LocalnetChainConfig.StakingEpoch, params.LocalnetChainConfig.TwoSecondsEpoch,
	}
	// Number of shards, how many slots on each , how many slots owned by Harmony
	localnetV0   Instance
	localnetV1   Instance
	localnetV2   Instance
	localnetV3   Instance
	localnetV3_1 Instance
	localnetV3_2 Instance
	localnetV4   Instance
)

// LocalnetSchedule is the local testnet sharding
// configuration schedule.
var LocalnetSchedule localnetSchedule

var feeCollectorsLocalnet = FeeCollectors{
	// pk: 0x1111111111111111111111111111111111111111111111111111111111111111
	mustAddress("0x19E7E376E7C213B7E7e7e46cc70A5dD086DAff2A"): numeric.MustNewDecFromStr("0.5"),
	// pk: 0x2222222222222222222222222222222222222222222222222222222222222222
	mustAddress("0x1563915e194D8CfBA1943570603F7606A3115508"): numeric.MustNewDecFromStr("0.5"),
}

// pk: 0x3333333333333333333333333333333333333333333333333333333333333333
var hip30CollectionAddressLocalnet = mustAddress("0x5CbDd86a2FA8Dc4bDdd8a8f69dBa48572EeC07FB")

type localnetSchedule struct{}

const (
	localnetV1Epoch = 1

	localnetEpochBlock1 = 5

	localnetVdfDifficulty = 5000 // This takes about 10s to finish the vdf
)

func (ls localnetSchedule) InstanceForEpoch(epoch *big.Int) Instance {
	switch {
	case params.LocalnetChainConfig.IsOneSecond(epoch):
		return localnetV4
	case params.LocalnetChainConfig.IsHIP30(epoch):
		return localnetV4
	case params.LocalnetChainConfig.IsFeeCollectEpoch(epoch):
		return localnetV3_2
	case params.LocalnetChainConfig.IsSixtyPercent(epoch):
		return localnetV3_1
	case params.LocalnetChainConfig.IsTwoSeconds(epoch):
		return localnetV3
	case params.LocalnetChainConfig.IsStaking(epoch):
		return localnetV2
	case epoch.Cmp(big.NewInt(localnetV1Epoch)) >= 0:
		return localnetV1
	default: // genesis
		return localnetV0
	}
}

func (ls localnetSchedule) BlocksPerEpochOld() uint64 {
	localnetConfig := GetLocalnetConfig()
	return localnetConfig.BlocksPerEpoch
}

func (ls localnetSchedule) BlocksPerEpoch() uint64 {
	localnetConfig := GetLocalnetConfig()
	return localnetConfig.BlocksPerEpochV2
}

func (ls localnetSchedule) twoSecondsFirstBlock() uint64 {
	if params.LocalnetChainConfig.TwoSecondsEpoch.Uint64() == 0 {
		return 0
	}
	return (params.LocalnetChainConfig.TwoSecondsEpoch.Uint64()-1)*ls.BlocksPerEpochOld() + localnetEpochBlock1
}

func (ls localnetSchedule) CalcEpochNumber(blockNum uint64) *big.Int {
	firstBlock2s := ls.twoSecondsFirstBlock()
	switch {
	case blockNum < localnetEpochBlock1:
		return big.NewInt(0)
	case blockNum < firstBlock2s:
		return big.NewInt(int64((blockNum-localnetEpochBlock1)/ls.BlocksPerEpochOld() + 1))
	default:
		extra := uint64(0)
		if firstBlock2s == 0 {
			blockNum -= localnetEpochBlock1
			extra = 1
		}
		return big.NewInt(int64(extra + (blockNum-firstBlock2s)/ls.BlocksPerEpoch() + params.LocalnetChainConfig.TwoSecondsEpoch.Uint64()))
	}
}

func (ls localnetSchedule) IsLastBlock(blockNum uint64) bool {
	switch {
	case blockNum < localnetEpochBlock1-1:
		return false
	case blockNum == localnetEpochBlock1-1:
		return true
	default:
		firstBlock2s := ls.twoSecondsFirstBlock()
		switch {
		case blockNum >= firstBlock2s:
			if firstBlock2s == 0 {
				blockNum -= localnetEpochBlock1
			}
			return ((blockNum-firstBlock2s)%ls.BlocksPerEpoch() == ls.BlocksPerEpoch()-1)
		default: // genesis
			blocks := ls.BlocksPerEpochOld()
			return ((blockNum-localnetEpochBlock1)%blocks == blocks-1)
		}
	}
}

func (ls localnetSchedule) EpochLastBlock(epochNum uint64) uint64 {
	switch {
	case epochNum == 0:
		return localnetEpochBlock1 - 1
	default:
		switch {
		case params.LocalnetChainConfig.IsTwoSeconds(big.NewInt(int64(epochNum))):
			blocks := ls.BlocksPerEpoch()
			firstBlock2s := ls.twoSecondsFirstBlock()
			block2s := (1 + epochNum - params.LocalnetChainConfig.TwoSecondsEpoch.Uint64()) * blocks
			if firstBlock2s == 0 {
				return block2s - blocks + localnetEpochBlock1 - 1
			}
			return firstBlock2s + block2s - 1
		default: // genesis
			blocks := ls.BlocksPerEpochOld()
			return localnetEpochBlock1 + blocks*epochNum - 1
		}
	}
}

func (ls localnetSchedule) VdfDifficulty() int {
	return localnetVdfDifficulty
}

func (ls localnetSchedule) GetNetworkID() NetworkID {
	return LocalNet
}

// BuildLocalnetJoyueOtherShardRPCs builds joyue.other-shard-rpcs for localnet (HTTP RPC 9500+2*shard).
func BuildLocalnetJoyueOtherShardRPCs(numShards uint32) string {
	numShards = NormalizeLocalnetNumShards(numShards)
	if numShards < 2 {
		numShards = 2
	}
	var b strings.Builder
	for i := uint32(0); i < numShards; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		port := 9500 + 2*i
		fmt.Fprintf(&b, "%d=http://127.0.0.1:%d", i, port)
	}
	return b.String()
}

// GetShardingStructure is the sharding structure for localnet.
// For the classic 2-shard layout (test/configs/local-resharding.txt), entrypoint
// HTTP/WS ports follow 9500+2*shard / 9800+2*shard (first validator P2P 9000+2*shard, +500/+800).
// For 4/8/16 shards (test/configs/local-resharding-{4,8,16}.txt), each shard’s first validator
// P2P is 9000+shard*((NumNodesPerShard+1)*4); HTTP/WS use the same stride from 9500/9800.
func (ls localnetSchedule) GetShardingStructure(numShard, shardID int) []map[string]interface{} {
	res := []map[string]interface{}{}
	httpAt := func(shardIdx int) int { return 9500 + 2*shardIdx }
	wsAt := func(shardIdx int) int { return 9800 + 2*shardIdx }
	if numShard != 2 {
		slots := ls.InstanceForEpoch(big.NewInt(0)).NumNodesPerShard() + 1
		if slots < 2 {
			slots = 2
		}
		stride := slots * 4
		httpAt = func(shardIdx int) int { return 9500 + shardIdx*stride }
		wsAt = func(shardIdx int) int { return 9800 + shardIdx*stride }
	}
	for i := 0; i < numShard; i++ {
		res = append(res, map[string]interface{}{
			"current": int(shardID) == i,
			"shardID": i,
			"http":    fmt.Sprintf("http://127.0.0.1:%d", httpAt(i)),
			"ws":      fmt.Sprintf("ws://127.0.0.1:%d", wsAt(i)),
		})
	}
	return res
}

// IsSkippedEpoch returns if an epoch was skipped on shard due to staking epoch
func (ls localnetSchedule) IsSkippedEpoch(shardID uint32, epoch *big.Int) bool {
	return false
}

// RewardFrequency returns the frequency of block reward
func (ls localnetSchedule) RewardFrequency() uint64 {
	return 16
}

func InitLocalnetInstances() {
	n := GetLocalnetConfig().NumShards
	if n == 0 {
		n = 2
	}
	n = NormalizeLocalnetNumShards(n)

	if n == 2 {
		localnetV0 = MustNewInstance(
			2, 7, 5, 0,
			numeric.OneDec(), genesis.LocalHarmonyAccounts,
			genesis.LocalFnAccounts, emptyAllowlist, nil,
			numeric.ZeroDec(), ethCommon.Address{},
			localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpochOld(),
		)
		localnetV1 = MustNewInstance(
			2, 8, 5, 0,
			numeric.OneDec(), genesis.LocalHarmonyAccountsV1,
			genesis.LocalFnAccountsV1, emptyAllowlist, nil,
			numeric.ZeroDec(), ethCommon.Address{},
			localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpochOld(),
		)
		localnetV2 = MustNewInstance(
			2, 9, 6, 0,
			numeric.MustNewDecFromStr("0.68"),
			genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
			emptyAllowlist, nil,
			numeric.ZeroDec(), ethCommon.Address{},
			localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpochOld(),
		)
		localnetV3 = MustNewInstance(
			2, 9, 6, 0,
			numeric.MustNewDecFromStr("0.68"),
			genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
			emptyAllowlist, nil,
			numeric.ZeroDec(), ethCommon.Address{},
			localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
		)
		localnetV3_1 = MustNewInstance(
			2, 9, 6, 0,
			numeric.MustNewDecFromStr("0.68"),
			genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
			emptyAllowlist, nil,
			numeric.ZeroDec(), ethCommon.Address{},
			localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
		)
		localnetV3_2 = MustNewInstance(
			2, 9, 6, 0,
			numeric.MustNewDecFromStr("0.68"),
			genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
			emptyAllowlist, feeCollectorsLocalnet,
			numeric.ZeroDec(), ethCommon.Address{},
			localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
		)
		localnetV4 = MustNewInstance(
			2, 9, 6, 0, numeric.MustNewDecFromStr("0.68"),
			genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
			emptyAllowlist, feeCollectorsLocalnet,
			numeric.MustNewDecFromStr("0.25"), hip30CollectionAddressLocalnet,
			localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
		)
		return
	}

	// Multi-shard localnet (4/8/16): uniform topology per epoch so genesis committee matches key list length.
	// 7 nodes per shard, 6 Harmony + 1 FN; requires len(LocalHarmonyAccountsV2) >= 6*N and len(LocalFnAccountsV2) >= N.
	const nodesPerShard = 7
	const hmyPerShard = 6
	needH := int(n) * hmyPerShard
	needF := int(n) * (nodesPerShard - hmyPerShard)
	if len(genesis.LocalHarmonyAccountsV2) < needH {
		panic(fmt.Sprintf(
			"localnet: need at least %d harmony accounts for %d shards (6 per shard), have %d",
			needH, n, len(genesis.LocalHarmonyAccountsV2),
		))
	}
	if len(genesis.LocalFnAccountsV2) < needF {
		panic(fmt.Sprintf(
			"localnet: need at least %d FN accounts for %d shards (1 per shard), have %d",
			needF, n, len(genesis.LocalFnAccountsV2),
		))
	}

	localnetV0 = MustNewInstance(
		n, nodesPerShard, hmyPerShard, 0,
		numeric.OneDec(), genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
		emptyAllowlist, nil,
		numeric.ZeroDec(), ethCommon.Address{},
		localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpochOld(),
	)
	localnetV1 = MustNewInstance(
		n, nodesPerShard, hmyPerShard, 0,
		numeric.OneDec(), genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
		emptyAllowlist, nil,
		numeric.ZeroDec(), ethCommon.Address{},
		localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpochOld(),
	)
	localnetV2 = MustNewInstance(
		n, nodesPerShard, hmyPerShard, 0,
		numeric.MustNewDecFromStr("0.68"),
		genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
		emptyAllowlist, nil,
		numeric.ZeroDec(), ethCommon.Address{},
		localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpochOld(),
	)
	localnetV3 = MustNewInstance(
		n, nodesPerShard, hmyPerShard, 0,
		numeric.MustNewDecFromStr("0.68"),
		genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
		emptyAllowlist, nil,
		numeric.ZeroDec(), ethCommon.Address{},
		localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
	)
	localnetV3_1 = MustNewInstance(
		n, nodesPerShard, hmyPerShard, 0,
		numeric.MustNewDecFromStr("0.68"),
		genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
		emptyAllowlist, nil,
		numeric.ZeroDec(), ethCommon.Address{},
		localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
	)
	localnetV3_2 = MustNewInstance(
		n, nodesPerShard, hmyPerShard, 0,
		numeric.MustNewDecFromStr("0.68"),
		genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
		emptyAllowlist, feeCollectorsLocalnet,
		numeric.ZeroDec(), ethCommon.Address{},
		localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
	)
	localnetV4 = MustNewInstance(
		n, nodesPerShard, hmyPerShard, 0, numeric.MustNewDecFromStr("0.68"),
		genesis.LocalHarmonyAccountsV2, genesis.LocalFnAccountsV2,
		emptyAllowlist, feeCollectorsLocalnet,
		numeric.MustNewDecFromStr("0.25"), hip30CollectionAddressLocalnet,
		localnetReshardingEpoch, LocalnetSchedule.BlocksPerEpoch(),
	)
}
