package vm

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/params"
	"github.com/harmony-one/harmony/internal/utils"
)

/*
JOYUE Coordinator Precompile

预编译地址：0x0000000000000000000000000000000000000073 (0x73 = 115)

功能：
- 作为 JoyueCoordinator 合约的执行引擎
- 通过 delegatecall 访问合约的 storage
- 执行 Guard 验证、Delta 应用、2PC 协调等核心逻辑

调用格式：
通过 delegatecall 调用，calldata 格式与 Solidity 函数调用一致
*/

// joyueCoordinatorPrecompile 已注册到 WriteCapablePrecompiledContractsJoyue

// 函数选择器常量
const (
	// batchVerifyAndFreeze(bytes32,ColumnTx[])
	// 函数签名: batchVerifyAndFreeze(bytes32,(bytes32,(uint32,(address,bytes32,uint64,uint8,uint8,bytes))[],(address,bytes32,uint8,bytes)[])[])
	batchVerifyAndFreezeSelector = 0x94ef8ad1
	// finalizeBatch(bytes32,bytes32[],bool)
	finalizeBatchSelector = 0x98217df3
	// clearRound(bytes32)
	clearRoundSelector = 0x697cc3a3
	// processBundleWithOneRetry(...)
	processBundleWithOneRetrySelector = 0xdae3bdad
	// processIntentBatch(bytes[]) - keccak256("processIntentBatch(bytes[])")[:4]
	processIntentBatchSelector = 0x7d5e812e
	// processIntent(bytes) - selector computed via keccak256 at init
	// onCrossShardCallback(uint256,bool,bytes)
	// 函数签名: onCrossShardCallback(uint256,bool,bytes)
	// 计算: crypto.Keccak256([]byte("onCrossShardCallback(uint256,bool,bytes)"))[:4]
	onCrossShardCallbackSelector = 0x8f4ffcb1 // 需要验证，但先使用这个值
)

var processIntentSelector = binary.BigEndian.Uint32(crypto.Keccak256([]byte("processIntent(bytes)"))[:4])

// applyCommitAndRetry(bytes32,bytes32[],bytes32[],bytes32,bytes)
// 用于 attempt 2：在远端分片上顺序执行 finalize C1 + unfreeze R + re-freeze R(batch2)
var applyCommitAndRetrySelector = binary.BigEndian.Uint32(crypto.Keccak256([]byte("applyCommitAndRetry(bytes32,bytes32[],bytes32[],bytes32,bytes)"))[:4])

// applyFinalize(bytes32,bytes32[],bytes32[])
// 用于 attempt 3：在远端分片上顺序执行 finalize C2 + rollback F2
var applyFinalizeSelector = binary.BigEndian.Uint32(crypto.Keccak256([]byte("applyFinalize(bytes32,bytes32[],bytes32[])"))[:4])

// recomputeIntent(RawRequest,address,StateOverride[]) - Agent 重新计算 guards/deltas（用于 retry）
var recomputeIntentSelector = binary.BigEndian.Uint32(crypto.Keccak256([]byte("recomputeIntent((address,bytes4,bytes),address,(address,uint32,bytes32,uint256,uint64)[])"))[:4])

// Guard 操作常量
const (
	OP_EQ   = 0x01
	OP_NEQ  = 0x02
	OP_GT   = 0x03
	OP_GTE  = 0x04
	OP_LT   = 0x05
	OP_LTE  = 0x06
	OP_EXPR = 0xFF
)

// Delta 操作常量
const (
	D_ADD       = 0x01
	D_SUB       = 0x02
	D_SET       = 0x10
	D_BIT_OR    = 0x20
	D_BIT_CLEAR = 0x21
	D_BIT_XOR   = 0x22
)

// Guard 策略常量
const (
	STRATEGY_STRICT  = 0x00
	STRATEGY_RELAXED = 0x01
)

// joyueCoordinatorPrecompile 实现 JOYUE Coordinator 的预编译合约
type joyueCoordinatorPrecompile struct{}

// ============================================================
// ABI decode helpers (uint256/offset/len)
// ============================================================
// ABI 中 uint256/offset/len 都是 32 bytes，大端序，数值右对齐。
// 使用 big.Int 读取完整 uint256，若值 <= uint64 则返回；否则报溢出。
// 兼容不同来源的 ABI 编码（Relayer、go-ethereum abi.Pack、Solidity 等）。
func readU256AsUint64(data []byte, pos int) (uint64, error) {
	if pos < 0 || pos+32 > len(data) {
		return 0, errors.New("JOYUE: readU256 out of range")
	}
	v := new(big.Int).SetBytes(data[pos : pos+32])
	if !v.IsUint64() {
		return 0, errors.New("JOYUE: uint256 overflow (> uint64)")
	}
	return v.Uint64(), nil
}

func readU256AsInt(data []byte, pos int) (int, error) {
	v, err := readU256AsUint64(data, pos)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// abiUint256 将 uint64 编码为 32 字节大端序（ABI uint256）
func abiUint256(v uint64) []byte {
	b := make([]byte, 32)
	binary.BigEndian.PutUint64(b[24:32], v)
	return b
}

// abiDecodeBytes32Array 从 ABI 编码的 params 中按绝对偏移 offset 解码 bytes32[] 数组
func abiDecodeBytes32Array(params []byte, offset int) ([]common.Hash, error) {
	if offset+32 > len(params) {
		return nil, fmt.Errorf("JOYUE: abiDecodeBytes32Array offset %d out of range", offset)
	}
	length, err := readU256AsInt(params, offset)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: abiDecodeBytes32Array length: %w", err)
	}
	if offset+32+32*length > len(params) {
		return nil, fmt.Errorf("JOYUE: abiDecodeBytes32Array elements out of range (need %d, have %d)", offset+32+32*length, len(params))
	}
	result := make([]common.Hash, length)
	for i := 0; i < length; i++ {
		elemPos := offset + 32 + i*32
		result[i] = common.BytesToHash(params[elemPos : elemPos+32])
	}
	return result, nil
}

// abiDecodeBytes 从 ABI 编码的 params 中按绝对偏移 offset 解码 bytes 值
func abiDecodeBytes(params []byte, offset int) ([]byte, error) {
	if offset+32 > len(params) {
		return nil, fmt.Errorf("JOYUE: abiDecodeBytes offset %d out of range", offset)
	}
	length, err := readU256AsInt(params, offset)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: abiDecodeBytes length: %w", err)
	}
	if offset+32+length > len(params) {
		return nil, fmt.Errorf("JOYUE: abiDecodeBytes data out of range (need %d, have %d)", offset+32+length, len(params))
	}
	result := make([]byte, length)
	copy(result, params[offset+32:offset+32+length])
	return result, nil
}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueCoordinatorPrecompile) RequiredGas(evm *EVM, contract *Contract, input []byte) (uint64, error) {
	if len(input) < 4 {
		return 0, errors.New("JOYUE: invalid input length")
	}

	// 解析函数选择器
	selector := binary.BigEndian.Uint32(input[0:4])

	// 根据不同的函数选择器返回不同的 gas
	baseGas := uint64(1000)
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas

	// 根据函数选择器调整 gas
	switch selector {
	case batchVerifyAndFreezeSelector:
		// 基础 gas + 数据 gas + 每个交易的验证 gas（估算）
		// 每个交易大约需要 10000 gas（Guard 验证 + Delta 应用）
		return baseGas + dataGas + uint64(10000), nil
	case finalizeBatchSelector:
		// 基础 gas + 数据 gas + 每个交易的最终化 gas
		return baseGas + dataGas + uint64(5000), nil
	case clearRoundSelector:
		// 基础 gas + 数据 gas
		return baseGas + dataGas, nil
	case processBundleWithOneRetrySelector:
		// 基础 gas + 数据 gas + 复杂的 2PC 协调 gas
		return baseGas + dataGas + uint64(100000), nil
	case processIntentSelector:
		// 解析 payload + 执行 2PC 协调，按较高成本估算
		return baseGas + dataGas + uint64(100000), nil
	case processIntentBatchSelector:
		// 批量处理，按多个 intent 估算
		return baseGas + dataGas + uint64(300000), nil
	case applyCommitAndRetrySelector:
		// finalize C1 + unfreeze R + re-freeze R，成本与 batchVerifyAndFreeze 相当
		return baseGas + dataGas + uint64(20000), nil
	case applyFinalizeSelector:
		// finalize C2 + rollback F2，成本与 finalizeBatch 相当
		return baseGas + dataGas + uint64(10000), nil
	default:
		return baseGas + dataGas, nil
	}
}

// RunWriteCapable 执行预编译合约
func (c *joyueCoordinatorPrecompile) RunWriteCapable(evm *EVM, contract *Contract, input []byte) ([]byte, error) {
	if len(input) < 4 {
		return nil, errors.New("JOYUE: invalid input length")
	}

	// 解析函数选择器
	selector := binary.BigEndian.Uint32(input[0:4])
	paramsInfo := input[4:]

	// contract.Address() 是 JoyueCoordinator 的地址（通过 delegatecall）
	coordinatorAddr := contract.Address()

	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Str("caller", contract.CallerAddress.Hex()).
		Str("selector", fmt.Sprintf("0x%08x", selector)).
		Int("inputLen", len(input)).
		Int("paramsLen", len(paramsInfo)).
		Msg("[JOYUE Coordinator] RunWriteCapable called")

	// 根据函数选择器路由到不同的处理函数
	switch selector {
	case processIntentSelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleProcessIntent")
		return c.handleProcessIntent(evm, contract, coordinatorAddr, paramsInfo)
	case processIntentBatchSelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleProcessIntentBatch")
		return c.handleProcessIntentBatch(evm, contract, coordinatorAddr, paramsInfo)
	case batchVerifyAndFreezeSelector:
		utils.Logger().Info().
			Str("selector", fmt.Sprintf("0x%08x", selector)).
			Int("paramsInfoLen", len(paramsInfo)).
			Msg("[JOYUE Coordinator] routing to handleBatchVerifyAndFreeze")
		return c.handleBatchVerifyAndFreeze(evm, contract, coordinatorAddr, paramsInfo)
	case finalizeBatchSelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleFinalizeBatch")
		return c.handleFinalizeBatch(evm, contract, coordinatorAddr, paramsInfo)
	case clearRoundSelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleClearRound")
		return c.handleClearRound(evm, contract, coordinatorAddr, paramsInfo)
	case processBundleWithOneRetrySelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleProcessBundleWithOneRetry")
		return c.handleProcessBundleWithOneRetry(evm, contract, coordinatorAddr, paramsInfo)
	case onCrossShardCallbackSelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleOnCrossShardCallback")
		return c.handleOnCrossShardCallback(evm, contract, coordinatorAddr, paramsInfo)
	case applyCommitAndRetrySelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleApplyCommitAndRetry")
		return c.handleApplyCommitAndRetry(evm, contract, coordinatorAddr, paramsInfo)
	case applyFinalizeSelector:
		utils.Logger().Info().Msg("[JOYUE Coordinator] routing to handleApplyFinalize")
		return c.handleApplyFinalize(evm, coordinatorAddr, paramsInfo)
	default:
		utils.Logger().Warn().
			Str("selector", fmt.Sprintf("0x%08x", selector)).
			Msg("[JOYUE Coordinator] unknown function selector")
		return nil, fmt.Errorf("JOYUE: unknown function selector 0x%08x", selector)
	}
}

// ============================================================
// Storage 访问辅助函数
// ============================================================

// Storage 布局（基于 JoyueStorage.sol）：
// slot 0: mapping(bytes32 => uint256) _u
// slot 1: mapping(bytes32 => uint64) _ver
// slot 2: address public rpcOracle
// 注意：
// - 瞬时状态（_tU, _tSet, _tKeys, _tKeySeen）现在使用内存实现，不再使用 Storage
// - _frozenDeltas 已改为 Blob 模式存储，不再使用 mapping

const (
	storageSlotU         = 0 // mapping(bytes32 => uint256) _u
	storageSlotVer       = 1 // mapping(bytes32 => uint64) _ver
	storageSlotRpcOracle = 2 // address public rpcOracle

	// 异步 2PC 相关存储槽
	storageSlotPendingBatch = 3 // mapping(bytes32 => PendingBatch) _pendingBatches
	storageSlotResultMatrix = 4 // mapping(bytes32 => mapping(address => ResultMatrix)) _resultMatrices
	storageSlotRequestIdMap = 5 // mapping(uint256 => RequestIdInfo) _requestIdMap

	// Blob 存储模式：使用 keccak256(domain|batchId|key) 作为 baseSlot，无 Mod 避免碰撞
	// 读取时 length 上限，防止 slot 碰撞导致污染 length 引发 OOM 死循环
	maxBlobSize = 512 * 1024 // 512KB，单 blob 最大合理大小

	// 主合约侧 D_SUB 冻结表（解决并发 commit 时错误广播未确认修改的问题）
	storageSlotTotalFrozen  = 6 // mapping(bytes32 => uint256) key => 未 commit 的 D_SUB 冻结量之和
	storageSlotFrozenMaster = 7 // mapping(bytes32 => mapping(bytes32 => uint256)) key => txHash => amount
)

// getMappingSlot 计算 mapping 的 storage slot
// mappingSlot: mapping 所在的 slot (*big.Int，需要转换为 32 字节)
// key: mapping 的 key
func getMappingSlot(mappingSlot *big.Int, key common.Hash) common.Hash {
	// Solidity 中 mapping 的 slot 计算：keccak256(abi.encode(key, mappingSlot))
	// 注意：Solidity 的编码是 key (32 bytes) + mappingSlot (32 bytes)
	// mappingSlot 需要转换为 big-endian 32 字节
	slotBytes := make([]byte, 32)
	mappingSlot.FillBytes(slotBytes)
	return crypto.Keccak256Hash(key.Bytes(), slotBytes)
}

// getNestedMappingSlot 计算嵌套 mapping 的 storage slot
// mappingSlot: 外层 mapping 所在的 slot (*big.Int)
// outerKey: 外层 mapping 的 key
// innerKey: 内层 mapping 的 key
func getNestedMappingSlot(mappingSlot *big.Int, outerKey common.Hash, innerKey common.Hash) common.Hash {
	// 先计算外层 mapping 的 slot
	outerSlotHash := getMappingSlot(mappingSlot, outerKey)
	// 将 outerSlotHash 转换为 *big.Int
	outerSlot := new(big.Int).SetBytes(outerSlotHash.Bytes())
	// 再计算内层 mapping 的 slot
	return getMappingSlot(outerSlot, innerKey)
}

// getTotalFrozenForKey 读取 key 上未 commit 的 D_SUB 冻结量之和
func (c *joyueCoordinatorPrecompile) getTotalFrozenForKey(evm *EVM, addr common.Address, key common.Hash) *big.Int {
	slot := getMappingSlot(big.NewInt(storageSlotTotalFrozen), key)
	val := evm.StateDB.GetState(addr, slot)
	return new(big.Int).SetBytes(val.Bytes())
}

// addToMasterFreezeTable 将 D_SUB 冻结量加入主合约冻结表
func (c *joyueCoordinatorPrecompile) addToMasterFreezeTable(evm *EVM, addr common.Address, key common.Hash, txHash common.Hash, amount *big.Int) {
	// _frozenMaster[key][txHash] = amount
	txSlot := getNestedMappingSlot(big.NewInt(storageSlotFrozenMaster), key, txHash)
	amountBytes := make([]byte, 32)
	amount.FillBytes(amountBytes)
	evm.StateDB.SetState(addr, txSlot, common.BytesToHash(amountBytes))

	// _totalFrozenForKey[key] += amount
	totalSlot := getMappingSlot(big.NewInt(storageSlotTotalFrozen), key)
	totalVal := evm.StateDB.GetState(addr, totalSlot)
	totalInt := new(big.Int).SetBytes(totalVal.Bytes())
	totalInt.Add(totalInt, amount)
	newBytes := make([]byte, 32)
	totalInt.FillBytes(newBytes)
	evm.StateDB.SetState(addr, totalSlot, common.BytesToHash(newBytes))
}

// removeFromMasterFreezeTable 从主合约冻结表移除 txHash 的冻结量
func (c *joyueCoordinatorPrecompile) removeFromMasterFreezeTable(evm *EVM, addr common.Address, key common.Hash, txHash common.Hash) (amount *big.Int) {
	txSlot := getNestedMappingSlot(big.NewInt(storageSlotFrozenMaster), key, txHash)
	val := evm.StateDB.GetState(addr, txSlot)
	amount = new(big.Int).SetBytes(val.Bytes())
	if amount.Sign() == 0 {
		return amount
	}
	// 清除 _frozenMaster[key][txHash]
	evm.StateDB.SetState(addr, txSlot, common.Hash{})

	// _totalFrozenForKey[key] -= amount
	totalSlot := getMappingSlot(big.NewInt(storageSlotTotalFrozen), key)
	totalVal := evm.StateDB.GetState(addr, totalSlot)
	totalInt := new(big.Int).SetBytes(totalVal.Bytes())
	totalInt.Sub(totalInt, amount)
	if totalInt.Sign() < 0 {
		totalInt.SetInt64(0)
	}
	newBytes := make([]byte, 32)
	totalInt.FillBytes(newBytes)
	evm.StateDB.SetState(addr, totalSlot, common.BytesToHash(newBytes))
	return amount
}

// getAuthValue 读取权威状态原始值（不含冻结表扣减）
func (c *joyueCoordinatorPrecompile) getAuthValue(evm *EVM, addr common.Address, key common.Hash) common.Hash {
	uSlot := getMappingSlot(big.NewInt(storageSlotU), key)
	return evm.StateDB.GetState(addr, uSlot)
}

// collectStateOverridesFromBatch1 从 batch1 的 callback 结果收集 StateOverride（guard/delta 失败时返回的权威最新状态）
func (c *joyueCoordinatorPrecompile) collectStateOverridesFromBatch1(evm *EVM, coordinatorAddr common.Address, batch1 common.Hash, participants []Participant, details []IntentDetail, retryActive []bool) map[common.Hash][]StateOverride {
	utils.Logger().Info().
		Str("batch1", batch1.Hex()).
		Int("participants", len(participants)).
		Int("details", len(details)).
		Msg("[JOYUE Coordinator] collectStateOverridesFromBatch1: start")

	result := make(map[common.Hash][]StateOverride)
	seenByTx := make(map[common.Hash]map[common.Hash]bool) // txHash -> (contractAddr,shardId,key) 去重

	for pIdx, participant := range participants {
		pr, err := c.getParticipantResult(evm, coordinatorAddr, batch1, participant)
		if err != nil {
			utils.Logger().Debug().
				Int("participantIndex", pIdx).
				Str("participant", participant.Addr.Hex()).
				Uint32("shardId", participant.ShardId).
				Err(err).
				Msg("[JOYUE Coordinator] collectStateOverridesFromBatch1: participant result not found, skip")
			continue
		}
		for i := range details {
			if !retryActive[i] || i >= len(pr.GuardResults) || i >= len(pr.DeltaResults) {
				continue
			}
			txHash := details[i].TxHash
			if seenByTx[txHash] == nil {
				seenByTx[txHash] = make(map[common.Hash]bool)
			}
			seen := seenByTx[txHash]
			gr := &pr.GuardResults[i]
			dr := &pr.DeltaResults[i]

			// Guard 失败：从 details[i].Guards[FailedGuardIndex] 取 contractAddr/shardId，从 GuardResult 取 LatestKey/Val/Ver
			if gr.GuardFailed && int(gr.FailedGuardIndex) < len(details[i].Guards) {
				g := &details[i].Guards[gr.FailedGuardIndex]
				key := stateOverrideKey(g.ContractAddr, g.ShardId, gr.LatestKey)
				if !seen[key] && gr.LatestVal != nil {
					seen[key] = true
					result[txHash] = append(result[txHash], StateOverride{
						ContractAddr: g.ContractAddr,
						ShardId:      g.ShardId,
						Key:          gr.LatestKey,
						Value:        gr.LatestVal,
						Version:      gr.LatestVer,
					})
					utils.Logger().Info().
						Str("txHash", txHash.Hex()).
						Str("source", "guard").
						Str("contractAddr", g.ContractAddr.Hex()).
						Uint32("shardId", g.ShardId).
						Str("key", gr.LatestKey.Hex()).
						Str("value", gr.LatestVal.String()).
						Uint64("version", gr.LatestVer).
						Msg("[JOYUE Coordinator] collectStateOverridesFromBatch1: added guard override")
				}
			}
			// Delta 失败：FailedDeltaIndex 是 column 内的索引，需映射回 details[i].Deltas
			if !dr.Ok {
				var d *Delta
				colIdx := 0
				for j := range details[i].Deltas {
					if details[i].Deltas[j].ContractAddr == participant.Addr && details[i].Deltas[j].ShardId == participant.ShardId {
						if colIdx == int(dr.FailedDeltaIndex) {
							d = &details[i].Deltas[j]
							break
						}
						colIdx++
					}
				}
				if d != nil {
					key := stateOverrideKey(d.ContractAddr, d.ShardId, dr.LatestKey)
					if !seen[key] && dr.LatestVal != nil {
						seen[key] = true
						result[txHash] = append(result[txHash], StateOverride{
							ContractAddr: d.ContractAddr,
							ShardId:      d.ShardId,
							Key:          dr.LatestKey,
							Value:        dr.LatestVal,
							Version:      dr.LatestVer,
						})
						utils.Logger().Info().
							Str("txHash", txHash.Hex()).
							Str("source", "delta").
							Str("contractAddr", d.ContractAddr.Hex()).
							Uint32("shardId", d.ShardId).
							Str("key", dr.LatestKey.Hex()).
							Str("value", dr.LatestVal.String()).
							Uint64("version", dr.LatestVer).
							Msg("[JOYUE Coordinator] collectStateOverridesFromBatch1: added delta override")
					}
				}
			}
		}
	}

	totalOverrides := 0
	for txHash, ovs := range result {
		totalOverrides += len(ovs)
		utils.Logger().Info().
			Str("txHash", txHash.Hex()).
			Int("overrideCount", len(ovs)).
			Msg("[JOYUE Coordinator] collectStateOverridesFromBatch1: tx override summary")
	}
	utils.Logger().Info().
		Int("txCount", len(result)).
		Int("totalOverrides", totalOverrides).
		Msg("[JOYUE Coordinator] collectStateOverridesFromBatch1: done")
	return result
}

func stateOverrideKey(addr common.Address, shardId uint32, key common.Hash) common.Hash {
	shardBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(shardBytes, shardId)
	return crypto.Keccak256Hash(addr.Bytes(), shardBytes, key.Bytes())
}

// getAgentOnSameShard 从 Coordinator 存储读取 agentOnSameShard（JoyueStorageV2 slot 3）
func (c *joyueCoordinatorPrecompile) getAgentOnSameShard(evm *EVM, coordinatorAddr common.Address) common.Address {
	slot3 := common.BigToHash(big.NewInt(3))
	val := evm.StateDB.GetState(coordinatorAddr, slot3)
	return common.BytesToAddress(val[12:32]) // address 右对齐 20 字节
}

// buildRecomputeIntentCalldata 构建 recomputeIntent(req, user, stateOverrides) 的 ABI calldata
//func (c *joyueCoordinatorPrecompile) buildRecomputeIntentCalldata(req RawRequest, user common.Address, overrides []StateOverride) []byte {
//	// ABI: selector(4) + head(96) + tail
//	// head: offset_to_RawRequest(32)=96, user(32), offset_to_StateOverride[](32)
//	// tail: RawRequest(96+len(args)) + StateOverride[]
//	headLen := 4 + 96
//	rawReqStart := headLen
//	rawReqLen := 64 + 32 + len(req.Args) // targetAddr(32)+selector(32) + argsOffset(32) + argsLen(32) + args
//	overridesStart := rawReqStart + rawReqLen
//	overridesLen := 32 + len(overrides)*160
//	totalLen := overridesStart + overridesLen
//
//	calldata := make([]byte, totalLen)
//	binary.BigEndian.PutUint32(calldata[0:4], recomputeIntentSelector)
//	copy(calldata[4:36], abiUint256(96))
//	copy(calldata[36:56], user.Bytes())
//	copy(calldata[68:100], abiUint256(uint64(overridesStart-4))) // offset from params start (byte 4)
//	// RawRequest at rawReqStart
//	copy(calldata[rawReqStart:rawReqStart+20], req.TargetAddr.Bytes())
//	copy(calldata[rawReqStart+32:rawReqStart+36], req.Selector[:])
//	copy(calldata[rawReqStart+64:rawReqStart+96], abiUint256(64))
//	copy(calldata[rawReqStart+96:rawReqStart+128], abiUint256(uint64(len(req.Args))))
//	copy(calldata[rawReqStart+128:], req.Args)
//	// StateOverride[]
//	copy(calldata[overridesStart:overridesStart+32], abiUint256(uint64(len(overrides))))
//	for i, ov := range overrides {
//		base := overridesStart + 32 + i*160
//		copy(calldata[base+12:base+32], ov.ContractAddr.Bytes()) // address 右对齐
//		binary.BigEndian.PutUint32(calldata[base+60:base+64], ov.ShardId)
//		copy(calldata[base+64:base+96], ov.Key.Bytes())
//		if ov.Value != nil {
//			ov.Value.FillBytes(calldata[base+96 : base+128])
//		}
//		binary.BigEndian.PutUint64(calldata[base+152:base+160], ov.Version)
//	}
//	return calldata
//}

// buildRecomputeIntentCalldata 构建 recomputeIntent(req, user, stateOverrides) 的 ABI calldata
func (c *joyueCoordinatorPrecompile) buildRecomputeIntentCalldata(req RawRequest, user common.Address, overrides []StateOverride) []byte {
	// ABI arguments:
	// 1. req (RawRequest tuple: address, bytes4, bytes) -> 动态结构
	// 2. user (address) -> 静态结构
	// 3. stateOverrides (StateOverride[] array) -> 动态结构

	// 1. 计算 RawRequest.args 的对齐长度
	reqArgsPaddedLen := (len(req.Args) + 31) / 32 * 32

	// RawRequest 元组长度:
	// Head: targetAddr(32) + selector(32) + argsOffset(32) = 96 字节
	// Tail: argsLength(32) + argsPaddedData
	rawReqLen := 96 + 32 + reqArgsPaddedLen

	// StateOverride[] 数组长度:
	// length(32) + elements(160 * N)
	overridesLen := 32 + len(overrides)*160

	// 总 Calldata 长度
	// selector(4) + 函数参数Head(96) + rawReqLen + overridesLen
	headLen := 96 // 3 个顶级参数占用 3 个 Slot
	totalLen := 4 + headLen + rawReqLen + overridesLen

	calldata := make([]byte, totalLen)

	// --- 写入 Selector ---
	binary.BigEndian.PutUint32(calldata[0:4], recomputeIntentSelector)

	// --- 写入函数级别的 Head (起始位置: 4) ---
	// 参数 1 偏移量 (req): 紧跟在 Head 之后，即 96
	copy(calldata[4:36], abiUint256(96))

	// 参数 2 (user 地址): address 必须右对齐至 32 字节！(写入最后 20 字节)
	copy(calldata[48:68], user.Bytes()) // 4 + 32 = 36; 36 + 32 = 68; 右对齐 68 - 20 = 48

	// 参数 3 偏移量 (stateOverrides): 在 req 数据之后
	copy(calldata[68:100], abiUint256(uint64(96+rawReqLen)))

	// --- 写入 参数 1: RawRequest (起始位置: 100) ---
	rawReqStart := 100

	// 1. targetAddr (address -> 右对齐)
	copy(calldata[rawReqStart+12:rawReqStart+32], req.TargetAddr.Bytes())

	// 2. selector (bytes4 -> 左对齐，在 32 字节块的头部)
	copy(calldata[rawReqStart+32:rawReqStart+36], req.Selector[:])

	// 3. argsOffset (指向元组内的动态数据区，相对于本元组起始位置 96 字节)
	copy(calldata[rawReqStart+64:rawReqStart+96], abiUint256(96))

	// 4. args length
	copy(calldata[rawReqStart+96:rawReqStart+128], abiUint256(uint64(len(req.Args))))

	// 5. args data (自动 padding 补齐)
	copy(calldata[rawReqStart+128:rawReqStart+128+len(req.Args)], req.Args)

	// --- 写入 参数 3: StateOverride[] (起始位置: 100 + rawReqLen) ---
	overridesStart := 100 + rawReqLen

	// 1. 数组长度
	copy(calldata[overridesStart:overridesStart+32], abiUint256(uint64(len(overrides))))

	// 2. 数组元素 (每个元素 160 bytes 的静态 Tuple)
	for i, ov := range overrides {
		base := overridesStart + 32 + i*160

		// contractAddr (address -> 右对齐)
		copy(calldata[base+12:base+32], ov.ContractAddr.Bytes())

		// shardId (uint32 -> 右对齐)
		binary.BigEndian.PutUint32(calldata[base+60:base+64], ov.ShardId)

		// key (bytes32 -> 满 32 字节)
		copy(calldata[base+64:base+96], ov.Key.Bytes())

		// value (uint256 -> 右对齐)
		if ov.Value != nil {
			ov.Value.FillBytes(calldata[base+96 : base+128])
		}

		// version (uint64 -> 右对齐)
		binary.BigEndian.PutUint64(calldata[base+152:base+160], ov.Version)
	}

	return calldata
}

// callAgentRecomputeIntent 调用 Agent.recomputeIntent，返回新的 guards/deltas（ok 为 false 时 fallback 到原始值）
func (c *joyueCoordinatorPrecompile) callAgentRecomputeIntent(evm *EVM, contract *Contract, agentAddr common.Address, req RawRequest, user common.Address, overrides []StateOverride) (guards []Guard, deltas []Delta, ok bool) {
	calldata := c.buildRecomputeIntentCalldata(req, user, overrides)
	gas := uint64(500000) // 预留足够 gas
	if contract.Gas < gas {
		gas = contract.Gas
	}
	ret, _, err := evm.Call(contract, agentAddr, calldata, gas, big.NewInt(0))
	if err != nil || len(ret) < 96 {
		utils.Logger().Warn().Err(err).
			Str("agent", agentAddr.Hex()).
			Str("user", user.Hex()).
			Str("targetAddr", req.TargetAddr.Hex()).
			Int("argsLen", len(req.Args)).
			Int("overridesCount", len(overrides)).
			Int("retLen", len(ret)).
			Msg("[JOYUE Coordinator] callAgentRecomputeIntent: call failed (Agent.recomputeIntent reverted; check: req.args format, cache precompile, gas)")
		return nil, nil, false
	}
	// 解析返回 (guards, deltas, ok): offset(32) + offset(32) + bool(32)
	guardsOffset, _ := readU256AsInt(ret, 0)
	deltasOffset, _ := readU256AsInt(ret, 32)
	okByte := ret[95] // bool 在最后字节
	if okByte == 0 {
		return nil, nil, false
	}
	guards, err = c.decodeGuards(ret, guardsOffset)
	if err != nil {
		utils.Logger().Warn().Err(err).Msg("[JOYUE Coordinator] callAgentRecomputeIntent: decode guards failed")
		return nil, nil, false
	}
	deltas, err = c.decodeDeltas(ret, deltasOffset)
	if err != nil {
		utils.Logger().Warn().Err(err).Msg("[JOYUE Coordinator] callAgentRecomputeIntent: decode deltas failed")
		return nil, nil, false
	}
	return guards, deltas, true
}

// participantKey 生成参与者在存储映射中的 key（address + shardId）
func participantKey(participant Participant) common.Hash {
	shardBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(shardBytes, participant.ShardId)
	return crypto.Keccak256Hash(participant.Addr.Bytes(), shardBytes)
}

// participantIdxMapKey 生成参与者 idxMap 的存储 key
func participantIdxMapKey(participant Participant) common.Hash {
	return crypto.Keccak256Hash(participantKey(participant).Bytes(), []byte("idxmap"))
}

// getUint 读取权威状态值（可用量 = authValue - totalFrozen，用于 guard 校验和 delta 应用）
func (c *joyueCoordinatorPrecompile) getUint(evm *EVM, addr common.Address, key common.Hash, flag int) (common.Hash, uint64) {
	// _u 在 slot 0
	uSlot := getMappingSlot(big.NewInt(storageSlotU), key)
	value := evm.StateDB.GetState(addr, uSlot)

	// _ver 在 slot 1
	verSlot := getMappingSlot(big.NewInt(storageSlotVer), key)
	verBytes := evm.StateDB.GetState(addr, verSlot)
	version := binary.BigEndian.Uint64(verBytes[24:32]) // uint64 在最后 8 字节

	// 如果 flag 为 0，直接返回底层的 authInt（原始权威值），不扣减 totalFrozen。
	if flag == 0 {
		utils.Logger().Info().
			Str("addr", addr.Hex()).
			Str("key", key.Hex()).
			Int("flag", flag).
			Str("authValue", new(big.Int).SetBytes(value.Bytes()).Text(10)). // 仅打印时转成数字
			Uint64("version", version).
			Msg("[JOYUE Coordinator] getUint: Reading raw auth state (skipped frozen deduction)")

		// value 已经是 common.Hash，直接返回！
		return value, version
	}

	authInt := new(big.Int).SetBytes(value.Bytes())
	totalFrozen := c.getTotalFrozenForKey(evm, addr, key)
	available := new(big.Int).Sub(authInt, totalFrozen)
	if available.Sign() < 0 {
		available.SetInt64(0)
	}
	availableBytes := make([]byte, 32)
	available.FillBytes(availableBytes)

	utils.Logger().Info().
		Str("addr", addr.Hex()).
		Str("key", key.Hex()).
		Str("authValue", authInt.Text(10)).
		Str("totalFrozen", totalFrozen.Text(10)).
		Str("available", available.Text(10)).
		Uint64("version", version).
		Msg("[JOYUE Coordinator] getUint: Reading state (auth - frozen)")

	return common.BytesToHash(availableBytes), version
}

// setUint 设置权威状态值
func (c *joyueCoordinatorPrecompile) setUint(evm *EVM, addr common.Address, key common.Hash, value common.Hash) {
	// _u 在 slot 0
	uSlot := getMappingSlot(big.NewInt(storageSlotU), key)
	evm.StateDB.SetState(addr, uSlot, value)

	// _ver 在 slot 1，自动增加版本号
	verSlot := getMappingSlot(big.NewInt(storageSlotVer), key)
	verBytes := evm.StateDB.GetState(addr, verSlot)
	version := binary.BigEndian.Uint64(verBytes[24:32]) + 1

	// 更新版本号（uint64 在最后 8 字节）
	newVerBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(newVerBytes[24:32], version)
	evm.StateDB.SetState(addr, verSlot, common.BytesToHash(newVerBytes))
}

// isFrozen 通过检查 _frozenDeltas 的长度来判断交易是否已冻结
// ========== Blob 存储模式实现 ==========

// blob 存储域前缀，避免不同用途的 slot 碰撞（如 ParticipantResult 与 FrozenDeltas）
const (
	blobDomainFrozenDeltas      = "frozen_deltas"
	blobDomainParticipantResult = "participant_result"
	blobDomainIdxMap            = "idx_map"
)

// getBlobSlotWithDomain 计算 blob 的起始 slot（使用 keccak256 全量 hash，无 Mod）
// domain 用于隔离不同数据类型；直接使用 256 位 hash 作为 slot，避免 Mod(10000) 导致碰撞
func getBlobSlotWithDomain(domain string, batchId, key common.Hash) common.Hash {
	combined := append([]byte(domain), batchId.Bytes()...)
	combined = append(combined, key.Bytes()...)
	hashCombined := crypto.Keccak256Hash(combined)
	return hashCombined
}

// getBlobSlot 兼容旧调用（FrozenDeltas 用 batchId+txHash）
func getBlobSlot(batchId, txHash common.Hash) common.Hash {
	return getBlobSlotWithDomain(blobDomainFrozenDeltas, batchId, txHash)
}

// isFrozen 检查是否已冻结（Blob 模式）
func (c *joyueCoordinatorPrecompile) isFrozen(evm *EVM, addr common.Address, batchId, txHash common.Hash) bool {
	baseSlot := getBlobSlot(batchId, txHash)
	firstSlot := evm.StateDB.GetState(addr, baseSlot)

	// 检查长度字段（前 4 字节）
	length := binary.BigEndian.Uint32(firstSlot[0:4])
	return length > 0
}

// setFrozenDeltasBlob 使用 Blob 模式存储冻结的 Deltas（RLP 编码）
func (c *joyueCoordinatorPrecompile) setFrozenDeltasBlob(evm *EVM, addr common.Address, batchId, txHash common.Hash, deltas []Delta) error {
	utils.Logger().Info().
		Str("addr", addr.Hex()).
		Uint32("shardID", evm.Context.ShardID).
		Str("batchId", batchId.Hex()).
		Str("txHash", txHash.Hex()).
		Int("deltasCount", len(deltas)).
		Msg("[JOYUE Coordinator] setFrozenDeltasBlob: START")

	// 使用 RLP 编码 Delta[]
	encoded, err := rlp.EncodeToBytes(deltas)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("batchId", batchId.Hex()).
			Str("txHash", txHash.Hex()).
			Msg("[JOYUE Coordinator] setFrozenDeltasBlob: RLP encoding failed")
		return err
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("txHash", txHash.Hex()).
		Int("encodedLen", len(encoded)).
		Msg("[JOYUE Coordinator] setFrozenDeltasBlob: RLP encoded successfully")

	// 计算起始 slot
	baseSlot := getBlobSlot(batchId, txHash)

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("txHash", txHash.Hex()).
		Str("baseSlot", baseSlot.Hex()).
		Msg("[JOYUE Coordinator] setFrozenDeltasBlob: Calculated base slot")

	// 编码数据：长度（4 字节）+ RLP 编码的数据
	totalData := make([]byte, 4+len(encoded))
	binary.BigEndian.PutUint32(totalData[0:4], uint32(len(encoded)))
	copy(totalData[4:], encoded)

	// 写入连续的 slot
	slotCount := (len(totalData) + 31) / 32 // 向上取整
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("txHash", txHash.Hex()).
		Int("totalDataLen", len(totalData)).
		Int("slotCount", slotCount).
		Msg("[JOYUE Coordinator] setFrozenDeltasBlob: Writing to storage slots")

	for i := 0; i < slotCount; i++ {
		slot := new(big.Int).SetBytes(baseSlot.Bytes())
		slot.Add(slot, big.NewInt(int64(i)))
		slotHash := common.BigToHash(slot)

		start := i * 32
		end := start + 32
		if end > len(totalData) {
			end = len(totalData)
		}

		var slotData common.Hash
		copy(slotData[:], totalData[start:end])
		evm.StateDB.SetState(addr, slotHash, slotData)

		utils.Logger().Info().
			Int("slotIndex", i).
			Str("slotHash", slotHash.Hex()).
			Hex("slotData", slotData[:]).
			Msg("[JOYUE Coordinator] setFrozenDeltasBlob: Wrote slot")
	}

	utils.Logger().Info().
		Str("addr", addr.Hex()).
		Str("batchId", batchId.Hex()).
		Str("txHash", txHash.Hex()).
		Int("slotCount", slotCount).
		Uint64("blockNumber", evm.BlockNumber.Uint64()).
		Msg("[JOYUE Coordinator] setFrozenDeltasBlob: COMPLETED")

	return nil
}

// getFrozenDeltasBlob 使用 Blob 模式读取冻结的 Deltas（RLP 解码）
func (c *joyueCoordinatorPrecompile) getFrozenDeltasBlob(evm *EVM, addr common.Address, batchId, txHash common.Hash) ([]Delta, error) {
	// 计算起始 slot
	baseSlot := getBlobSlot(batchId, txHash)

	// 读取第一个 slot 获取长度
	firstSlot := evm.StateDB.GetState(addr, baseSlot)
	encodedLength := binary.BigEndian.Uint32(firstSlot[0:4])

	if encodedLength == 0 {
		return nil, nil // 未设置
	}
	if encodedLength > maxBlobSize {
		return nil, fmt.Errorf("JOYUE: corrupted blob length %d exceeds max %d", encodedLength, maxBlobSize)
	}

	// 计算需要读取的 slot 数量
	totalBytes := 4 + int(encodedLength)
	slotCount := (totalBytes + 31) / 32

	// 读取所有 slot
	data := make([]byte, totalBytes)
	for i := 0; i < slotCount; i++ {
		slot := new(big.Int).SetBytes(baseSlot.Bytes())
		slot.Add(slot, big.NewInt(int64(i)))
		slotHash := common.BigToHash(slot)

		slotData := evm.StateDB.GetState(addr, slotHash)

		start := i * 32
		end := start + 32
		if end > len(data) {
			end = len(data)
		}

		copy(data[start:end], slotData[:end-start])
	}

	// 提取 RLP 编码的数据（跳过长度字段）
	encoded := data[4:]

	// RLP 解码
	var deltas []Delta
	err := rlp.DecodeBytes(encoded, &deltas)
	if err != nil {
		return nil, err
	}

	return deltas, nil
}

// clearFrozenDeltasBlob 清除 Blob 数据
func (c *joyueCoordinatorPrecompile) clearFrozenDeltasBlob(evm *EVM, addr common.Address, batchId, txHash common.Hash) {
	baseSlot := getBlobSlot(batchId, txHash)
	firstSlot := evm.StateDB.GetState(addr, baseSlot)
	encodedLength := binary.BigEndian.Uint32(firstSlot[0:4])

	if encodedLength == 0 {
		return // 已经清除
	}
	if encodedLength > maxBlobSize {
		return // 异常 length，跳过清除避免死循环
	}

	// 计算需要清除的 slot 数量
	totalBytes := 4 + int(encodedLength)
	slotCount := (totalBytes + 31) / 32

	// 清除所有 slot
	for i := 0; i < slotCount; i++ {
		slot := new(big.Int).SetBytes(baseSlot.Bytes())
		slot.Add(slot, big.NewInt(int64(i)))
		slotHash := common.BigToHash(slot)
		evm.StateDB.SetState(addr, slotHash, common.Hash{})
	}
}

// emitEvent 发出事件
func (c *joyueCoordinatorPrecompile) emitEvent(evm *EVM, contract *Contract, eventSig common.Hash, topics []common.Hash, data []byte) {
	evm.StateDB.AddLog(&types.Log{
		Address:     contract.Address(), // JoyueCoordinator 的地址
		Topics:      append([]common.Hash{eventSig}, topics...),
		Data:        data,
		BlockNumber: evm.BlockNumber.Uint64(),
	})
}

// completionType 用于 AgentResultEmitted 事件（交易结束类型）
const (
	CompletionAttempt1DirectSuccess = 1 // attempt1 直接成功
	CompletionAttempt1DirectFail    = 2 // attempt1 直接失败
	CompletionAttempt1RetryFail     = 3 // attempt1 重试，attempt1 中重试失败
	CompletionAttempt2Success       = 4 // attempt2 成功
	CompletionAttempt2Fail          = 5 // attempt2 失败
)

// agentResultEmittedEventSig AgentResultEmitted(bytes32 indexed txHash, bool success, uint8 completionType)
var agentResultEmittedEventSig = crypto.Keccak256Hash([]byte("AgentResultEmitted(bytes32,bool,uint8)"))

// stateBroadcastEventSig StateBroadcast(address indexed contractAddr, bytes32 indexed key, uint256 value, uint64 version, bytes32 txId)
var stateBroadcastEventSig = crypto.Keccak256Hash([]byte("StateBroadcast(address,bytes32,uint256,uint64,bytes32)"))

// stateUnfreezeEventSig StateUnfreeze(address indexed contractAddr, bytes32 indexed key, bytes32 txId)
var stateUnfreezeEventSig = crypto.Keccak256Hash([]byte("StateUnfreeze(address,bytes32,bytes32)"))

// emitStateBroadcast 发出 StateBroadcast 事件，通知缓存系统更新权威缓存并解冻 txId。
// 事件签名：StateBroadcast(address indexed contractAddr, bytes32 indexed key, uint256 value, uint64 version, bytes32 txId)
// Data 布局（96 bytes）：[0:32]=value, [32:64]=version(右对齐 uint64), [64:96]=txId
func (c *joyueCoordinatorPrecompile) emitStateBroadcast(evm *EVM, coordinatorAddr common.Address, key common.Hash, txId common.Hash) {
	newVal, newVer := c.getUint(evm, coordinatorAddr, key, 0)

	contractAddrTopic := common.BytesToHash(coordinatorAddr.Bytes())

	data := make([]byte, 96)
	copy(data[0:32], newVal.Bytes())
	binary.BigEndian.PutUint64(data[56:64], newVer)
	copy(data[64:96], txId.Bytes())

	evm.StateDB.AddLog(&types.Log{
		Address: coordinatorAddr,
		Topics: []common.Hash{
			stateBroadcastEventSig,
			contractAddrTopic,
			key,
		},
		Data:        data,
		BlockNumber: evm.BlockNumber.Uint64(),
	})

	utils.Logger().Info().
		Str("contract", coordinatorAddr.Hex()).
		Str("key", key.Hex()).
		Str("value", new(big.Int).SetBytes(newVal.Bytes()).Text(10)).
		Uint64("version", newVer).
		Str("txId", txId.Hex()).
		Msg("[JOYUE Coordinator] emitStateBroadcast: emitted")
}

// emitStateUnfreeze 发出 StateUnfreeze 事件，通知缓存系统仅解冻 txId（不更新权威缓存）。
// 事件签名：StateUnfreeze(address indexed contractAddr, bytes32 indexed key, bytes32 txId)
// Data 布局（32 bytes）：[0:32]=txId
func (c *joyueCoordinatorPrecompile) emitStateUnfreeze(evm *EVM, coordinatorAddr common.Address, key common.Hash, txId common.Hash) {
	contractAddrTopic := common.BytesToHash(coordinatorAddr.Bytes())

	data := make([]byte, 32)
	copy(data[0:32], txId.Bytes())

	evm.StateDB.AddLog(&types.Log{
		Address: coordinatorAddr,
		Topics: []common.Hash{
			stateUnfreezeEventSig,
			contractAddrTopic,
			key,
		},
		Data:        data,
		BlockNumber: evm.BlockNumber.Uint64(),
	})

	utils.Logger().Info().
		Str("contract", coordinatorAddr.Hex()).
		Str("key", key.Hex()).
		Str("txId", txId.Hex()).
		Msg("[JOYUE Coordinator] emitStateUnfreeze: emitted")
}

// ============================================================
// ABI 编码/解码辅助函数
// ============================================================

// 注意：由于 Solidity 的 ABI 编码比较复杂，这里先提供基础框架
// 完整的实现需要使用 go-ethereum 的 abi 包，并定义对应的 Go 结构体

// Guard 结构体（对应 Solidity 的 JoyueLib.Guard）
type Guard struct {
	ContractAddr common.Address
	ShardId      uint32
	Key          common.Hash
	ReadVersion  uint64
	Strategy     uint8
	Op           uint8
	Val          []byte
}

// Delta 结构体（对应 Solidity 的 JoyueLib.Delta）
type Delta struct {
	ContractAddr common.Address
	ShardId      uint32
	Key          common.Hash
	Op           uint8
	Val          []byte
}

// ColumnGuard 结构体（对应 Solidity 的 ColumnGuard）
type ColumnGuard struct {
	GuardIndex uint32
	Guard      Guard
}

// ColumnTx 结构体（对应 Solidity 的 ColumnTx）
type ColumnTx struct {
	TxHash common.Hash
	Guards []ColumnGuard
	Deltas []Delta
}

// ColumnResult 结构体（对应 Solidity 的 ColumnResult）
type ColumnResult struct {
	TxHash           common.Hash
	Ok               bool
	GuardFailed      bool
	FailedGuardIndex uint32
	FailedDeltaIndex uint32
	LatestKey        common.Hash
	LatestVal        *big.Int
	LatestVer        uint64
}

// RawRequest 结构体（对应 Solidity 的 JoyueLib.RawRequest）
type RawRequest struct {
	TargetAddr common.Address
	Selector   [4]byte
	Args       []byte
}

// StateOverride callback 返回的权威最新状态，用于 recomputeIntent 优先于缓存
type StateOverride struct {
	ContractAddr common.Address
	ShardId      uint32
	Key          common.Hash
	Value        *big.Int
	Version      uint64
}

// IntentDetail 结构体（对应 Solidity 的 IntentDetail）
type IntentDetail struct {
	TxHash common.Hash
	Sender common.Address
	Nonce  uint64
	Req    RawRequest
	Guards []Guard
	Deltas []Delta
	User   common.Address // 发起 Intent 的用户（用于 retry 时 recomputeIntent）
}

// IntentBundle 结构体（对应 Solidity 的 IntentBundle）
type IntentBundle struct {
	BundleId         common.Hash
	AgentShardId     uint32
	MasterShardId    uint32
	AgentContract    common.Address
	MasterContract   common.Address
	Epoch            uint64
	AgentBlockNumber uint64
	TimestampMs      uint64
	Detail           []IntentDetail
}

// Participant 参与者（地址 + 分片）
type Participant struct {
	Addr    common.Address
	ShardId uint32
}

// FinalResult 结构体（对应 Solidity 的 FinalResult）
type FinalResult struct {
	TxHash       common.Hash
	Status       uint8 // 0 = FINAL_COMMIT, 1 = FINAL_FAIL
	AttemptsUsed uint8
}

// handleBatchVerifyAndFreeze 处理 batchVerifyAndFreeze 函数调用
func (c *joyueCoordinatorPrecompile) handleBatchVerifyAndFreeze(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Str("caller", contract.CallerAddress.Hex()).
		Int("paramsLen", len(params)).
		Msg("[JOYUE Coordinator] handleBatchVerifyAndFreeze: CALLED - Message received!")

	// 解析参数：batchId (bytes32) + ColumnTx[] (动态数组)
	if len(params) < 64 {
		utils.Logger().Error().
			Int("paramsLen", len(params)).
			Msg("[JOYUE Coordinator] handleBatchVerifyAndFreeze: invalid params length")
		return nil, errors.New("JOYUE: invalid params length")
	}

	// batchId 在前 32 字节
	batchId := common.BytesToHash(params[0:32])

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Hex("params0-32", params[0:32]).
		Hex("params32-64", params[32:64]).
		Msg("[JOYUE Coordinator] handleBatchVerifyAndFreeze: Parsed batchId")

	// ColumnTx[] 是动态数组，需要解析偏移量（ABI 32 字节槽右对齐，用 readU256AsUint64）
	// 偏移量在 32-64 字节
	txsOffsetRaw, err := readU256AsUint64(params, 32)
	if err != nil {
		return nil, err
	}
	if txsOffsetRaw < 64 || int(txsOffsetRaw) >= len(params) {
		return nil, errors.New("JOYUE: invalid array offset")
	}

	// 解析数组长度（ABI 32 字节槽右对齐）
	arrayOffset := int(txsOffsetRaw)
	if arrayOffset+32 > len(params) {
		return nil, errors.New("JOYUE: invalid array length offset")
	}
	arrayLen, err := readU256AsUint64(params, arrayOffset)
	if err != nil {
		return nil, err
	}

	// 解析每个 ColumnTx
	txs := make([]ColumnTx, 0, arrayLen)
	for i := uint64(0); i < arrayLen; i++ {
		// 每个元素是动态类型，需要解析偏移量
		elementOffset := arrayOffset + 32 + int(i*32)
		if elementOffset+32 > len(params) {
			return nil, errors.New("JOYUE: invalid element offset")
		}
		// 动态数组元素为动态 tuple，因此数组里存的是相对 array head 的 element pointer
		elementPtrRel, err := readU256AsInt(params, elementOffset)
		if err != nil {
			return nil, err
		}
		// 透传编码：element offset 相对于 array 起始位置（包含 length）
		elementPtr := arrayOffset + elementPtrRel

		// 解析 ColumnTx 结构
		tx, err := c.decodeColumnTxPassthrough(params, elementPtr)
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Int("txsCount", len(txs)).
		Msg("[JOYUE Coordinator] handleBatchVerifyAndFreeze: Parsed all ColumnTxs - Returning mock success results")

	//执行验证和冻结逻辑
	results, err := c.executeBatchVerifyAndFreeze(evm, contract, coordinatorAddr, batchId, txs)
	if err != nil {
		return nil, err
	}

	// 编码返回结果
	return c.encodeColumnResults(results)
}

// decodeColumnTxArrayBody 从 encodeColumnTxsForCall 输出的 array body 解析出 []ColumnTx
// body 格式：[length(32)][element offset 0 (32)]...[element offset N-1 (32)][element data...]
// 所有偏移量相对于 body 起始位置（即 length word 所在的位置）
func (c *joyueCoordinatorPrecompile) decodeColumnTxArrayBody(body []byte) ([]ColumnTx, error) {
	if len(body) < 32 {
		return nil, errors.New("JOYUE: decodeColumnTxArrayBody too short")
	}
	arrayLen, err := readU256AsUint64(body, 0)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: decodeColumnTxArrayBody length: %w", err)
	}
	txs := make([]ColumnTx, 0, arrayLen)
	for i := uint64(0); i < arrayLen; i++ {
		elementOffset := 32 + int(i*32)
		if elementOffset+32 > len(body) {
			return nil, errors.New("JOYUE: decodeColumnTxArrayBody element offset out of range")
		}
		elementPtrRel, err := readU256AsInt(body, elementOffset)
		if err != nil {
			return nil, fmt.Errorf("JOYUE: decodeColumnTxArrayBody element ptr: %w", err)
		}
		// 透传编码：offset 相对于 array 起始（length word 所在位置）
		elementPtr := elementPtrRel
		tx, err := c.decodeColumnTxPassthrough(body, elementPtr)
		if err != nil {
			return nil, fmt.Errorf("JOYUE: decodeColumnTxArrayBody element %d: %w", i, err)
		}
		txs = append(txs, tx)
	}
	return txs, nil
}

// decodeColumnTx 解析单个 ColumnTx 结构
func (c *joyueCoordinatorPrecompile) decodeColumnTx(data []byte, offset int) (ColumnTx, error) {
	var tx ColumnTx

	if offset+96 > len(data) {
		return tx, errors.New("JOYUE: invalid ColumnTx offset")
	}

	// txHash (bytes32) - 前 32 字节
	tx.TxHash = common.BytesToHash(data[offset : offset+32])

	// guards (ColumnGuard[]) - 动态数组，偏移量在 offset+32
	// 注意：tuple 内部的偏移量是相对于 tuple 起始位置（offset）的
	guardsOffsetRel, err := readU256AsInt(data, offset+32)
	if err != nil {
		return tx, err
	}
	guardsOffset := offset + guardsOffsetRel
	guards, err := c.decodeColumnGuards(data, guardsOffset)
	if err != nil {
		return tx, err
	}
	tx.Guards = guards

	// deltas (Delta[]) - 动态数组，偏移量在 offset+64
	// 注意：tuple 内部的偏移量是相对于 tuple 起始位置（offset）的
	deltasOffsetRel, err := readU256AsInt(data, offset+64)
	if err != nil {
		return tx, err
	}
	deltasOffset := offset + deltasOffsetRel
	deltas, err := c.decodeDeltas(data, deltasOffset)
	if err != nil {
		return tx, err
	}
	tx.Deltas = deltas

	return tx, nil
}

// decodeColumnTxPassthrough 解析单个 ColumnTx 结构（透传编码：array offset 基于 array 起始位置）
func (c *joyueCoordinatorPrecompile) decodeColumnTxPassthrough(data []byte, offset int) (ColumnTx, error) {
	var tx ColumnTx

	if offset+96 > len(data) {
		return tx, errors.New("JOYUE: invalid ColumnTx offset")
	}

	// txHash (bytes32) - 前 32 字节
	tx.TxHash = common.BytesToHash(data[offset : offset+32])

	// guards (ColumnGuard[]) - 动态数组，偏移量在 offset+32
	guardsOffsetRel, err := readU256AsInt(data, offset+32)
	if err != nil {
		return tx, err
	}
	guardsOffset := offset + guardsOffsetRel
	guards, err := c.decodeColumnGuardsPassthrough(data, guardsOffset)
	if err != nil {
		return tx, err
	}
	tx.Guards = guards

	// deltas (Delta[]) - 动态数组，偏移量在 offset+64
	deltasOffsetRel, err := readU256AsInt(data, offset+64)
	if err != nil {
		return tx, err
	}
	deltasOffset := offset + deltasOffsetRel
	deltas, err := c.decodeDeltasPassthrough(data, deltasOffset)
	if err != nil {
		return tx, err
	}
	tx.Deltas = deltas

	return tx, nil
}

// decodeColumnGuards 解析 ColumnGuard[] 数组
func (c *joyueCoordinatorPrecompile) decodeColumnGuards(data []byte, offset int) ([]ColumnGuard, error) {
	if offset+32 > len(data) {
		return nil, errors.New("JOYUE: invalid guards array offset")
	}

	arrayLen, err := readU256AsUint64(data, offset)
	if err != nil {
		return nil, err
	}
	guards := make([]ColumnGuard, 0, arrayLen)

	for i := uint64(0); i < arrayLen; i++ {
		elementOffset := offset + 32 + int(i*32)
		if elementOffset+32 > len(data) {
			return nil, errors.New("JOYUE: invalid guard element offset")
		}
		// 动态数组元素为动态 tuple，因此数组里存的是相对 array head 的 element pointer
		elementPtrRel, err := readU256AsInt(data, elementOffset)
		if err != nil {
			return nil, err
		}
		// 注意：array element offset 相对于 array head（紧跟 length 的位置，即 offset+32）
		elementPtr := (offset + 32) + elementPtrRel

		// 解析 ColumnGuard 结构
		var cg ColumnGuard
		if elementPtr+96 > len(data) {
			return nil, errors.New("JOYUE: invalid ColumnGuard offset")
		}

		// guardIndex (uint32) - 前 32 字节（左填充）
		cg.GuardIndex = binary.BigEndian.Uint32(data[elementPtr+28 : elementPtr+32])

		// guard (Guard) - 动态结构，偏移量在 elementPtr+32
		guardOffsetRel, err := readU256AsInt(data, elementPtr+32)
		if err != nil {
			return nil, err
		}
		// Guard 结构内部的偏移量是相对于 ColumnGuard 结构起始位置的
		guardOffset := elementPtr + guardOffsetRel
		guard, err := c.decodeGuard(data, guardOffset)
		if err != nil {
			return nil, err
		}
		cg.Guard = guard

		guards = append(guards, cg)
	}

	return guards, nil
}

// decodeColumnGuardsPassthrough 解析 ColumnGuard[] 数组（透传编码：array offset 基于 array 起始位置）
func (c *joyueCoordinatorPrecompile) decodeColumnGuardsPassthrough(data []byte, offset int) ([]ColumnGuard, error) {
	if offset+32 > len(data) {
		return nil, errors.New("JOYUE: invalid guards array offset")
	}

	arrayLen, err := readU256AsUint64(data, offset)
	if err != nil {
		return nil, err
	}
	guards := make([]ColumnGuard, 0, arrayLen)

	for i := uint64(0); i < arrayLen; i++ {
		elementOffset := offset + 32 + int(i*32)
		if elementOffset+32 > len(data) {
			return nil, errors.New("JOYUE: invalid guard element offset")
		}
		elementPtrRel, err := readU256AsInt(data, elementOffset)
		if err != nil {
			return nil, err
		}
		// 透传编码：array element offset 相对于 array 起始位置（包含 length）
		elementPtr := offset + elementPtrRel

		var cg ColumnGuard
		if elementPtr+96 > len(data) {
			return nil, errors.New("JOYUE: invalid ColumnGuard offset")
		}

		// guardIndex (uint32) - 前 32 字节（左填充）
		cg.GuardIndex = binary.BigEndian.Uint32(data[elementPtr+28 : elementPtr+32])

		// guard (Guard) - 动态结构，偏移量在 elementPtr+32
		guardOffsetRel, err := readU256AsInt(data, elementPtr+32)
		if err != nil {
			return nil, err
		}
		guardOffset := elementPtr + guardOffsetRel
		guard, err := c.decodeGuard(data, guardOffset)
		if err != nil {
			return nil, err
		}
		cg.Guard = guard

		guards = append(guards, cg)
	}

	return guards, nil
}

// decodeGuard 解析 Guard 结构
func (c *joyueCoordinatorPrecompile) decodeGuard(data []byte, offset int) (Guard, error) {
	var guard Guard

	if offset+224 > len(data) {
		return guard, errors.New("JOYUE: invalid Guard offset")
	}

	// contractAddr (address) - 前 32 字节（右填充）
	guard.ContractAddr = common.BytesToAddress(data[offset+12 : offset+32])

	// shardId (uint32) - 32-64 字节（左填充，取最后 4 字节）
	guard.ShardId = binary.BigEndian.Uint32(data[offset+60 : offset+64])

	// key (bytes32) - 64-96 字节
	guard.Key = common.BytesToHash(data[offset+64 : offset+96])

	// readVersion (uint64) - 96-128 字节（左填充）
	guard.ReadVersion = binary.BigEndian.Uint64(data[offset+120 : offset+128])

	// strategy (uint8) - 128-160 字节（左填充）
	guard.Strategy = data[offset+159]

	// op (uint8) - 160-192 字节（左填充）
	guard.Op = data[offset+191]

	// val (bytes) - 动态类型，偏移量在 192-224 字节
	// 注意：tuple 内部的 offset 是相对于 tuple 起始位置（offset）
	valOffsetRel, err := readU256AsInt(data, offset+192)
	if err != nil {
		return guard, err
	}
	valOffset := offset + valOffsetRel
	if valOffset+32 > len(data) {
		return guard, errors.New("JOYUE: invalid val offset")
	}
	valLen, err := readU256AsUint64(data, valOffset)
	if err != nil {
		return guard, err
	}
	if valOffset+32+int(valLen) > len(data) {
		return guard, errors.New("JOYUE: invalid val length")
	}
	guard.Val = make([]byte, valLen)
	copy(guard.Val, data[valOffset+32:valOffset+32+int(valLen)])

	return guard, nil
}

// decodeDeltas 解析 Delta[] 数组
func (c *joyueCoordinatorPrecompile) decodeDeltas(data []byte, offset int) ([]Delta, error) {
	if offset+32 > len(data) {
		return nil, errors.New("JOYUE: invalid deltas array offset")
	}

	arrayLen, err := readU256AsUint64(data, offset)
	if err != nil {
		return nil, err
	}
	deltas := make([]Delta, 0, arrayLen)

	for i := uint64(0); i < arrayLen; i++ {
		elementOffset := offset + 32 + int(i*32)
		if elementOffset+32 > len(data) {
			return nil, errors.New("JOYUE: invalid delta element offset")
		}
		// 动态数组元素为动态 tuple，因此数组里存的是相对 offset 的 element pointer
		elementPtrRel, err := readU256AsInt(data, elementOffset)
		if err != nil {
			return nil, err
		}
		// 注意：array element offset 相对于 array head（紧跟 length 的位置，即 offset+32）
		elementPtr := (offset + 32) + elementPtrRel

		// 解析 Delta 结构
		delta, err := c.decodeDelta(data, elementPtr)
		if err != nil {
			return nil, err
		}
		deltas = append(deltas, delta)
	}

	return deltas, nil
}

// decodeDeltasPassthrough 解析 Delta[] 数组（透传编码：array offset 基于 array 起始位置）
func (c *joyueCoordinatorPrecompile) decodeDeltasPassthrough(data []byte, offset int) ([]Delta, error) {
	if offset+32 > len(data) {
		return nil, errors.New("JOYUE: invalid deltas array offset")
	}

	arrayLen, err := readU256AsUint64(data, offset)
	if err != nil {
		return nil, err
	}
	deltas := make([]Delta, 0, arrayLen)

	for i := uint64(0); i < arrayLen; i++ {
		elementOffset := offset + 32 + int(i*32)
		if elementOffset+32 > len(data) {
			return nil, errors.New("JOYUE: invalid delta element offset")
		}
		elementPtrRel, err := readU256AsInt(data, elementOffset)
		if err != nil {
			return nil, err
		}
		// 透传编码：array element offset 相对于 array 起始位置（包含 length）
		elementPtr := offset + elementPtrRel

		delta, err := c.decodeDelta(data, elementPtr)
		if err != nil {
			return nil, err
		}
		deltas = append(deltas, delta)
	}

	return deltas, nil
}

// decodeDelta 解析 Delta 结构
func (c *joyueCoordinatorPrecompile) decodeDelta(data []byte, offset int) (Delta, error) {
	var delta Delta

	if offset+192 > len(data) {
		return delta, errors.New("JOYUE: invalid Delta offset")
	}

	// contractAddr (address) - 前 32 字节（右填充）
	delta.ContractAddr = common.BytesToAddress(data[offset+12 : offset+32])

	// shardId (uint32) - 32-64 字节（左填充，取最后 4 字节）
	delta.ShardId = binary.BigEndian.Uint32(data[offset+60 : offset+64])

	// key (bytes32) - 64-96 字节
	delta.Key = common.BytesToHash(data[offset+64 : offset+96])

	// op (uint8) - 96-128 字节（左填充）
	delta.Op = data[offset+127]

	// val (bytes) - 动态类型，偏移量在 128-160 字节
	// 注意：tuple 内部的 offset 是相对于 tuple 起始位置（offset）
	valOffsetRel, err := readU256AsInt(data, offset+128)
	if err != nil {
		return delta, err
	}
	valOffset := offset + valOffsetRel
	if valOffset+32 > len(data) {
		return delta, errors.New("JOYUE: invalid delta val offset")
	}
	valLen, err := readU256AsUint64(data, valOffset)
	if err != nil {
		return delta, err
	}
	if valOffset+32+int(valLen) > len(data) {
		return delta, errors.New("JOYUE: invalid delta val length")
	}
	delta.Val = make([]byte, valLen)
	copy(delta.Val, data[valOffset+32:valOffset+32+int(valLen)])

	return delta, nil
}

// executeBatchVerifyAndFreeze 执行批量验证和冻结
// 优化：使用 Go 内存存储瞬时状态，而不是 Storage
// 瞬时状态只在函数执行期间存在，用于累积同一批次内多个交易的 Deltas
func (c *joyueCoordinatorPrecompile) executeBatchVerifyAndFreeze(evm *EVM, contract *Contract, coordinatorAddr common.Address, batchId common.Hash, txs []ColumnTx) ([]ColumnResult, error) {
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Uint32("shardID", evm.Context.ShardID).
		Uint64("blockNumber", evm.BlockNumber.Uint64()).
		Str("txHash", evm.StateDB.TxHash().Hex()).
		Int("txsCount", len(txs)).
		Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: START")

	results := make([]ColumnResult, len(txs))

	// 使用 Go 内存存储瞬时状态（只在函数执行期间存在）
	// key -> value (累积后的值)
	transientState := make(map[common.Hash]*big.Int)

	// 初始化瞬时状态：从权威状态读取初始值
	initTransientState := func(key common.Hash) {
		if _, exists := transientState[key]; !exists {
			authVal, _ := c.getUint(evm, coordinatorAddr, key, 1)
			authValInt := new(big.Int).SetBytes(authVal.Bytes())
			transientState[key] = authValInt
			utils.Logger().Info().
				Str("key", key.Hex()).
				Str("authVal", authValInt.Text(10)).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Initialized transient state from authoritative")
		}
	}

	for i, tx := range txs {
		utils.Logger().Info().
			Int("txIndex", i).
			Str("txHash", tx.TxHash.Hex()).
			Int("guardsCount", len(tx.Guards)).
			Int("deltasCount", len(tx.Deltas)).
			Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Processing ColumnTx")

		result := ColumnResult{
			TxHash: tx.TxHash,
		}

		// 1) 验证 Guards
		guardOk := true
		for _, cg := range tx.Guards {
			// 解析 Guard.Val 用于日志
			var guardValStr string
			var guardValHex string
			if len(cg.Guard.Val) >= 32 {
				guardValInt := new(big.Int).SetBytes(cg.Guard.Val[:32])
				guardValStr = guardValInt.Text(10)
				guardValHex = fmt.Sprintf("0x%064x", guardValInt)
			} else {
				guardValStr = "invalid"
				guardValHex = fmt.Sprintf("0x%x", cg.Guard.Val)
			}
			utils.Logger().Info().
				Int("txIndex", i).
				Uint32("guardIndex", cg.GuardIndex).
				Str("guardKey", cg.Guard.Key.Hex()).
				Str("guardContract", cg.Guard.ContractAddr.Hex()).
				Uint64("readVersion", cg.Guard.ReadVersion).
				Uint8("strategy", cg.Guard.Strategy).
				Uint8("op", cg.Guard.Op).
				Str("guardVal", guardValStr).
				Str("guardValHex", guardValHex).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Verifying Guard")

			ok, curVal, curVer := c.verifyAtomicGuard(evm, coordinatorAddr, cg.Guard)
			curValInt := new(big.Int).SetBytes(curVal.Bytes())
			if !ok {
				guardOk = false
				result.Ok = false
				result.GuardFailed = true
				result.FailedGuardIndex = cg.GuardIndex
				result.LatestKey = cg.Guard.Key
				result.LatestVal = new(big.Int).Set(curValInt)
				result.LatestVer = curVer
				utils.Logger().Warn().
					Int("txIndex", i).
					Uint32("guardIndex", cg.GuardIndex).
					Str("guardKey", cg.Guard.Key.Hex()).
					Str("curVal", curValInt.Text(10)).
					Uint64("curVer", curVer).
					Uint64("readVersion", cg.Guard.ReadVersion).
					Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Guard verification FAILED")
				break
			}
			utils.Logger().Info().
				Int("txIndex", i).
				Uint32("guardIndex", cg.GuardIndex).
				Str("guardKey", cg.Guard.Key.Hex()).
				Str("curVal", curValInt.Text(10)).
				Uint64("curVer", curVer).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Guard verification PASSED")
		}

		if !guardOk {
			results[i] = result
			utils.Logger().Warn().
				Int("txIndex", i).
				Str("txHash", tx.TxHash.Hex()).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: ColumnTx FAILED at Guard verification")
			continue
		}

		// 2) 冻结 Deltas 到瞬时状态（使用内存）
		deltaOk := true
		for di, delta := range tx.Deltas {
			utils.Logger().Info().
				Int("txIndex", i).
				Int("deltaIndex", di).
				Str("deltaKey", delta.Key.Hex()).
				Str("deltaContract", delta.ContractAddr.Hex()).
				Uint8("deltaOp", delta.Op).
				Hex("deltaVal", delta.Val).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Processing Delta")

			if delta.ContractAddr != coordinatorAddr {
				deltaOk = false
				result.FailedDeltaIndex = uint32(di)
				utils.Logger().Warn().
					Int("txIndex", i).
					Int("deltaIndex", di).
					Str("deltaContract", delta.ContractAddr.Hex()).
					Str("coordinatorAddr", coordinatorAddr.Hex()).
					Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Delta contract address mismatch")
				break
			}

			// 初始化瞬时状态（如果不存在）
			initTransientState(delta.Key)
			curT := transientState[delta.Key]

			utils.Logger().Info().
				Int("txIndex", i).
				Int("deltaIndex", di).
				Str("deltaKey", delta.Key.Hex()).
				Str("curTransientVal", curT.Text(10)).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Current transient state value")

			// 应用 Delta 操作到内存中的瞬时状态
			newVal, ok := c.applyDeltaToValueInMemory(curT, delta)
			if !ok {
				deltaOk = false
				result.FailedDeltaIndex = uint32(di)
				result.LatestKey = delta.Key
				result.LatestVal = new(big.Int).Set(curT)
				authVal, authVer := c.getUint(evm, coordinatorAddr, delta.Key, 1)
				authValInt := new(big.Int).SetBytes(authVal.Bytes())
				result.LatestVer = authVer
				utils.Logger().Warn().
					Int("txIndex", i).
					Int("deltaIndex", di).
					Str("deltaKey", delta.Key.Hex()).
					Str("curTransientVal", curT.Text(10)).
					Str("authVal", authValInt.Text(10)).
					Uint64("authVer", authVer).
					Uint8("deltaOp", delta.Op).
					Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Delta application FAILED")
				break
			}

			// 更新内存中的瞬时状态
			transientState[delta.Key] = newVal
			utils.Logger().Info().
				Int("txIndex", i).
				Int("deltaIndex", di).
				Str("deltaKey", delta.Key.Hex()).
				Str("curTransientVal", curT.Text(10)).
				Str("newTransientVal", newVal.Text(10)).
				Uint8("deltaOp", delta.Op).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Delta applied to transient state")
		}

		if !deltaOk {
			result.Ok = false
			result.GuardFailed = false
			results[i] = result
			utils.Logger().Warn().
				Int("txIndex", i).
				Str("txHash", tx.TxHash.Hex()).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: ColumnTx FAILED at Delta application")
			continue
		}

		// 3) 标记冻结成功：保存冻结，并对 SUB 做"真实减库存"占用
		utils.Logger().Info().
			Int("txIndex", i).
			Str("txHash", tx.TxHash.Hex()).
			Int("deltasCount", len(tx.Deltas)).
			Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Applying authoritative freeze")
		c.applyAuthoritativeFreeze(evm, coordinatorAddr, tx.TxHash, tx.Deltas)

		// 保存 _frozenDeltas（使用 Blob 模式 + RLP 编码）
		if len(tx.Deltas) > 0 {
			utils.Logger().Info().
				Int("txIndex", i).
				Str("txHash", tx.TxHash.Hex()).
				Str("batchId", batchId.Hex()).
				Int("deltasCount", len(tx.Deltas)).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Saving frozen Deltas to blob storage")
			err := c.setFrozenDeltasBlob(evm, coordinatorAddr, batchId, tx.TxHash, tx.Deltas)
			if err != nil {
				// 如果编码失败，标记为失败
				deltaOk = false
				result.FailedDeltaIndex = uint32(len(tx.Deltas))
				result.Ok = false
				result.GuardFailed = false
				results[i] = result
				utils.Logger().Error().
					Err(err).
					Int("txIndex", i).
					Str("txHash", tx.TxHash.Hex()).
					Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Failed to save frozen Deltas")
				continue
			}
			utils.Logger().Info().
				Int("txIndex", i).
				Str("txHash", tx.TxHash.Hex()).
				Str("batchId", batchId.Hex()).
				Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: Frozen Deltas saved successfully")
		}

		result.Ok = true
		results[i] = result
		utils.Logger().Info().
			Int("txIndex", i).
			Str("txHash", tx.TxHash.Hex()).
			Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: ColumnTx SUCCESS")
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Int("txsCount", len(txs)).
		Int("successCount", func() int {
			count := 0
			for _, r := range results {
				if r.Ok {
					count++
				}
			}
			return count
		}()).
		Msg("[JOYUE Coordinator] executeBatchVerifyAndFreeze: COMPLETED")

	// 函数执行完毕，transientState 自动释放（Go GC）
	return results, nil
}

// applyDeltaToValueInMemory 将 Delta 应用到内存中的值（不写回 storage）
func (c *joyueCoordinatorPrecompile) applyDeltaToValueInMemory(curVal *big.Int, delta Delta) (*big.Int, bool) {
	// 解析 Delta 值
	if len(delta.Val) < 32 {
		utils.Logger().Warn().
			Str("deltaKey", delta.Key.Hex()).
			Uint8("deltaOp", delta.Op).
			Int("deltaValLen", len(delta.Val)).
			Msg("[JOYUE Coordinator] applyDeltaToValueInMemory: Delta value too short")
		return nil, false
	}
	deltaVal := new(big.Int).SetBytes(delta.Val[:32])
	curInt := new(big.Int).Set(curVal) // 复制，避免修改原值

	var newInt *big.Int
	var opName string
	switch delta.Op {
	case D_ADD:
		newInt = new(big.Int).Add(curInt, deltaVal)
		opName = "ADD"
	case D_SUB:
		if curInt.Cmp(deltaVal) < 0 {
			utils.Logger().Warn().
				Str("deltaKey", delta.Key.Hex()).
				Str("curVal", curInt.Text(10)).
				Str("deltaVal", deltaVal.Text(10)).
				Msg("[JOYUE Coordinator] applyDeltaToValueInMemory: SUB underflow")
			return nil, false // 下溢
		}
		newInt = new(big.Int).Sub(curInt, deltaVal)
		opName = "SUB"
	case D_SET:
		newInt = deltaVal
		opName = "SET"
	case D_BIT_OR:
		newInt = new(big.Int).Or(curInt, deltaVal)
		opName = "BIT_OR"
	case D_BIT_CLEAR:
		notDeltaVal := new(big.Int).Not(deltaVal)
		newInt = new(big.Int).And(curInt, notDeltaVal)
		opName = "BIT_CLEAR"
	case D_BIT_XOR:
		newInt = new(big.Int).Xor(curInt, deltaVal)
		opName = "BIT_XOR"
	default:
		utils.Logger().Warn().
			Str("deltaKey", delta.Key.Hex()).
			Uint8("deltaOp", delta.Op).
			Msg("[JOYUE Coordinator] applyDeltaToValueInMemory: Unknown delta operation")
		return nil, false
	}

	utils.Logger().Info().
		Str("deltaKey", delta.Key.Hex()).
		Str("op", opName).
		Str("curVal", curInt.Text(10)).
		Str("deltaVal", deltaVal.Text(10)).
		Str("newVal", newInt.Text(10)).
		Msg("[JOYUE Coordinator] applyDeltaToValueInMemory: Delta applied successfully")

	return newInt, true
}

// verifyAtomicGuard 验证原子 Guard
// 注意：coordinatorAddr 是调用 batchVerifyAndFreeze 的合约地址（可能是主合约或参与者合约）
// Guard.ContractAddr 应该等于 coordinatorAddr，表示该 Guard 属于该合约
func (c *joyueCoordinatorPrecompile) verifyAtomicGuard(evm *EVM, coordinatorAddr common.Address, guard Guard) (bool, common.Hash, uint64) {
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Str("guardContractAddr", guard.ContractAddr.Hex()).
		Str("guardKey", guard.Key.Hex()).
		Msg("[JOYUE Coordinator] verifyAtomicGuard: Starting verification")

	// 检查合约地址与分片：Guard 必须属于当前合约与当前分片
	if guard.ContractAddr != coordinatorAddr || guard.ShardId != evm.Context.ShardID {
		utils.Logger().Warn().
			Str("coordinatorAddr", coordinatorAddr.Hex()).
			Str("guardContractAddr", guard.ContractAddr.Hex()).
			Uint32("guardShardId", guard.ShardId).
			Uint32("localShardId", evm.Context.ShardID).
			Msg("[JOYUE Coordinator] verifyAtomicGuard: Contract address mismatch")
		return false, common.Hash{}, 0
	}

	// 表达式 Guard（暂时不支持，返回 false）
	if guard.Op == OP_EXPR {
		utils.Logger().Warn().
			Str("guardKey", guard.Key.Hex()).
			Msg("[JOYUE Coordinator] verifyAtomicGuard: Expression Guard not supported")
		return false, common.Hash{}, 0
	}

	// 读取当前状态值（从 coordinatorAddr 合约读取）
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Str("guardKey", guard.Key.Hex()).
		Msg("[JOYUE Coordinator] verifyAtomicGuard: Reading state from coordinator contract")
	curVal, curVer := c.getUint(evm, coordinatorAddr, guard.Key, 1)

	// 版本策略检查
	if guard.Strategy == STRATEGY_STRICT {
		// 严格模式：版本必须完全匹配
		if curVer != guard.ReadVersion {
			return false, curVal, curVer
		}
	} else if guard.Strategy == STRATEGY_RELAXED {
		// 宽松模式：当前版本必须 >= 读取版本
		if curVer < guard.ReadVersion {
			return false, curVal, curVer
		}
	} else {
		// 未知策略，返回失败
		return false, curVal, curVer
	}

	// 解析比较值
	if len(guard.Val) < 32 {
		return false, curVal, curVer
	}
	rhs := new(big.Int).SetBytes(guard.Val[:32])

	// 执行比较
	curInt := new(big.Int).SetBytes(curVal.Bytes())
	ok := c.cmp(guard.Op, curInt, rhs)

	return ok, curVal, curVer
}

// cmp 执行比较操作
func (c *joyueCoordinatorPrecompile) cmp(op uint8, cur *big.Int, rhs *big.Int) bool {
	switch op {
	case OP_EQ:
		return cur.Cmp(rhs) == 0
	case OP_NEQ:
		return cur.Cmp(rhs) != 0
	case OP_GT:
		return cur.Cmp(rhs) > 0
	case OP_GTE:
		return cur.Cmp(rhs) >= 0
	case OP_LT:
		return cur.Cmp(rhs) < 0
	case OP_LTE:
		return cur.Cmp(rhs) <= 0
	default:
		return false
	}
}

// applyDeltaToValue 将 Delta 应用到值上（不写回 storage）
func (c *joyueCoordinatorPrecompile) applyDeltaToValue(curVal common.Hash, delta Delta) (common.Hash, bool) {
	// 解析 Delta 值
	if len(delta.Val) < 32 {
		return common.Hash{}, false
	}
	deltaVal := new(big.Int).SetBytes(delta.Val[:32])
	curInt := new(big.Int).SetBytes(curVal.Bytes())

	var newInt *big.Int
	switch delta.Op {
	case D_ADD:
		newInt = new(big.Int).Add(curInt, deltaVal)
	case D_SUB:
		if curInt.Cmp(deltaVal) < 0 {
			return common.Hash{}, false // 下溢
		}
		newInt = new(big.Int).Sub(curInt, deltaVal)
	case D_SET:
		newInt = deltaVal
	case D_BIT_OR:
		newInt = new(big.Int).Or(curInt, deltaVal)
	case D_BIT_CLEAR:
		notDeltaVal := new(big.Int).Not(deltaVal)
		newInt = new(big.Int).And(curInt, notDeltaVal)
	case D_BIT_XOR:
		newInt = new(big.Int).Xor(curInt, deltaVal)
	default:
		return common.Hash{}, false
	}

	// 转换为 common.Hash
	newBytes := make([]byte, 32)
	newInt.FillBytes(newBytes)
	return common.BytesToHash(newBytes), true
}

// applyAuthoritativeFreeze 应用权威冻结（对 SUB 只写冻结表，不修改权威状态）
func (c *joyueCoordinatorPrecompile) applyAuthoritativeFreeze(evm *EVM, coordinatorAddr common.Address, txHash common.Hash, deltas []Delta) {
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Str("txHash", txHash.Hex()).
		Int("deltasCount", len(deltas)).
		Msg("[JOYUE Coordinator] applyAuthoritativeFreeze: START")

	for i, delta := range deltas {
		if delta.ContractAddr != coordinatorAddr || delta.ShardId != evm.Context.ShardID {
			utils.Logger().Info().
				Int("deltaIndex", i).
				Str("deltaContract", delta.ContractAddr.Hex()).
				Str("coordinatorAddr", coordinatorAddr.Hex()).
				Uint32("deltaShardId", delta.ShardId).
				Uint32("localShardId", evm.Context.ShardID).
				Msg("[JOYUE Coordinator] applyAuthoritativeFreeze: Skipping delta (contract mismatch)")
			continue
		}
		if delta.Op == D_SUB {
			if len(delta.Val) < 32 {
				utils.Logger().Warn().
					Int("deltaIndex", i).
					Str("deltaKey", delta.Key.Hex()).
					Int("deltaValLen", len(delta.Val)).
					Msg("[JOYUE Coordinator] applyAuthoritativeFreeze: Delta value too short, skipping")
				continue
			}
			subVal := new(big.Int).SetBytes(delta.Val[:32])
			// 可用量 = auth - totalFrozen，guard 已通过，直接写入冻结表
			c.addToMasterFreezeTable(evm, coordinatorAddr, delta.Key, txHash, subVal)
			utils.Logger().Info().
				Int("deltaIndex", i).
				Str("deltaKey", delta.Key.Hex()).
				Str("txHash", txHash.Hex()).
				Str("subVal", subVal.Text(10)).
				Msg("[JOYUE Coordinator] applyAuthoritativeFreeze: SUB delta added to freeze table (auth unchanged)")
		} else {
			utils.Logger().Info().
				Int("deltaIndex", i).
				Str("deltaKey", delta.Key.Hex()).
				Uint8("deltaOp", delta.Op).
				Msg("[JOYUE Coordinator] applyAuthoritativeFreeze: Skipping non-SUB delta (only SUB operations are frozen)")
		}
	}

	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Msg("[JOYUE Coordinator] applyAuthoritativeFreeze: COMPLETED")
}

// encodeColumnResults 编码 ColumnResult[] 数组
func (c *joyueCoordinatorPrecompile) encodeColumnResults(results []ColumnResult) ([]byte, error) {
	// ABI 编码动态数组：
	// - 偏移量（32 字节）：指向数组数据
	// - 数组长度（32 字节）
	// - 每个元素的偏移量（32 字节 × N）
	// - 每个元素的数据

	// 计算总大小
	offset := 32 + 32 + len(results)*32 // 偏移量 + 长度 + 元素偏移量
	elementOffsets := make([]int, len(results))
	elementSizes := make([]int, len(results))

	// 计算每个元素的大小和偏移量
	for i := range results {
		elementOffsets[i] = offset
		// ColumnResult 结构大小：txHash(32) + ok(32) + guardFailed(32) + failedGuardIndex(32) + failedDeltaIndex(32) + latestKey(32) + latestVal(32) + latestVer(32) = 256
		elementSizes[i] = 256
		offset += elementSizes[i]
	}

	// 分配输出缓冲区
	output := make([]byte, offset)

	// 写入数组偏移量（固定为 32，因为偏移量本身在位置 0）
	binary.BigEndian.PutUint64(output[24:32], 32)

	// 写入数组长度
	binary.BigEndian.PutUint64(output[56:64], uint64(len(results)))

	// 写入每个元素的偏移量
	for i, elemOffset := range elementOffsets {
		offsetPos := 64 + i*32
		binary.BigEndian.PutUint64(output[offsetPos+24:offsetPos+32], uint64(elemOffset))
	}

	// 写入每个元素的数据
	for i, result := range results {
		elemPos := elementOffsets[i]
		// txHash (bytes32)
		copy(output[elemPos:elemPos+32], result.TxHash.Bytes())
		// ok (bool)
		if result.Ok {
			output[elemPos+63] = 1
		}
		// guardFailed (bool)
		if result.GuardFailed {
			output[elemPos+95] = 1
		}
		// failedGuardIndex (uint32)
		binary.BigEndian.PutUint32(output[elemPos+124:elemPos+128], result.FailedGuardIndex)
		// failedDeltaIndex (uint32)
		binary.BigEndian.PutUint32(output[elemPos+156:elemPos+160], result.FailedDeltaIndex)
		// latestKey (bytes32)
		copy(output[elemPos+160:elemPos+192], result.LatestKey.Bytes())
		// latestVal (uint256)
		if result.LatestVal != nil {
			valBytes := make([]byte, 32)
			result.LatestVal.FillBytes(valBytes)
			copy(output[elemPos+192:elemPos+224], valBytes)
		}
		// latestVer (uint64)
		binary.BigEndian.PutUint64(output[elemPos+216:elemPos+224], result.LatestVer)
	}

	return output, nil
}

// handleFinalizeBatch 处理 finalizeBatch 函数调用
func (c *joyueCoordinatorPrecompile) handleFinalizeBatch(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	// 解析参数：batchId (bytes32) + txHashes (bytes32[]) + commit (bool)
	if len(params) < 96 {
		return nil, errors.New("JOYUE: invalid params length")
	}

	// batchId 在前 32 字节
	batchId := common.BytesToHash(params[0:32])

	// txHashes 是动态数组，偏移量在 32-64 字节
	txHashesOffset := int(binary.BigEndian.Uint64(params[32:64]))
	if txHashesOffset+32 > len(params) {
		return nil, errors.New("JOYUE: invalid txHashes array offset")
	}
	txHashesLen := binary.BigEndian.Uint64(params[txHashesOffset : txHashesOffset+32])

	// 解析 txHashes 数组
	txHashes := make([]common.Hash, 0, txHashesLen)
	for i := uint64(0); i < txHashesLen; i++ {
		hashOffset := txHashesOffset + 32 + int(i*32)
		if hashOffset+32 > len(params) {
			return nil, errors.New("JOYUE: invalid txHash offset")
		}
		txHashes = append(txHashes, common.BytesToHash(params[hashOffset:hashOffset+32]))
	}

	// commit (bool) 在 64-96 字节
	commit := params[95] != 0

	// 执行最终化逻辑
	err := c.executeFinalizeBatch(evm, coordinatorAddr, batchId, txHashes, commit)
	if err != nil {
		return nil, err
	}

	// finalizeBatch 没有返回值，返回空
	return []byte{}, nil
}

// // executeFinalizeBatch 执行最终化批次
//
//	func (c *joyueCoordinatorPrecompile) executeFinalizeBatch(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, txHashes []common.Hash, commit bool) error {
//		for _, txHash := range txHashes {
//			// 检查是否已冻结（通过 _frozenDeltas 的长度判断）
//			if !c.isFrozen(evm, coordinatorAddr, batchId, txHash) {
//				baseSlot := getBlobSlot(batchId, txHash)
//				firstSlot := evm.StateDB.GetState(coordinatorAddr, baseSlot)
//				utils.Logger().Info().
//					Str("coordinatorAddr", coordinatorAddr.Hex()).
//					Str("batchId", batchId.Hex()).
//					Str("txHash", txHash.Hex()).
//					Str("baseSlot", baseSlot.Hex()).
//					Hex("firstSlot", firstSlot.Bytes()).
//					Bool("commit", commit).
//					Msg("[JOYUE Coordinator] executeFinalizeBatch: isFrozen=false, skipping (no frozen deltas for this tx on this coordinator)")
//				continue
//			}
//
//			if commit {
//				deltas, err := c.getFrozenDeltasDecoded(evm, coordinatorAddr, batchId, txHash)
//				if err != nil {
//					return err
//				}
//				if err = c.finalizeCommitApply(evm, coordinatorAddr, txHash, deltas); err != nil {
//					return err
//				}
//			} else {
//				deltas, err := c.getFrozenDeltasDecoded(evm, coordinatorAddr, batchId, txHash)
//				if err != nil {
//					return err
//				}
//				c.finalizeRollback(evm, coordinatorAddr, batchId, txHash, deltas)
//			}
//
//			// 清除冻结状态（通过清除 Blob 数据）
//			c.clearFrozenDeltasBlob(evm, coordinatorAddr, batchId, txHash)
//		}
//
//		return nil
//	}
//
// executeFinalizeBatch 执行最终化批次
func (c *joyueCoordinatorPrecompile) executeFinalizeBatch(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, txHashes []common.Hash, commit bool) error {
	for _, txHash := range txHashes {
		// 检查是否已冻结（通过 _frozenDeltas 的长度判断）
		isFrozen := c.isFrozen(evm, coordinatorAddr, batchId, txHash)

		// 场景 1：如果是 Commit，且本地没有冻结，说明严重异常或该节点未参与，直接跳过
		if commit && !isFrozen {
			baseSlot := getBlobSlot(batchId, txHash)
			firstSlot := evm.StateDB.GetState(coordinatorAddr, baseSlot)
			utils.Logger().Info().
				Str("coordinatorAddr", coordinatorAddr.Hex()).
				Uint32("shardID", evm.Context.ShardID).
				Uint64("blockNumber", evm.BlockNumber.Uint64()).
				Str("currentTxHash", evm.StateDB.TxHash().Hex()).
				Str("batchId", batchId.Hex()).
				Str("commitTxHash", txHash.Hex()).
				Str("baseSlot", baseSlot.Hex()).
				Hex("firstSlot", firstSlot.Bytes()).
				Msg("[JOYUE Coordinator] executeFinalizeBatch: isFrozen=false - check if batchVerifyAndFreeze ran in EARLIER block before this applyFinalize")
			continue
		}

		if commit {
			// 正常 Commit 流程
			deltas, err := c.getFrozenDeltasDecoded(evm, coordinatorAddr, batchId, txHash)
			if err != nil {
				return err
			}
			if err = c.finalizeCommitApply(evm, coordinatorAddr, txHash, deltas); err != nil {
				return err
			}
		} else {
			// 场景 2：Rollback 回滚流程
			if isFrozen {
				// 正常回滚：本地有冻结记录，走原有的清除冻结表 + 广播解冻事件的逻辑
				deltas, err := c.getFrozenDeltasDecoded(evm, coordinatorAddr, batchId, txHash)
				if err != nil {
					return err
				}
				c.finalizeRollback(evm, coordinatorAddr, batchId, txHash, deltas)
			} else {
				// 核心优化：本地（主合约）冻结失败导致 isFrozen == false
				// 虽然不需要操作本地冻结表，但必须把事件广播出去，通知缓存系统（如分片 B）解冻！
				// 我们通过读取 PendingBatch 找回这笔交易原本的 Deltas。
				pendingBatch, err := c.getPendingBatch(evm, coordinatorAddr, batchId)
				if err == nil && pendingBatch != nil {
					var targetDeltas []Delta
					for _, detail := range pendingBatch.Details {
						if detail.TxHash == txHash {
							targetDeltas = detail.Deltas
							break
						}
					}

					if len(targetDeltas) > 0 {
						utils.Logger().Info().
							Str("coordinatorAddr", coordinatorAddr.Hex()).
							Str("txHash", txHash.Hex()).
							Msg("[JOYUE Coordinator] executeFinalizeBatch: Not frozen locally, but emitting StateUnfreeze for cache relayers")

						// 仅发出解冻事件，绝对不要调用 removeFromMasterFreezeTable，因为之前就没加进去
						for _, delta := range targetDeltas {
							if delta.ContractAddr != coordinatorAddr {
								continue
							}
							if delta.Op == D_SUB && len(delta.Val) >= 32 {
								c.emitStateUnfreeze(evm, coordinatorAddr, delta.Key, txHash)
							}
						}
					}
				} else {
					// 远端分片收到 applyFinalize 时本地没有 PendingBatch 是正常的，直接跳过
					utils.Logger().Info().
						Str("coordinatorAddr", coordinatorAddr.Hex()).
						Str("batchId", batchId.Hex()).
						Str("txHash", txHash.Hex()).
						Bool("commit", commit).
						Msg("[JOYUE Coordinator] executeFinalizeBatch: isFrozen=false and no PendingBatch, skipping rollback safely")
				}
			}
		}

		// 如果之前有冻结，清理 Blob 数据
		if isFrozen {
			c.clearFrozenDeltasBlob(evm, coordinatorAddr, batchId, txHash)
		}
	}

	return nil
}

// finalizeCommitApply 提交时应用 Deltas，并为所有涉及的 key 发出 StateBroadcast 事件（携带 txId）。
// - D_SUB：冻结时已写入 storage，commit 时再调一次 setUint 刷新版本（明确标记"已最终确认"），再广播。
// - 其他（ADD/SET/BIT_OR 等）：先写入 storage，再广播 + 解冻 txId。
func (c *joyueCoordinatorPrecompile) finalizeCommitApply(evm *EVM, coordinatorAddr common.Address, txHash common.Hash, deltas []Delta) error {
	for _, delta := range deltas {
		if delta.ContractAddr != coordinatorAddr {
			continue
		}
		if delta.Op == D_SUB {
			// SUB 冻结阶段只写了冻结表，commit 时：扣减权威状态、从冻结表移除、广播
			if len(delta.Val) < 32 {
				continue
			}
			authVal := c.getAuthValue(evm, coordinatorAddr, delta.Key)
			subVal := new(big.Int).SetBytes(delta.Val[:32])
			newAuth := new(big.Int).Sub(new(big.Int).SetBytes(authVal.Bytes()), subVal)
			if newAuth.Sign() < 0 {
				newAuth.SetInt64(0)
			}
			newBytes := make([]byte, 32)
			newAuth.FillBytes(newBytes)
			c.setUint(evm, coordinatorAddr, delta.Key, common.BytesToHash(newBytes))
			c.removeFromMasterFreezeTable(evm, coordinatorAddr, delta.Key, txHash)
			c.emitStateBroadcast(evm, coordinatorAddr, delta.Key, txHash)
			continue
		}
		// 应用非 SUB Delta（ADD / SET / BIT_OR 等）
		curVal, _ := c.getUint(evm, coordinatorAddr, delta.Key, 0)
		newVal, ok := c.applyDeltaToValue(curVal, delta)
		if !ok {
			return errors.New("JOYUE: commit apply failed")
		}
		c.setUint(evm, coordinatorAddr, delta.Key, newVal)
		c.emitStateBroadcast(evm, coordinatorAddr, delta.Key, txHash)
	}
	return nil
}

// finalizeRollback 回滚时恢复 D_SUB 状态并发出 StateUnfreeze 事件（携带 txId），通知 Agent 释放冻结量。
func (c *joyueCoordinatorPrecompile) finalizeRollback(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, txHash common.Hash, deltas []Delta) {
	for _, delta := range deltas {
		if delta.ContractAddr != coordinatorAddr {
			continue
		}
		if delta.Op == D_SUB {
			if len(delta.Val) < 32 {
				continue
			}
			// 冻结阶段只写了冻结表，未修改 auth，rollback 时只需从冻结表移除并发出解冻事件
			c.removeFromMasterFreezeTable(evm, coordinatorAddr, delta.Key, txHash)
			utils.Logger().Info().
				Str("coordinatorAddr", coordinatorAddr.Hex()).
				Str("batchId", batchId.Hex()).
				Str("txHash", txHash.Hex()).
				Str("deltaKey", delta.Key.Hex()).
				Msg("[JOYUE Coordinator] finalizeRollback: D_SUB removed from freeze table, emitStateUnfreeze")
			c.emitStateUnfreeze(evm, coordinatorAddr, delta.Key, txHash)
		}
	}
}

// getFrozenDeltasDecoded 获取并解码冻结的 Deltas（Blob 模式）
func (c *joyueCoordinatorPrecompile) getFrozenDeltasDecoded(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, txHash common.Hash) ([]Delta, error) {
	// 使用 Blob 模式读取并 RLP 解码
	return c.getFrozenDeltasBlob(evm, coordinatorAddr, batchId, txHash)
}

// ── handleApplyCommitAndRetry ──────────────────────────────────────────────
// 远端 coordinator 收到 attempt 2 的组合请求时调用
// ABI 编码格式：applyCommitAndRetry(bytes32 batch1, bytes32[] commitTxHashes, bytes32[] retryTxHashes, bytes32 batch2, bytes columnTxsData)
// params（已去掉 4 字节 selector）布局：
//
//	[0:32]   batch1          (bytes32, fixed)
//	[32:64]  commitOffset    (uint256，指向 commitTxHashes 数组头)
//	[64:96]  retryOffset     (uint256，指向 retryTxHashes 数组头)
//	[96:128] batch2          (bytes32, fixed)
//	[128:160] colOffset      (uint256，指向 columnTxsData bytes 头)
//	[commitOffset:]  commitTxHashes: length + elements
//	[retryOffset:]   retryTxHashes:  length + elements
//	[colOffset:]     columnTxsData:  length + padded bytes
func (c *joyueCoordinatorPrecompile) handleApplyCommitAndRetry(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Int("paramsLen", len(params)).
		Msg("[JOYUE Coordinator] handleApplyCommitAndRetry: CALLED")

	if len(params) < 160 {
		return nil, errors.New("JOYUE: applyCommitAndRetry params too short (need ≥160)")
	}

	batch1 := common.BytesToHash(params[0:32])
	commitOffset, err := readU256AsInt(params, 32)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyCommitAndRetry commitOffset: %w", err)
	}
	retryOffset, err := readU256AsInt(params, 64)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyCommitAndRetry retryOffset: %w", err)
	}
	batch2 := common.BytesToHash(params[96:128])
	colOffset, err := readU256AsInt(params, 128)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyCommitAndRetry colOffset: %w", err)
	}

	// decode commitTxHashes
	commitTxHashes, err := abiDecodeBytes32Array(params, commitOffset)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyCommitAndRetry commitTxHashes: %w", err)
	}
	// decode retryTxHashes
	retryTxHashes, err := abiDecodeBytes32Array(params, retryOffset)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyCommitAndRetry retryTxHashes: %w", err)
	}
	// decode columnTxsData (bytes)
	columnTxsData, err := abiDecodeBytes(params, colOffset)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyCommitAndRetry columnTxsData: %w", err)
	}

	utils.Logger().Info().
		Str("batch1", batch1.Hex()).
		Str("batch2", batch2.Hex()).
		Int("commitCount", len(commitTxHashes)).
		Int("retryCount", len(retryTxHashes)).
		Int("columnTxsLen", len(columnTxsData)).
		Msg("[JOYUE Coordinator] handleApplyCommitAndRetry: parsed params")

	// 1. finalize batch1 commit txs
	if len(commitTxHashes) > 0 {
		if err := c.executeFinalizeBatch(evm, coordinatorAddr, batch1, commitTxHashes, true); err != nil {
			utils.Logger().Warn().Err(err).Msg("[JOYUE Coordinator] handleApplyCommitAndRetry: finalize commit failed")
		}
	}
	// 2. unfreeze batch1 retry txs (rollback)
	if len(retryTxHashes) > 0 {
		if err := c.executeFinalizeBatch(evm, coordinatorAddr, batch1, retryTxHashes, false); err != nil {
			utils.Logger().Warn().Err(err).Msg("[JOYUE Coordinator] handleApplyCommitAndRetry: unfreeze retry failed")
		}
	}
	// 3. decode ColumnTxs and re-freeze for batch2
	if len(columnTxsData) == 0 {
		// 该参与者在 batch2 没有需要处理的 tx，直接返回空 ColumnResults
		utils.Logger().Warn().
			Str("coordinatorAddr", coordinatorAddr.Hex()).
			Str("batch2", batch2.Hex()).
			Int("commitCount", len(commitTxHashes)).
			Int("retryCount", len(retryTxHashes)).
			Msg("[JOYUE Coordinator] handleApplyCommitAndRetry: empty columnTxsData - this participant has no deltas for retry txs, applyFinalize will skip all (isFrozen=false)")
		return c.encodeColumnResults([]ColumnResult{})
	}
	col, err := c.decodeColumnTxArrayBody(columnTxsData)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: handleApplyCommitAndRetry decode columnTxs failed: %w", err)
	}
	results, err := c.executeBatchVerifyAndFreeze(evm, contract, coordinatorAddr, batch2, col)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: handleApplyCommitAndRetry executeBatchVerifyAndFreeze failed: %w", err)
	}
	utils.Logger().Info().
		Str("batch2", batch2.Hex()).
		Int("resultsCount", len(results)).
		Msg("[JOYUE Coordinator] handleApplyCommitAndRetry: re-freeze done")
	return c.encodeColumnResults(results)
}

// ── handleApplyFinalize ────────────────────────────────────────────────────
// 远端 coordinator 收到 attempt 3 的 finalize 请求时调用（fire-and-forget，无返回值）
// ABI 编码格式：applyFinalize(bytes32 batch2, bytes32[] commitTxHashes, bytes32[] failTxHashes)
// params（已去掉 4 字节 selector）布局：
//
//	[0:32]  batch2         (bytes32, fixed)
//	[32:64] commitOffset   (uint256，指向 commitTxHashes 数组头)
//	[64:96] failOffset     (uint256，指向 failTxHashes 数组头)
//	[commitOffset:] commitTxHashes: length + elements
//	[failOffset:]   failTxHashes:   length + elements
func (c *joyueCoordinatorPrecompile) handleApplyFinalize(evm *EVM, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Int("paramsLen", len(params)).
		Msg("[JOYUE Coordinator] handleApplyFinalize: CALLED")

	if len(params) < 96 {
		return nil, errors.New("JOYUE: applyFinalize params too short (need ≥96)")
	}

	batch2 := common.BytesToHash(params[0:32])
	commitOffset, err := readU256AsInt(params, 32)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyFinalize commitOffset: %w", err)
	}
	failOffset, err := readU256AsInt(params, 64)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyFinalize failOffset: %w", err)
	}

	commitTxHashes, err := abiDecodeBytes32Array(params, commitOffset)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyFinalize commitTxHashes: %w", err)
	}
	failTxHashes, err := abiDecodeBytes32Array(params, failOffset)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: applyFinalize failTxHashes: %w", err)
	}

	utils.Logger().Info().
		Str("batch2", batch2.Hex()).
		Int("commitCount", len(commitTxHashes)).
		Int("failCount", len(failTxHashes)).
		Msg("[JOYUE Coordinator] handleApplyFinalize: parsed params")

	if len(commitTxHashes) > 0 {
		if err := c.executeFinalizeBatch(evm, coordinatorAddr, batch2, commitTxHashes, true); err != nil {
			utils.Logger().Warn().Err(err).Msg("[JOYUE Coordinator] handleApplyFinalize: finalize commit failed")
		} else {
			utils.Logger().Info().
				Str("batch2", batch2.Hex()).
				Int("count", len(commitTxHashes)).
				Msg("[JOYUE Coordinator] handleApplyFinalize: finalize commit done")
		}
	}
	if len(failTxHashes) > 0 {
		if err := c.executeFinalizeBatch(evm, coordinatorAddr, batch2, failTxHashes, false); err != nil {
			utils.Logger().Warn().Err(err).Msg("[JOYUE Coordinator] handleApplyFinalize: rollback fail failed")
		} else {
			utils.Logger().Info().
				Str("batch2", batch2.Hex()).
				Int("count", len(failTxHashes)).
				Msg("[JOYUE Coordinator] handleApplyFinalize: rollback fail done")
		}
	}
	return []byte{}, nil
}

// handleClearRound 处理 clearRound 函数调用
// 注意：瞬时状态现在使用内存实现，在 executeBatchVerifyAndFreeze 执行完后自动释放
// 此函数保留是为了向后兼容，但不再执行任何操作
func (c *joyueCoordinatorPrecompile) handleClearRound(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	// 解析参数：batchId (bytes32)
	if len(params) < 32 {
		return nil, errors.New("JOYUE: invalid params length")
	}

	// 瞬时状态使用内存实现，不需要清理 Storage
	// clearRound 没有返回值，返回空
	return []byte{}, nil
}

// handleProcessBundleWithOneRetry 处理 processBundleWithOneRetry 函数调用
func (c *joyueCoordinatorPrecompile) handleProcessBundleWithOneRetry(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Str("caller", contract.CallerAddress.Hex()).
		Int("paramsLen", len(params)).
		Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry called")

	// 解析参数：IntentBundle + roundIdBase (uint64) + participants (address[])
	// 这是一个复杂的结构，需要逐步解析
	bundle, roundIdBase, participants, err := c.decodeProcessBundleWithOneRetry(params)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Int("paramsLen", len(params)).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry: decodeProcessBundleWithOneRetry failed")
		return nil, err
	}

	// 验证参数
	if len(participants) == 0 {
		return nil, errors.New("JOYUE: participants empty")
	}

	// ========== 测试模式：只打印参数，不执行主流程 ==========
	utils.Logger().Info().
		Str("bundleId", bundle.BundleId.Hex()).
		Uint32("agentShardId", bundle.AgentShardId).
		Uint32("masterShardId", bundle.MasterShardId).
		Str("agentContract", bundle.AgentContract.Hex()).
		Str("masterContract", bundle.MasterContract.Hex()).
		Uint64("epoch", bundle.Epoch).
		Uint64("agentBlockNumber", bundle.AgentBlockNumber).
		Uint64("timestampMs", bundle.TimestampMs).
		Int("detailCount", len(bundle.Detail)).
		Uint64("roundIdBase", roundIdBase).
		Int("participantsCount", len(participants)).
		Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Parameters")

	// 打印 bundle.Detail 详细信息
	utils.Logger().Info().
		Int("bundleDetailLen", len(bundle.Detail)).
		Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: About to iterate bundle.Detail")

	for i, detail := range bundle.Detail {
		utils.Logger().Info().
			Int("detailIndex", i).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Processing detail")

		// 安全地格式化 selector
		selectorStr := fmt.Sprintf("0x%02x%02x%02x%02x", detail.Req.Selector[0], detail.Req.Selector[1], detail.Req.Selector[2], detail.Req.Selector[3])

		utils.Logger().Info().
			Int("detailIndex", i).
			Str("txHash", detail.TxHash.Hex()).
			Str("sender", detail.Sender.Hex()).
			Uint64("nonce", detail.Nonce).
			Str("req.targetAddr", detail.Req.TargetAddr.Hex()).
			Str("req.selector", selectorStr).
			Int("req.argsLen", len(detail.Req.Args)).
			Int("guardsCount", len(detail.Guards)).
			Int("deltasCount", len(detail.Deltas)).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: IntentDetail")

		utils.Logger().Info().
			Int("detailIndex", i).
			Int("guardsLen", len(detail.Guards)).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: About to iterate Guards")

		// 打印 Guards 详细信息
		utils.Logger().Info().
			Int("detailIndex", i).
			Int("guardsLen", len(detail.Guards)).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: About to iterate Guards")

		for j, guard := range detail.Guards {
			utils.Logger().Info().
				Int("detailIndex", i).
				Int("guardIndex", j).
				Str("guard.contractAddr", guard.ContractAddr.Hex()).
				Str("guard.key", guard.Key.Hex()).
				Uint64("guard.readVersion", guard.ReadVersion).
				Uint8("guard.op", guard.Op).
				Hex("guard.val", guard.Val).
				Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Guard")
		}

		utils.Logger().Info().
			Int("detailIndex", i).
			Int("guardsLen", len(detail.Guards)).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Finished iterating Guards")

		// 打印 Deltas 详细信息
		utils.Logger().Info().
			Int("detailIndex", i).
			Int("deltasLen", len(detail.Deltas)).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: About to iterate Deltas")

		for j, delta := range detail.Deltas {
			utils.Logger().Info().
				Int("detailIndex", i).
				Int("deltaIndex", j).
				Str("delta.contractAddr", delta.ContractAddr.Hex()).
				Str("delta.key", delta.Key.Hex()).
				Uint8("delta.op", delta.Op).
				Hex("delta.val", delta.Val).
				Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Delta")
		}

		utils.Logger().Info().
			Int("detailIndex", i).
			Int("deltasLen", len(detail.Deltas)).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Finished iterating Deltas")
	}

	utils.Logger().Info().
		Int("bundleDetailLen", len(bundle.Detail)).
		Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Finished iterating bundle.Detail")

	// 打印 participants
	for i, participant := range participants {
		utils.Logger().Info().
			Int("participantIndex", i).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry - TEST MODE: Participant")
	}

	//返回空的 FinalResult[]（测试模式）
	//finals := make([]FinalResult, len(bundle.Detail))
	//for i := range finals {
	//	finals[i] = FinalResult{
	//		TxHash: bundle.Detail[i].TxHash,
	//		Status: 0, // FINAL_FAIL
	//	}
	//}

	// 执行主流程
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Uint64("roundIdBase", roundIdBase).
		Int("participantsCount", len(participants)).
		Int("detailsCount", len(bundle.Detail)).
		Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry: Starting executeProcessBundleWithOneRetry")

	finals, err := c.executeProcessBundleWithOneRetry(evm, contract, coordinatorAddr, bundle, roundIdBase, participants)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry: executeProcessBundleWithOneRetry failed")
		return nil, err
	}

	utils.Logger().Info().
		Int("finalsCount", len(finals)).
		Msg("[JOYUE Coordinator] handleProcessBundleWithOneRetry: executeProcessBundleWithOneRetry completed")

	// 编码返回结果
	return c.encodeFinalResults(finals)
}

// decodeProcessBundleWithOneRetry 解析 processBundleWithOneRetry 函数的参数
func (c *joyueCoordinatorPrecompile) decodeProcessBundleWithOneRetry(params []byte) (IntentBundle, uint64, []Participant, error) {
	var bundle IntentBundle
	var roundIdBase uint64
	var participants []Participant

	if len(params) < 96 {
		return bundle, 0, nil, errors.New("JOYUE: invalid params length")
	}

	// IntentBundle 是动态结构，偏移量在 0-32 字节（uint256）
	bundleOffsetRaw, err := readU256AsInt(params, 0)
	if err != nil {
		return bundle, 0, nil, err
	}

	utils.Logger().Info().
		Int("bundleOffsetRaw", bundleOffsetRaw).
		Int("paramsLen", len(params)).
		Hex("params0-8", params[0:8]).
		Hex("params24-32", params[24:32]).
		Hex("params32-64", params[32:64]).
		Hex("params64-96", params[64:96]).
		Msg("[JOYUE Coordinator] decodeProcessBundleWithOneRetry: bundleOffsetRaw")
	if bundleOffsetRaw+32 > len(params) {
		return bundle, 0, nil, errors.New("JOYUE: invalid bundle offset")
	}

	// 解析 IntentBundle
	bundle, err = c.decodeIntentBundle(params, bundleOffsetRaw)
	if err != nil {
		return bundle, 0, nil, err
	}

	// roundIdBase (uint64) 在 32-64 字节（左填充）
	roundIdBase = binary.BigEndian.Uint64(params[56:64])

	// participants (Participant[]) 是动态数组，偏移量在 64-96 字节（uint256）
	participantsOffsetRaw, err := readU256AsInt(params, 64)
	if err != nil {
		return bundle, 0, nil, err
	}

	utils.Logger().Info().
		Int("participantsOffsetRaw", participantsOffsetRaw).
		Msg("[JOYUE Coordinator] decodeProcessBundleWithOneRetry: participantsOffsetRaw")
	if participantsOffsetRaw+32 > len(params) {
		return bundle, 0, nil, errors.New("JOYUE: invalid participants array offset")
	}
	participantsLen, err := readU256AsUint64(params, participantsOffsetRaw)
	if err != nil {
		return bundle, 0, nil, err
	}

	// 解析 participants 数组（每个元素为 (address,uint32)，静态 tuple）
	participants = make([]Participant, 0, participantsLen)
	for i := uint64(0); i < participantsLen; i++ {
		elementOffset := participantsOffsetRaw + 32 + int(i*64)
		if elementOffset+64 > len(params) {
			return bundle, 0, nil, errors.New("JOYUE: invalid participant element offset")
		}
		addr := common.BytesToAddress(params[elementOffset+12 : elementOffset+32])
		shardId := binary.BigEndian.Uint32(params[elementOffset+60 : elementOffset+64])
		participants = append(participants, Participant{Addr: addr, ShardId: shardId})
	}

	return bundle, roundIdBase, participants, nil
}

// handleProcessIntent 处理 processIntent(bytes) 调用（统一在预编译解码 payload）
func (c *joyueCoordinatorPrecompile) handleProcessIntent(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	// params 为 ABI 编码的单个 bytes 参数
	if len(params) < 64 {
		return nil, errors.New("JOYUE: invalid params length for processIntent")
	}

	payloadOffset, err := readU256AsInt(params, 0)
	if err != nil {
		return nil, err
	}
	if payloadOffset+32 > len(params) {
		return nil, errors.New("JOYUE: invalid payload offset")
	}
	payloadLen, err := readU256AsUint64(params, payloadOffset)
	if err != nil {
		return nil, err
	}
	if payloadOffset+32+int(payloadLen) > len(params) {
		return nil, errors.New("JOYUE: invalid payload length")
	}
	payload := params[payloadOffset+32 : payloadOffset+32+int(payloadLen)]

	// 解码 payload
	detail, agentShardId, agentContract, agentBlockNumber, nonce, req, guards, deltas, err := c.decodeIntentPayload(payload)
	if err != nil {
		return nil, err
	}

	// 组装 IntentBundle
	bundle := IntentBundle{
		BundleId:         crypto.Keccak256Hash(payload, coordinatorAddr.Bytes()),
		AgentShardId:     agentShardId,
		MasterShardId:    evm.Context.ShardID,
		AgentContract:    agentContract,
		MasterContract:   coordinatorAddr,
		Epoch:            0,
		AgentBlockNumber: agentBlockNumber,
		TimestampMs:      0,
		Detail:           []IntentDetail{detail},
	}

	// 提取 participants（按 address + shardId 去重）
	participants := c.extractParticipantsFromDetail(detail)

	selectorStr := fmt.Sprintf("0x%02x%02x%02x%02x", req.Selector[0], req.Selector[1], req.Selector[2], req.Selector[3])
	utils.Logger().Info().
		Str("bundleId", bundle.BundleId.Hex()).
		Uint32("agentShardId", bundle.AgentShardId).
		Uint32("masterShardId", bundle.MasterShardId).
		Str("agentContract", bundle.AgentContract.Hex()).
		Str("masterContract", bundle.MasterContract.Hex()).
		Uint64("agentBlockNumber", bundle.AgentBlockNumber).
		Uint64("nonce", nonce).
		Str("req.selector", selectorStr).
		Int("participantsCount", len(participants)).
		Int("guardsCount", len(guards)).
		Int("deltasCount", len(deltas)).
		Msg("[JOYUE Coordinator] handleProcessIntent: decoded payload")

	// 执行 bundle（roundIdBase 需每个 bundle 唯一，避免同区块内 batchId 冲突）
	roundIdBase := evm.BlockNumber.Uint64()<<32 | uint64(evm.StateDB.TxIndex())
	finals, err := c.executeProcessBundleWithOneRetry(evm, contract, coordinatorAddr, bundle, roundIdBase, participants)
	if err != nil {
		return nil, err
	}

	return c.encodeFinalResults(finals)
}

// handleProcessIntentBatch 处理 processIntentBatch(bytes[]) 调用
func (c *joyueCoordinatorPrecompile) handleProcessIntentBatch(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	txHash := evm.StateDB.TxHash()
	blockNum := evm.BlockNumber.Uint64()

	utils.Logger().Info().
		Str("txHash", txHash.Hex()).
		Uint64("block", blockNum).
		Int("paramsLen", len(params)).
		Str("caller", contract.CallerAddress.Hex()).
		Msg("[JOYUE Coordinator] handleProcessIntentBatch: entry")

	// params 为 ABI 编码的 bytes[] 参数：offset(32) + [at offset: length(32) + elem_offsets(N*32)] + [len+data, ...]
	if len(params) < 64 {
		utils.Logger().Error().Str("txHash", txHash.Hex()).Int("paramsLen", len(params)).Msg("[JOYUE Coordinator] handleProcessIntentBatch: invalid params length")
		return nil, errors.New("JOYUE: invalid params length for processIntentBatch")
	}

	arrayOffset, err := readU256AsInt(params, 0)
	if err != nil {
		hexLen := 32
		if len(params) < hexLen {
			hexLen = len(params)
		}
		utils.Logger().Error().
			Err(err).
			Str("txHash", txHash.Hex()).
			Str("params0_32_hex", fmt.Sprintf("%x", params[:hexLen])).
			Msg("[JOYUE Coordinator] handleProcessIntentBatch: read array offset failed")
		return nil, err
	}
	if arrayOffset+32 > len(params) {
		utils.Logger().Error().Str("txHash", txHash.Hex()).Int("arrayOffset", arrayOffset).Int("paramsLen", len(params)).Msg("[JOYUE Coordinator] handleProcessIntentBatch: invalid array offset")
		return nil, errors.New("JOYUE: invalid array offset for processIntentBatch")
	}
	payloadCount, err := readU256AsUint64(params, arrayOffset)
	if err != nil {
		utils.Logger().Error().Err(err).Str("txHash", txHash.Hex()).Msg("[JOYUE Coordinator] handleProcessIntentBatch: read payload count failed")
		return nil, err
	}
	if payloadCount == 0 {
		utils.Logger().Error().Str("txHash", txHash.Hex()).Msg("[JOYUE Coordinator] handleProcessIntentBatch: empty payloads")
		return nil, errors.New("JOYUE: empty payloads")
	}
	if payloadCount > 100 {
		utils.Logger().Error().Str("txHash", txHash.Hex()).Uint64("payloadCount", payloadCount).Msg("[JOYUE Coordinator] handleProcessIntentBatch: too many payloads")
		return nil, errors.New("JOYUE: too many payloads")
	}

	// 解析每个 payload，element offsets 在 arrayOffset+32 处
	details := make([]IntentDetail, 0, payloadCount)
	var agentShardId uint32
	var agentContract common.Address
	var agentBlockNumber uint64

	elemOffsetsBase := arrayOffset + 32
	for i := uint64(0); i < payloadCount; i++ {
		elemOffset, err := readU256AsInt(params, elemOffsetsBase+int(i)*32)
		if err != nil {
			utils.Logger().Error().Err(err).Str("txHash", txHash.Hex()).Uint64("payloadIndex", i).Msg("[JOYUE Coordinator] handleProcessIntentBatch: read elem offset failed")
			return nil, err
		}
		if elemOffset+32 > len(params) {
			utils.Logger().Error().Str("txHash", txHash.Hex()).Uint64("payloadIndex", i).Int("elemOffset", elemOffset).Msg("[JOYUE Coordinator] handleProcessIntentBatch: invalid payload element offset")
			return nil, errors.New("JOYUE: invalid payload element offset")
		}
		payloadLen, err := readU256AsUint64(params, elemOffset)
		if err != nil {
			utils.Logger().Error().Err(err).Str("txHash", txHash.Hex()).Uint64("payloadIndex", i).Msg("[JOYUE Coordinator] handleProcessIntentBatch: read payload len failed")
			return nil, err
		}
		if elemOffset+32+int(payloadLen) > len(params) {
			utils.Logger().Error().Str("txHash", txHash.Hex()).Uint64("payloadIndex", i).Int("elemOffset", elemOffset).Uint64("payloadLen", payloadLen).Int("paramsLen", len(params)).Msg("[JOYUE Coordinator] handleProcessIntentBatch: invalid payload length")
			return nil, errors.New("JOYUE: invalid payload length")
		}
		payload := params[elemOffset+32 : elemOffset+32+int(payloadLen)]

		detail, aShardId, aContract, aBlockNum, _, _, _, _, err := c.decodeIntentPayload(payload)
		if err != nil {
			utils.Logger().Error().Err(err).Str("txHash", txHash.Hex()).Uint64("payloadIndex", i).Int("payloadLen", len(payload)).Msg("[JOYUE Coordinator] handleProcessIntentBatch: decode payload failed")
			return nil, fmt.Errorf("JOYUE: decode payload %d failed: %w", i, err)
		}
		details = append(details, detail)
		if i == 0 {
			agentShardId = aShardId
			agentContract = aContract
			agentBlockNumber = aBlockNum
		}
	}

	// 组装 IntentBundle
	bundleIdInput := make([]byte, 0, 32*len(details)+len(coordinatorAddr.Bytes()))
	for _, d := range details {
		bundleIdInput = append(bundleIdInput, d.TxHash.Bytes()...)
	}
	bundleIdInput = append(bundleIdInput, coordinatorAddr.Bytes()...)

	bundle := IntentBundle{
		BundleId:         crypto.Keccak256Hash(bundleIdInput),
		AgentShardId:     agentShardId,
		MasterShardId:    evm.Context.ShardID,
		AgentContract:    agentContract,
		MasterContract:   coordinatorAddr,
		Epoch:            0,
		AgentBlockNumber: agentBlockNumber,
		TimestampMs:      0,
		Detail:           details,
	}

	// 合并所有 detail 的 participants（必须排序以保证确定性，避免 map 迭代顺序导致 BAD BLOCK merkle root 不一致）
	participantSet := make(map[Participant]bool)
	for _, d := range details {
		for _, p := range c.extractParticipantsFromDetail(d) {
			participantSet[p] = true
		}
	}
	participants := make([]Participant, 0, len(participantSet))
	for p := range participantSet {
		participants = append(participants, p)
	}
	sort.Slice(participants, func(i, j int) bool {
		if participants[i].ShardId != participants[j].ShardId {
			return participants[i].ShardId < participants[j].ShardId
		}
		return bytes.Compare(participants[i].Addr.Bytes(), participants[j].Addr.Bytes()) < 0
	})

	utils.Logger().Info().
		Str("txHash", txHash.Hex()).
		Str("bundleId", bundle.BundleId.Hex()).
		Int("detailsCount", len(details)).
		Int("participantsCount", len(participants)).
		Uint32("agentShardId", agentShardId).
		Str("agentContract", agentContract.Hex()).
		Msg("[JOYUE Coordinator] handleProcessIntentBatch: decoded bundle, starting executeProcessBundleWithOneRetry")

	// roundIdBase 必须每个 bundle 唯一，避免同区块内多个 processIntentBatch 共享 batchId 导致 PendingBatch/ResultMatrix 覆盖
	// 使用 blockNumber<<32 | txIndex 确保同区块内不同交易得到不同 roundIdBase
	roundIdBase := evm.BlockNumber.Uint64()<<32 | uint64(evm.StateDB.TxIndex())
	finals, err := c.executeProcessBundleWithOneRetry(evm, contract, coordinatorAddr, bundle, roundIdBase, participants)
	if err != nil {
		utils.Logger().Error().Err(err).
			Str("txHash", txHash.Hex()).
			Str("bundleId", bundle.BundleId.Hex()).
			Int("detailsCount", len(details)).
			Msg("[JOYUE Coordinator] handleProcessIntentBatch: executeProcessBundleWithOneRetry failed")
		return nil, err
	}

	utils.Logger().Info().
		Str("txHash", txHash.Hex()).
		Str("bundleId", bundle.BundleId.Hex()).
		Int("finalsCount", len(finals)).
		Msg("[JOYUE Coordinator] handleProcessIntentBatch: success")

	// 返回 FinalResult[][]，外层一个元素（本 bundle 的结果数组）
	return c.encodeFinalResultsBatch(finals)
}

// encodeFinalResultsBatch 编码 FinalResult[][] 用于 processIntentBatch 返回值
func (c *joyueCoordinatorPrecompile) encodeFinalResultsBatch(finals []FinalResult) ([]byte, error) {
	// 返回 (FinalResult[][])，即 [finals]
	inner, err := c.encodeFinalResults(finals)
	if err != nil {
		return nil, err
	}
	// 修正 inner 中的 offset：inner 被放在 96 处，其 array data 在 96+32=128
	copy(inner[24:32], make([]byte, 8))
	binary.BigEndian.PutUint64(inner[24:32], 128)

	// 外层：offset(32)=32, length(32)=1, elem0_offset(32)=96
	result := make([]byte, 0, 96+len(inner))
	result = append(result, make([]byte, 24)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], 32) // offset to outer array
	result = append(result, make([]byte, 24)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], 1) // outer length = 1
	result = append(result, make([]byte, 24)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], 96) // element 0 offset
	result = append(result, inner...)
	return result, nil
}

// decodeIntentBundle 解析 IntentBundle 结构
func (c *joyueCoordinatorPrecompile) decodeIntentBundle(data []byte, offset int) (IntentBundle, error) {
	var bundle IntentBundle

	if offset+256 > len(data) {
		return bundle, errors.New("JOYUE: invalid IntentBundle offset")
	}

	// bundleId (bytes32) - 前 32 字节
	bundle.BundleId = common.BytesToHash(data[offset : offset+32])

	// agentShardId (uint32) - 32-64 字节（左填充）
	bundle.AgentShardId = binary.BigEndian.Uint32(data[offset+60 : offset+64])

	// masterShardId (uint32) - 64-96 字节（左填充）
	bundle.MasterShardId = binary.BigEndian.Uint32(data[offset+92 : offset+96])

	// agentContract (address) - 96-128 字节（右填充）
	bundle.AgentContract = common.BytesToAddress(data[offset+108 : offset+128])

	// masterContract (address) - 128-160 字节（右填充）
	bundle.MasterContract = common.BytesToAddress(data[offset+140 : offset+160])

	// epoch (uint64) - 160-192 字节（左填充）
	bundle.Epoch = binary.BigEndian.Uint64(data[offset+184 : offset+192])

	// agentBlockNumber (uint64) - 192-224 字节（左填充）
	bundle.AgentBlockNumber = binary.BigEndian.Uint64(data[offset+216 : offset+224])

	// timestampMs (uint64) - 224-256 字节（左填充）
	bundle.TimestampMs = binary.BigEndian.Uint64(data[offset+248 : offset+256])

	// detail (IntentDetail[]) - 动态数组，偏移量在 256-288 字节（uint256）
	// 注意：tuple 内部 offset 是相对 tuple 起始位置（offset），不是全局绝对位置
	detailOffsetRel, err := readU256AsInt(data, offset+256)
	if err != nil {
		return bundle, err
	}
	detailOffsetRaw := offset + detailOffsetRel

	utils.Logger().Info().
		Int("offset", offset).
		Int("detailOffsetRaw", detailOffsetRaw).
		Int("dataLen", len(data)).
		Hex("data256-288", data[offset+256:offset+288]).
		Msg("[JOYUE Coordinator] decodeIntentBundle: parsed detailOffsetRaw")

	if detailOffsetRaw+32 > len(data) {
		return bundle, errors.New("JOYUE: invalid detail array offset")
	}
	detailLen, err := readU256AsUint64(data, detailOffsetRaw)
	if err != nil {
		return bundle, err
	}

	utils.Logger().Info().
		Int("detailOffsetRaw", detailOffsetRaw).
		Uint64("detailLen", detailLen).
		Msg("[JOYUE Coordinator] decodeIntentBundle: parsed detailLen")

	// 解析 detail 数组
	bundle.Detail = make([]IntentDetail, 0, detailLen)

	utils.Logger().Info().
		Int("detailOffsetRaw", detailOffsetRaw).
		Uint64("detailLen", detailLen).
		Int("dataLen", len(data)).
		Msg("[JOYUE Coordinator] decodeIntentBundle: parsing detail array")

	for i := uint64(0); i < detailLen; i++ {
		// elementOffset 是 detail 数组中第 i 个元素的偏移量（相对于 data）
		elementOffset := detailOffsetRaw + 32 + int(i*32)
		if elementOffset+32 > len(data) {
			return bundle, errors.New("JOYUE: invalid detail element offset")
		}
		// elementPtr 是 IntentDetail 结构的偏移量
		// 注意：dynamic array 的 element offset 是相对于 array head（紧跟 length 的位置，即 detailOffsetRaw+32）
		elementPtrRel, err := readU256AsInt(data, elementOffset)
		if err != nil {
			return bundle, err
		}
		elementPtr := (detailOffsetRaw + 32) + elementPtrRel

		utils.Logger().Info().
			Uint64("i", i).
			Int("elementOffset", elementOffset).
			Int("elementPtr", elementPtr).
			Int("dataLen", len(data)).
			Hex("elementOffsetBytes", data[elementOffset:elementOffset+32]).
			Msg("[JOYUE Coordinator] decodeIntentBundle: parsing detail element")

		detail, err := c.decodeIntentDetail(data, elementPtr)
		if err != nil {
			utils.Logger().Error().
				Err(err).
				Int("elementPtr", elementPtr).
				Int("dataLen", len(data)).
				Msg("[JOYUE Coordinator] decodeIntentBundle: decodeIntentDetail failed")
			return bundle, err
		}
		bundle.Detail = append(bundle.Detail, detail)
	}

	return bundle, nil
}

// decodeIntentDetail 解析 IntentDetail 结构
func (c *joyueCoordinatorPrecompile) decodeIntentDetail(data []byte, offset int) (IntentDetail, error) {
	var detail IntentDetail

	if offset+224 > len(data) {
		return detail, errors.New("JOYUE: invalid IntentDetail offset")
	}

	// txHash (bytes32) - 前 32 字节
	detail.TxHash = common.BytesToHash(data[offset : offset+32])

	// sender (address) - 32-64 字节（右填充）
	detail.Sender = common.BytesToAddress(data[offset+44 : offset+64])

	// nonce (uint64) - 64-96 字节（左填充）
	detail.Nonce = binary.BigEndian.Uint64(data[offset+88 : offset+96])

	// req (RawRequest) - 动态结构，偏移量在 96-128 字节
	reqOffsetRel, err := readU256AsInt(data, offset+96)
	if err != nil {
		return detail, err
	}
	req, err := c.decodeRawRequest(data, offset+reqOffsetRel)
	if err != nil {
		return detail, err
	}
	detail.Req = req

	// guards (Guard[]) - 动态数组，偏移量在 128-160 字节
	guardsOffsetRel, err := readU256AsInt(data, offset+128)
	if err != nil {
		return detail, err
	}
	guards, err := c.decodeGuards(data, offset+guardsOffsetRel)
	if err != nil {
		return detail, err
	}
	detail.Guards = guards

	// deltas (Delta[]) - 动态数组，偏移量在 160-192 字节
	deltasOffsetRel, err := readU256AsInt(data, offset+160)
	if err != nil {
		return detail, err
	}
	deltas, err := c.decodeDeltas(data, offset+deltasOffsetRel)
	if err != nil {
		return detail, err
	}
	detail.Deltas = deltas

	return detail, nil
}

// decodeIntentPayload 解析 processIntent(bytes) 的 payload
// payload = abi.encode(txHash, agentShardId, agentContract, agentBlockNumber, nonce, req, guards, deltas, user)
func (c *joyueCoordinatorPrecompile) decodeIntentPayload(data []byte) (IntentDetail, uint32, common.Address, uint64, uint64, RawRequest, []Guard, []Delta, error) {
	var detail IntentDetail
	if len(data) < 288 {
		return detail, 0, common.Address{}, 0, 0, RawRequest{}, nil, nil, errors.New("JOYUE: invalid payload length")
	}

	txHash := common.BytesToHash(data[0:32])
	agentShardId := binary.BigEndian.Uint32(data[60:64])
	agentContract := common.BytesToAddress(data[76:96])
	agentBlockNumber := binary.BigEndian.Uint64(data[120:128])
	nonce := binary.BigEndian.Uint64(data[152:160])
	user := common.BytesToAddress(data[268:288]) // 第 9 个参数 user (address)，右对齐 20 字节

	reqOffsetRel, err := readU256AsInt(data, 160)
	if err != nil {
		return detail, 0, common.Address{}, 0, 0, RawRequest{}, nil, nil, err
	}
	guardsOffsetRel, err := readU256AsInt(data, 192)
	if err != nil {
		return detail, 0, common.Address{}, 0, 0, RawRequest{}, nil, nil, err
	}
	deltasOffsetRel, err := readU256AsInt(data, 224)
	if err != nil {
		return detail, 0, common.Address{}, 0, 0, RawRequest{}, nil, nil, err
	}

	req, err := c.decodeRawRequest(data, reqOffsetRel)
	if err != nil {
		return detail, 0, common.Address{}, 0, 0, RawRequest{}, nil, nil, err
	}
	guards, err := c.decodeGuards(data, guardsOffsetRel)
	if err != nil {
		return detail, 0, common.Address{}, 0, 0, RawRequest{}, nil, nil, err
	}
	deltas, err := c.decodeDeltas(data, deltasOffsetRel)
	if err != nil {
		return detail, 0, common.Address{}, 0, 0, RawRequest{}, nil, nil, err
	}

	detail = IntentDetail{
		TxHash: txHash,
		Sender: agentContract,
		Nonce:  nonce,
		Req:    req,
		Guards: guards,
		Deltas: deltas,
		User:   user,
	}

	return detail, agentShardId, agentContract, agentBlockNumber, nonce, req, guards, deltas, nil
}

// extractParticipantsFromDetail 提取参与者列表（address + shardId 去重）
func (c *joyueCoordinatorPrecompile) extractParticipantsFromDetail(detail IntentDetail) []Participant {
	set := make(map[Participant]bool)
	var participants []Participant

	for _, guard := range detail.Guards {
		p := Participant{Addr: guard.ContractAddr, ShardId: guard.ShardId}
		if !set[p] {
			set[p] = true
			participants = append(participants, p)
		}
	}
	for _, delta := range detail.Deltas {
		p := Participant{Addr: delta.ContractAddr, ShardId: delta.ShardId}
		if !set[p] {
			set[p] = true
			participants = append(participants, p)
		}
	}

	return participants
}

// decodeRawRequest 解析 RawRequest 结构
func (c *joyueCoordinatorPrecompile) decodeRawRequest(data []byte, offset int) (RawRequest, error) {
	var req RawRequest

	if offset+96 > len(data) {
		return req, errors.New("JOYUE: invalid RawRequest offset")
	}

	// targetAddr (address) - 前 32 字节（右填充）
	req.TargetAddr = common.BytesToAddress(data[offset+12 : offset+32])

	// selector (bytes4) - 32-64 字节（右填充）
	copy(req.Selector[:], data[offset+32:offset+36])

	// args (bytes) - 动态类型，偏移量在 64-96 字节
	// 注意：tuple 内部 offset 是相对本 tuple 的起始位置（offset）
	argsOffsetRel, err := readU256AsInt(data, offset+64)
	if err != nil {
		return req, err
	}
	argsOffset := offset + argsOffsetRel
	if argsOffset+32 > len(data) {
		return req, errors.New("JOYUE: invalid args offset")
	}
	argsLen, err := readU256AsUint64(data, argsOffset)
	if err != nil {
		return req, err
	}
	if argsOffset+32+int(argsLen) > len(data) {
		return req, errors.New("JOYUE: invalid args length")
	}
	req.Args = make([]byte, argsLen)
	copy(req.Args, data[argsOffset+32:argsOffset+32+int(argsLen)])

	return req, nil
}

// decodeGuards 解析 Guard[] 数组
func (c *joyueCoordinatorPrecompile) decodeGuards(data []byte, offset int) ([]Guard, error) {
	if offset+32 > len(data) {
		return nil, errors.New("JOYUE: invalid guards array offset")
	}

	arrayLen, err := readU256AsUint64(data, offset)
	if err != nil {
		return nil, err
	}
	guards := make([]Guard, 0, arrayLen)

	for i := uint64(0); i < arrayLen; i++ {
		elementOffset := offset + 32 + int(i*32)
		if elementOffset+32 > len(data) {
			return nil, errors.New("JOYUE: invalid guard element offset")
		}
		// 动态数组元素为动态 tuple，因此数组里存的是相对 offset 的 element pointer
		elementPtrRel, err := readU256AsInt(data, elementOffset)
		if err != nil {
			return nil, err
		}
		// 注意：array element offset 相对于 array head（紧跟 length 的位置，即 offset+32）
		elementPtr := (offset + 32) + elementPtrRel

		guard, err := c.decodeGuard(data, elementPtr)
		if err != nil {
			return nil, err
		}
		guards = append(guards, guard)
	}

	return guards, nil
}

// executeProcessBundleWithOneRetry 执行 processBundleWithOneRetry 主流程
// 重构：移除预校验，改为基于 Guard 和 Delta 矩阵的决策
func (c *joyueCoordinatorPrecompile) executeProcessBundleWithOneRetry(evm *EVM, contract *Contract, coordinatorAddr common.Address, bundle IntentBundle, roundIdBase uint64, participants []Participant) ([]FinalResult, error) {
	utils.Logger().Info().
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Str("bundleId", bundle.BundleId.Hex()).
		Uint64("epoch", bundle.Epoch).
		Uint64("roundIdBase", roundIdBase).
		Int("detailsCount", len(bundle.Detail)).
		Int("participantsCount", len(participants)).
		Msg("[JOYUE Coordinator] executeProcessBundleWithOneRetry: Starting")

	finals := make([]FinalResult, len(bundle.Detail))
	done := make([]bool, len(bundle.Detail))
	active := make([]bool, len(bundle.Detail))

	// 初始化：所有交易都处于活跃状态
	for i := range bundle.Detail {
		active[i] = true
	}

	// 只处理第一次尝试（异步模式）
	// 第一次尝试的结果会在回调中处理，当所有结果收集完成后，会在 triggerDecision 中判断是否需要第二次尝试
	utils.Logger().Info().
		Msg("[JOYUE Coordinator] executeProcessBundleWithOneRetry: Calling processFirstAttemptMatrix")

	err := c.processFirstAttemptMatrix(evm, contract, coordinatorAddr, bundle, roundIdBase, participants, active, finals, done)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Msg("[JOYUE Coordinator] executeProcessBundleWithOneRetry: processFirstAttemptMatrix failed")
		return nil, err
	}

	utils.Logger().Info().
		Msg("[JOYUE Coordinator] executeProcessBundleWithOneRetry: processFirstAttemptMatrix completed")

	// 注意：在异步模式下，第一次尝试的结果会在回调中处理
	// 所有结果收集完成后，会在 collectParticipantResult -> triggerDecision 中自动触发决策
	// 如果需要第二次尝试，也会在 triggerDecision 中自动触发

	// 返回初始状态（实际结果会在回调中更新）
	return finals, nil
}

// GuardMatrixResult Guard 矩阵结果（参与者 × 交易）
type GuardMatrixResult struct {
	Participant Participant
	TxResults   []GuardResult // 每个交易的 Guard 验证结果
}

// GuardResult 单个交易的 Guard 验证结果
type GuardResult struct {
	TxHash           common.Hash
	Ok               bool
	GuardFailed      bool
	FailedGuardIndex uint32
	LatestKey        common.Hash
	LatestVal        *big.Int
	LatestVer        uint64
}

// DeltaMatrixResult Delta 矩阵结果（参与者 × 交易）
type DeltaMatrixResult struct {
	Participant Participant
	TxResults   []DeltaResult // 每个交易的 Delta 冻结结果
}

// DeltaResult 单个交易的 Delta 冻结结果
type DeltaResult struct {
	TxHash           common.Hash
	Ok               bool
	FailedDeltaIndex uint32
	LatestKey        common.Hash
	LatestVal        *big.Int
	LatestVer        uint64
}

// MatrixDecision 矩阵决策结果
type MatrixDecision struct {
	commitTx    []common.Hash // 所有 Guard 和 Delta 都成功的交易
	retryTx     []common.Hash // 需要重试的交易
	failTx      []common.Hash // 最终失败的交易
	commitCount uint64
	retryCount  uint64
	failCount   uint64
}

// AttemptResult 尝试结果
type AttemptResult struct {
	committed    []bool
	needRetry    []bool
	callbackFail []bool
}

// PendingBatch 待处理的批次（异步 2PC 阶段1）
type PendingBatch struct {
	BatchId       common.Hash
	AttemptNo     uint8
	Participants  []Participant
	Details       []IntentDetail
	Done          []bool
	Active        []bool
	CreatedAt     uint64         // 区块号
	Epoch         uint64         // 用于构建第二次尝试的 batchId
	RoundIdBase   uint64         // 用于构建第二次尝试的 batchId
	AgentShardId  uint32         // 发起 Intent 的 Agent 所在分片（用于结果回调）
	AgentContract common.Address // 发起 Intent 的 Agent 合约地址
	// Phase 1 决策结果，供 processSecondAttemptMatrix 使用
	Phase1CommitTxHashes []common.Hash // attempt1 中判定为 commit 的 tx
	Phase1RetryTxHashes  []common.Hash // attempt1 中判定为 retry 的 tx
	Phase1FailTxHashes   []common.Hash // attempt1 中判定为直接 fail 的 tx（需要在各分片回滚）
	// Phase 2 决策结果，供 processThirdAttemptMatrix 使用
	Phase2CommitTxHashes []common.Hash // attempt2 中判定为 commit 的 tx
	Phase2FailTxHashes   []common.Hash // attempt2 中判定为 fail 的 tx
}

// ParticipantResult 参与者返回的结果（异步 2PC 阶段2）
type ParticipantResult struct {
	Participant  Participant
	GuardResults []GuardResult
	DeltaResults []DeltaResult
	Success      bool
}

// 注意：已移除预校验逻辑，改为基于矩阵的决策
// 预校验阶段不再需要，因为 Guard 和 Delta 的验证在 scatterAndFreeze 阶段完成

// processFirstAttemptMatrix 处理第一次尝试（基于矩阵，异步模式）
// 注意：在异步模式下，决策结果会在回调中处理，这里只启动异步流程
func (c *joyueCoordinatorPrecompile) processFirstAttemptMatrix(evm *EVM, contract *Contract, coordinatorAddr common.Address, bundle IntentBundle, roundIdBase uint64, participants []Participant, active []bool, finals []FinalResult, done []bool) error {
	batch1 := c.batchId(coordinatorAddr, bundle.Epoch, roundIdBase, 1)

	utils.Logger().Info().
		Str("batchId", batch1.Hex()).
		Uint64("epoch", bundle.Epoch).
		Uint64("roundIdBase", roundIdBase).
		Int("detailsCount", len(bundle.Detail)).
		Int("participantsCount", len(participants)).
		Msg("[JOYUE Coordinator] processFirstAttemptMatrix: Starting first attempt")

	// 启动异步流程（发出事件，不等待结果）
	// 决策结果会在 onCrossShardCallback 回调中处理
	err := c.runAttemptMatrix(evm, contract, coordinatorAddr, batch1, 1, participants, bundle.Detail, done, active, finals, bundle.Epoch, roundIdBase, bundle.AgentShardId, bundle.AgentContract)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("batchId", batch1.Hex()).
			Msg("[JOYUE Coordinator] processFirstAttemptMatrix: runAttemptMatrix failed")
		return err
	}

	utils.Logger().Info().
		Str("batchId", batch1.Hex()).
		Msg("[JOYUE Coordinator] processFirstAttemptMatrix: runAttemptMatrix completed")

	return nil
}

// processSecondAttemptMatrix 处理第二次尝试（Phase 2）
// 职责：
//  1. 对各分片发送组合请求：finalize batch1 C1 + unfreeze batch1 R + re-freeze R for batch2
//  2. 本地直接执行上述三步（不需要跨分片）
//  3. 等待 batch2 的 re-freeze 回调，触发 triggerDecision(attemptNo=2)
func (c *joyueCoordinatorPrecompile) processSecondAttemptMatrix(evm *EVM, contract *Contract, coordinatorAddr common.Address, epoch uint64, roundIdBase uint64, participants []Participant, details []IntentDetail, active []bool, finals []FinalResult, done []bool) error {
	batch1 := c.batchId(coordinatorAddr, epoch, roundIdBase, 1)
	pendingBatch1, err := c.getPendingBatch(evm, coordinatorAddr, batch1)
	if err != nil {
		return errors.New("JOYUE: first attempt not completed yet")
	}

	commitTxHashes := pendingBatch1.Phase1CommitTxHashes
	retryTxHashes := pendingBatch1.Phase1RetryTxHashes
	failTxHashes := pendingBatch1.Phase1FailTxHashes // F1 需要在各分片回滚

	utils.Logger().Info().
		Str("batch1", batch1.Hex()).
		Int("commitCount", len(commitTxHashes)).
		Int("retryCount", len(retryTxHashes)).
		Int("failCount", len(failTxHashes)).
		Msg("[JOYUE Coordinator] processSecondAttemptMatrix: starting")

	if len(retryTxHashes) == 0 {
		utils.Logger().Info().Msg("[JOYUE Coordinator] processSecondAttemptMatrix: no retry txs, nothing to do")
		return nil
	}

	// 构建 batch2 的 retryActive：只有 retry tx 参与
	retryActive := make([]bool, len(details))
	retryTxSet := make(map[common.Hash]bool, len(retryTxHashes))
	for _, h := range retryTxHashes {
		retryTxSet[h] = true
	}
	for i := range details {
		if !done[i] && retryTxSet[details[i].TxHash] {
			retryActive[i] = true
		}
	}

	// 从 batch1 的 callback 结果收集 StateOverride（guard/delta 失败时返回的权威最新状态）
	stateOverridesByTx := c.collectStateOverridesFromBatch1(evm, coordinatorAddr, batch1, participants, details, retryActive)

	// 若 agentOnSameShard 已设置，对每个 retry tx 调用 Agent.recomputeIntent 获取新 guards/deltas（传入 stateOverrides 优先于缓存）
	retryOverrideMap := make(map[common.Hash]retryOverride)
	agentOnSameShard := c.getAgentOnSameShard(evm, coordinatorAddr)
	utils.Logger().Info().
		Str("agentOnSameShard", agentOnSameShard.Hex()).
		Str("coordinatorAddr", coordinatorAddr.Hex()).
		Msg("[JOYUE Coordinator] processSecondAttemptMatrix: agentOnSameShard")

	var fastFailTxHashes []common.Hash  // 重试立即失败的交易
	var trueRetryTxHashes []common.Hash // 真正继续重试的交易

	if agentOnSameShard != (common.Address{}) {
		for i := range details {
			if !retryActive[i] {
				continue
			}
			d := &details[i]
			overrides := stateOverridesByTx[d.TxHash]
			guards, deltas, ok := c.callAgentRecomputeIntent(evm, contract, agentOnSameShard, d.Req, d.User, overrides)
			if ok {
				retryOverrideMap[d.TxHash] = retryOverride{Guards: guards, Deltas: deltas}
				trueRetryTxHashes = append(trueRetryTxHashes, d.TxHash)
				utils.Logger().Info().Str("txHash", d.TxHash.Hex()).Msg("[JOYUE Coordinator] processSecondAttemptMatrix: recomputeIntent succeeded")
			} else {
				// 业务逻辑验证失败，标记为终态 Fail，剥离出 Phase 2
				retryActive[i] = false
				done[i] = true
				finals[i] = FinalResult{TxHash: d.TxHash, Status: 1, AttemptsUsed: 2}
				fastFailTxHashes = append(fastFailTxHashes, d.TxHash)

				utils.Logger().Info().Str("txHash", d.TxHash.Hex()).Msg("[JOYUE Coordinator] processSecondAttemptMatrix: recomputeIntent failed, marking as terminal fail")
			}
		}
	} else {
		trueRetryTxHashes = retryTxHashes
	}

	// =========================================================================
	// 第一部分：直接进入第三阶段 (Phase 3) 解冻相关资源
	// 包含：Phase 1 失败的交易 + Phase 2 重试立即失败的交易
	// =========================================================================
	phase3UnfreezeTxHashes := append(failTxHashes, fastFailTxHashes...)

	if len(fastFailTxHashes) > 0 {
		// 立刻通过事件将短路失败的结果通知上层 Agent
		c.emitAgentResultToAgent(evm, contract, coordinatorAddr, pendingBatch1.AgentShardId, pendingBatch1.AgentContract, nil, fastFailTxHashes, CompletionAttempt1DirectFail, CompletionAttempt1RetryFail)
	}

	if len(phase3UnfreezeTxHashes) > 0 {
		for _, participant := range participants {
			if participant.ShardId == evm.Context.ShardID && participant.Addr == coordinatorAddr {
				// 本地执行：传入 attempt = 3 强制释放资源并发出 StateUnfreeze
				if err2 := c.executeFinalizeBatch(evm, coordinatorAddr, batch1, phase3UnfreezeTxHashes, false); err2 != nil {
					utils.Logger().Warn().Err(err2).Msg("[JOYUE Coordinator] processSecondAttemptMatrix: local phase3 unfreeze failed")
				}
			} else {
				// 远端执行：专门发一条 Finalize 请求给各分片，走第三阶段解冻通道
				if err2 := c.emitFinalizeRequest(evm, contract, coordinatorAddr, batch1, nil, phase3UnfreezeTxHashes, participant); err2 != nil {
					utils.Logger().Error().Err(err2).Msg("[JOYUE Coordinator] processSecondAttemptMatrix: emitFinalizeRequest for phase3 unfreeze failed")
				}
			}
		}
		utils.Logger().Info().
			Int("unfreezeCount", len(phase3UnfreezeTxHashes)).
			Msg("[JOYUE Coordinator] processSecondAttemptMatrix: completed direct Phase 3 unfreeze for terminal fails")
	}

	// =========================================================================
	// 极致短路：如果没有交易需要 Commit，也没有交易能继续重试，流程直接结束！
	// =========================================================================
	if len(commitTxHashes) == 0 && len(trueRetryTxHashes) == 0 {
		utils.Logger().Info().Msg("[JOYUE Coordinator] processSecondAttemptMatrix: no valid commits or retries, early exit after Phase 3 unfreeze")
		return nil
	}

	// =========================================================================
	// 第二部分：继续正常的 Phase 2 逻辑
	// =========================================================================

	batch2 := c.batchId(coordinatorAddr, epoch, roundIdBase, 2)

	// 保存 batch2 的 PendingBatch（结果回调时需要），继承 agent 信息
	pendingBatch2 := PendingBatch{
		BatchId:       batch2,
		AttemptNo:     2,
		Participants:  participants,
		Details:       details,
		Done:          done,
		Active:        retryActive,
		CreatedAt:     evm.BlockNumber.Uint64(),
		Epoch:         epoch,
		RoundIdBase:   roundIdBase,
		AgentShardId:  pendingBatch1.AgentShardId,
		AgentContract: pendingBatch1.AgentContract,
	}
	c.savePendingBatch(evm, coordinatorAddr, batch2, pendingBatch2)
	c.initResultMatrix(evm, coordinatorAddr, batch2, participants, len(details))

	utils.Logger().Info().
		Str("batch2", batch2.Hex()).
		Msg("[JOYUE Coordinator] processSecondAttemptMatrix: saved batch2 PendingBatch")

	// 为每个参与者发出"组合请求"
	for i, participant := range participants {
		// 构建该参与者在 batch2 中的 ColumnTx（只包含 retry tx，使用 recompute 的 override 若存在）
		col, idxMap := c.buildColumnTxsForParticipantWithRetryOverrides(participant, details, done, retryActive, retryOverrideMap)
		if err2 := c.saveParticipantIdxMap(evm, coordinatorAddr, batch2, participant, idxMap); err2 != nil {
			utils.Logger().Error().Err(err2).
				Str("participant", participant.Addr.Hex()).
				Msg("[JOYUE Coordinator] processSecondAttemptMatrix: saveParticipantIdxMap failed")
		}

		utils.Logger().Info().
			Int("participantIndex", i).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Int("columnTxCount", len(col)).
			Msg("[JOYUE Coordinator] processSecondAttemptMatrix: processing participant")

		if participant.ShardId == evm.Context.ShardID && participant.Addr == coordinatorAddr {
			// ── 本地执行 ──
			// 1. finalize batch1 commit txs（apply 之前冻结的状态）
			if len(commitTxHashes) > 0 {
				if err2 := c.executeFinalizeBatch(evm, coordinatorAddr, batch1, commitTxHashes, true); err2 != nil {
					utils.Logger().Warn().Err(err2).Msg("[JOYUE Coordinator] processSecondAttemptMatrix: local finalize commit failed")
				}
			}
			// 2. rollback batch1 fail txs（F1 直接失败的也需要回滚本地冻结）
			if len(failTxHashes) > 0 {
				if err2 := c.executeFinalizeBatch(evm, coordinatorAddr, batch1, failTxHashes, false); err2 != nil {
					utils.Logger().Warn().Err(err2).Msg("[JOYUE Coordinator] processSecondAttemptMatrix: local rollback fail failed")
				}
			}
			// 3. unfreeze batch1 retry txs（解除之前的冻结，加回库存）
			if len(retryTxHashes) > 0 {
				if err2 := c.executeFinalizeBatch(evm, coordinatorAddr, batch1, retryTxHashes, false); err2 != nil {
					utils.Logger().Warn().Err(err2).Msg("[JOYUE Coordinator] processSecondAttemptMatrix: local unfreeze retry failed")
				}
			}
			// 4. 对 batch2 重新冻结（只有 retry tx 有 ColumnTx）
			results, err2 := c.executeBatchVerifyAndFreeze(evm, contract, coordinatorAddr, batch2, col)
			if err2 != nil {
				guardResults := make([]GuardResult, len(details))
				deltaResults := make([]DeltaResult, len(details))
				for _, idx := range idxMap {
					if idx >= 0 && idx < len(details) {
						guardResults[idx] = GuardResult{Ok: false, GuardFailed: true}
						deltaResults[idx] = DeltaResult{Ok: false}
					}
				}
				_ = c.collectParticipantResult(evm, contract, coordinatorAddr, batch2, participant, guardResults, deltaResults, false)
				continue
			}
			guardResults, deltaResults := c.convertResultsFromColumnResults(results, idxMap, len(details))
			_ = c.collectParticipantResult(evm, contract, coordinatorAddr, batch2, participant, guardResults, deltaResults, true)
		} else {
			// ── 远端：发一条组合跨分片请求 ──
			// failTxHashes 和 retryTxHashes 都作为"需要解冻的"传给远端
			// 远端 handleApplyCommitAndRetry 会对两者都调 executeFinalizeBatch(rollback)
			allRollbackHashes := append(failTxHashes, retryTxHashes...)
			if err2 := c.emitApplyCommitAndRetryRequest(evm, contract, coordinatorAddr, batch1, commitTxHashes, allRollbackHashes, batch2, participant, col); err2 != nil {
				utils.Logger().Error().Err(err2).
					Str("participant", participant.Addr.Hex()).
					Msg("[JOYUE Coordinator] processSecondAttemptMatrix: emitApplyCommitAndRetryRequest failed")
				c.saveParticipantResult(evm, coordinatorAddr, batch2, participant, nil, nil, false)
			}
		}
	}

	utils.Logger().Info().
		Str("batch2", batch2.Hex()).
		Msg("[JOYUE Coordinator] processSecondAttemptMatrix: all participants dispatched")
	return nil
}

// processThirdAttemptMatrix 处理第三次尝试（Phase 3 finalize）
// 职责：把 batch2 的 C2/F2 决策下发到各分片，让各分片真正应用/回滚冻结状态
// 这是 fire-and-forget 模式：不需要等待所有分片的回调（decision 已经在 phase 2 确定）
func (c *joyueCoordinatorPrecompile) processThirdAttemptMatrix(evm *EVM, contract *Contract, coordinatorAddr common.Address, epoch uint64, roundIdBase uint64, participants []Participant, batch2 common.Hash, commitTxHashes []common.Hash, failTxHashes []common.Hash) error {
	utils.Logger().Info().
		Str("batch2", batch2.Hex()).
		Int("commitCount", len(commitTxHashes)).
		Int("failCount", len(failTxHashes)).
		Msg("[JOYUE Coordinator] processThirdAttemptMatrix: starting")

	for i, participant := range participants {
		utils.Logger().Info().
			Int("participantIndex", i).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Msg("[JOYUE Coordinator] processThirdAttemptMatrix: processing participant")

		if participant.ShardId == evm.Context.ShardID && participant.Addr == coordinatorAddr {
			// ── 本地直接执行 finalize ──
			if len(commitTxHashes) > 0 {
				if err := c.executeFinalizeBatch(evm, coordinatorAddr, batch2, commitTxHashes, true); err != nil {
					utils.Logger().Warn().Err(err).
						Msg("[JOYUE Coordinator] processThirdAttemptMatrix: local finalize commit failed")
				} else {
					utils.Logger().Info().
						Str("batch2", batch2.Hex()).
						Int("count", len(commitTxHashes)).
						Msg("[JOYUE Coordinator] processThirdAttemptMatrix: local finalize commit done")
				}
			}
			if len(failTxHashes) > 0 {
				if err := c.executeFinalizeBatch(evm, coordinatorAddr, batch2, failTxHashes, false); err != nil {
					utils.Logger().Warn().Err(err).
						Msg("[JOYUE Coordinator] processThirdAttemptMatrix: local rollback fail failed")
				} else {
					utils.Logger().Info().
						Str("batch2", batch2.Hex()).
						Int("count", len(failTxHashes)).
						Msg("[JOYUE Coordinator] processThirdAttemptMatrix: local rollback fail done")
				}
			}
		} else {
			// ── 远端：发 applyFinalize 跨分片请求（fire-and-forget）──
			if err := c.emitFinalizeRequest(evm, contract, coordinatorAddr, batch2, commitTxHashes, failTxHashes, participant); err != nil {
				utils.Logger().Error().Err(err).
					Str("participant", participant.Addr.Hex()).
					Msg("[JOYUE Coordinator] processThirdAttemptMatrix: emitFinalizeRequest failed")
			}
		}
	}

	utils.Logger().Info().
		Str("batch2", batch2.Hex()).
		Msg("[JOYUE Coordinator] processThirdAttemptMatrix: all participants dispatched")
	return nil
}

// batchId 计算批次 ID
func (c *joyueCoordinatorPrecompile) batchId(coordinatorAddr common.Address, epoch uint64, roundIdBase uint64, attempt uint8) common.Hash {
	// keccak256(abi.encodePacked(epoch, address(this), roundIdBase, attempt))
	data := make([]byte, 0, 32+20+8+1)
	data = append(data, big.NewInt(int64(epoch)).FillBytes(make([]byte, 32))...)
	data = append(data, coordinatorAddr.Bytes()...)
	data = append(data, big.NewInt(int64(roundIdBase)).FillBytes(make([]byte, 8))...)
	data = append(data, byte(attempt))
	return crypto.Keccak256Hash(data)
}

// runAttemptMatrix 运行一次尝试（基于矩阵，异步模式）
func (c *joyueCoordinatorPrecompile) runAttemptMatrix(evm *EVM, contract *Contract, coordinatorAddr common.Address, batchId common.Hash, attemptNo uint8, participants []Participant, details []IntentDetail, done []bool, active []bool, finals []FinalResult, epoch uint64, roundIdBase uint64, agentShardId uint32, agentContract common.Address) error {
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint8("attemptNo", attemptNo).
		Int("participantsCount", len(participants)).
		Int("detailsCount", len(details)).
		Msg("[JOYUE Coordinator] runAttemptMatrix: Starting scatterAndFreezeMatrix")

	// Scatter 和 Freeze（发出事件，不等待结果）
	// 所有结果收集完成后，会在 collectParticipantResult -> triggerDecision 中自动触发决策
	err := c.scatterAndFreezeMatrix(evm, contract, coordinatorAddr, batchId, attemptNo, participants, details, done, active, epoch, roundIdBase, agentShardId, agentContract)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("batchId", batchId.Hex()).
			Msg("[JOYUE Coordinator] runAttemptMatrix: scatterAndFreezeMatrix failed")
		return err
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Msg("[JOYUE Coordinator] runAttemptMatrix: scatterAndFreezeMatrix completed")

	return nil
}

// scatterAndFreezeMatrix 分散并冻结（异步模式：发出事件，不等待结果）
// 阶段1：发出跨分片请求事件，保存 PendingBatch
func (c *joyueCoordinatorPrecompile) scatterAndFreezeMatrix(evm *EVM, contract *Contract, coordinatorAddr common.Address, batchId common.Hash, attemptNo uint8, participants []Participant, details []IntentDetail, done []bool, active []bool, epoch uint64, roundIdBase uint64, agentShardId uint32, agentContract common.Address) error {
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint8("attemptNo", attemptNo).
		Int("participantsCount", len(participants)).
		Int("detailsCount", len(details)).
		Uint64("blockNumber", evm.BlockNumber.Uint64()).
		Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Starting")

	// 保存 PendingBatch
	pendingBatch := PendingBatch{
		BatchId:       batchId,
		AttemptNo:     attemptNo,
		Participants:  participants,
		Details:       details,
		Done:          done,
		Active:        active,
		CreatedAt:     evm.BlockNumber.Uint64(),
		Epoch:         epoch,
		RoundIdBase:   roundIdBase,
		AgentShardId:  agentShardId,
		AgentContract: agentContract,
	}
	c.savePendingBatch(evm, coordinatorAddr, batchId, pendingBatch)

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint32("agentShardId", agentShardId).
		Str("agentContract", agentContract.Hex()).
		Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Saved PendingBatch")

	// 初始化结果矩阵（所有结果初始化为失败）
	c.initResultMatrix(evm, coordinatorAddr, batchId, participants, len(details))

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Initialized result matrix")

	// 为每个参与者发出跨分片请求事件
	for i, participant := range participants {
		utils.Logger().Info().
			Int("participantIndex", i).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Str("batchId", batchId.Hex()).
			Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Processing participant")

		// 构建该参与者的 ColumnTx
		col, idxMap := c.buildColumnTxsForParticipant(participant, details, done, active)

		utils.Logger().Info().
			Int("participantIndex", i).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Int("columnTxCount", len(col)).
			Int("idxMapLen", len(idxMap)).
			Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Built ColumnTxs for participant")

		// 保存 idxMap 以便回调时映射结果
		if err := c.saveParticipantIdxMap(evm, coordinatorAddr, batchId, participant, idxMap); err != nil {
			utils.Logger().Error().
				Err(err).
				Str("batchId", batchId.Hex()).
				Str("participant", participant.Addr.Hex()).
				Uint32("participantShardId", participant.ShardId).
				Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: saveParticipantIdxMap failed")
		}

		// 如果参与者就是当前分片且合约匹配，直接本地执行
		if participant.ShardId == evm.Context.ShardID && participant.Addr == coordinatorAddr {
			utils.Logger().Info().
				Int("participantIndex", i).
				Str("participant", participant.Addr.Hex()).
				Uint32("participantShardId", participant.ShardId).
				Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Executing locally")

			results, err := c.executeBatchVerifyAndFreeze(evm, contract, coordinatorAddr, batchId, col)
			if err != nil {
				// 执行失败，标记 idxMap 对应交易失败
				guardResults := make([]GuardResult, len(details))
				deltaResults := make([]DeltaResult, len(details))
				for _, idx := range idxMap {
					if idx >= 0 && idx < len(details) {
						guardResults[idx] = GuardResult{Ok: false, GuardFailed: true}
						deltaResults[idx] = DeltaResult{Ok: false}
					}
				}
				_ = c.collectParticipantResult(evm, contract, coordinatorAddr, batchId, participant, guardResults, deltaResults, false)
				continue
			}

			guardResults, deltaResults := c.convertResultsFromColumnResults(results, idxMap, len(details))
			_ = c.collectParticipantResult(evm, contract, coordinatorAddr, batchId, participant, guardResults, deltaResults, true)
			continue
		}

		// 非本地：异步方式发出事件（通过 Executor 预编译处理）
		err := c.emitBatchVerifyAndFreezeRequest(evm, contract, coordinatorAddr, batchId, participant, col)
		if err != nil {
			utils.Logger().Error().
				Err(err).
				Int("participantIndex", i).
				Str("participant", participant.Addr.Hex()).
				Uint32("participantShardId", participant.ShardId).
				Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: emitBatchVerifyAndFreezeRequest failed")
			// 发出事件失败，标记为失败
			c.saveParticipantResult(evm, coordinatorAddr, batchId, participant, nil, nil, false)
		} else {
			utils.Logger().Info().
				Int("participantIndex", i).
				Str("participant", participant.Addr.Hex()).
				Uint32("participantShardId", participant.ShardId).
				Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Successfully emitted BatchVerifyAndFreezeRequest")
		}
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Int("participantsCount", len(participants)).
		Msg("[JOYUE Coordinator] scatterAndFreezeMatrix: Completed for all participants")

	return nil
}

// retryOverride 用于 retry 时覆盖 guards/deltas
type retryOverride struct {
	Guards []Guard
	Deltas []Delta
}

// buildColumnTxsForParticipant 为参与者构建 ColumnTx 数组
func (c *joyueCoordinatorPrecompile) buildColumnTxsForParticipant(participant Participant, details []IntentDetail, done []bool, active []bool) ([]ColumnTx, []int) {
	return c.buildColumnTxsForParticipantWithRetryOverrides(participant, details, done, active, nil)
}

// buildColumnTxsForParticipantWithRetryOverrides 为参与者构建 ColumnTx，支持 retry 时使用 recompute 的 guards/deltas
func (c *joyueCoordinatorPrecompile) buildColumnTxsForParticipantWithRetryOverrides(participant Participant, details []IntentDetail, done []bool, active []bool, retryOverrideMap map[common.Hash]retryOverride) ([]ColumnTx, []int) {
	utils.Logger().Info().
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Int("detailsCount", len(details)).
		Msg("[JOYUE Coordinator] buildColumnTxsForParticipant: Starting")

	var col []ColumnTx
	var idxMap []int

	for i := range details {
		if done[i] || !active[i] {
			continue
		}

		d := &details[i]
		useOverride := false
		var guardsOverride []Guard
		var deltasOverride []Delta
		if retryOverrideMap != nil {
			if ov, ok := retryOverrideMap[d.TxHash]; ok {
				useOverride = true
				guardsOverride = ov.Guards
				deltasOverride = ov.Deltas
			}
		}
		tx := c.buildSingleColumnTx(participant, *d, useOverride, guardsOverride, deltasOverride)
		col = append(col, tx)
		idxMap = append(idxMap, i)
	}

	utils.Logger().Info().
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Int("columnTxCount", len(col)).
		Int("idxMapLen", len(idxMap)).
		Msg("[JOYUE Coordinator] buildColumnTxsForParticipant: Completed")

	return col, idxMap
}

// createFailedGuardResults 创建失败的 Guard 结果
func (c *joyueCoordinatorPrecompile) createFailedGuardResults(details []IntentDetail, done []bool, active []bool) []GuardResult {
	results := make([]GuardResult, len(details))
	for i := range details {
		if !done[i] && active[i] {
			results[i] = GuardResult{
				TxHash:      details[i].TxHash,
				Ok:          false,
				GuardFailed: true,
			}
		}
	}
	return results
}

// createFailedDeltaResults 创建失败的 Delta 结果
func (c *joyueCoordinatorPrecompile) createFailedDeltaResults(details []IntentDetail, done []bool, active []bool) []DeltaResult {
	results := make([]DeltaResult, len(details))
	for i := range details {
		if !done[i] && active[i] {
			results[i] = DeltaResult{
				TxHash: details[i].TxHash,
				Ok:     false,
			}
		}
	}
	return results
}

// buildSingleColumnTx 构建单个 ColumnTx
func (c *joyueCoordinatorPrecompile) buildSingleColumnTx(participant Participant, detail IntentDetail, useOverride bool, guardsOverride []Guard, deltasOverride []Delta) ColumnTx {
	tx := ColumnTx{
		TxHash: detail.TxHash,
	}

	// 选择 Guards
	var guards []Guard
	if useOverride {
		guards = guardsOverride
	} else {
		guards = detail.Guards
	}

	// 过滤出属于该参与者的 Guards
	var columnGuards []ColumnGuard
	for gi, guard := range guards {
		if guard.ContractAddr == participant.Addr && guard.ShardId == participant.ShardId {
			columnGuards = append(columnGuards, ColumnGuard{
				GuardIndex: uint32(gi),
				Guard:      guard,
			})
		}
	}
	tx.Guards = columnGuards

	// 选择 Deltas
	var deltas []Delta
	if useOverride {
		deltas = deltasOverride
	} else {
		deltas = detail.Deltas
	}

	// 过滤出属于该参与者的 Deltas
	for _, delta := range deltas {
		if delta.ContractAddr == participant.Addr && delta.ShardId == participant.ShardId {
			tx.Deltas = append(tx.Deltas, delta)
		}
	}

	return tx
}

// decideFromMatrix 基于 Guard 和 Delta 矩阵进行决策
func (c *joyueCoordinatorPrecompile) decideFromMatrix(attemptNo uint8, participants []Participant, details []IntentDetail, done []bool, active []bool, guardMatrix []GuardMatrixResult, deltaMatrix []DeltaMatrixResult) (MatrixDecision, AttemptResult) {
	dec := MatrixDecision{
		commitTx: make([]common.Hash, len(details)),
		retryTx:  make([]common.Hash, len(details)),
		failTx:   make([]common.Hash, len(details)),
	}
	out := AttemptResult{
		committed:    make([]bool, len(details)),
		needRetry:    make([]bool, len(details)),
		callbackFail: make([]bool, len(details)),
	}

	for i := range details {
		if done[i] || !active[i] {
			continue
		}

		// 检查该交易在所有参与者上的 Guard 和 Delta 结果
		guardAllOk := true
		deltaAllOk := true

		// 检查该交易涉及哪些参与者
		involvedParticipants := c.getInvolvedParticipants(details[i], participants)

		for _, pIdx := range involvedParticipants {
			if pIdx >= len(guardMatrix) || pIdx >= len(deltaMatrix) {
				continue
			}

			// 检查 Guard 结果（只检查涉及该参与者的交易）
			if i < len(guardMatrix[pIdx].TxResults) {
				if !guardMatrix[pIdx].TxResults[i].Ok {
					guardAllOk = false
					break
				}
			}

			// 检查 Delta 结果（只检查涉及该参与者的交易）
			if i < len(deltaMatrix[pIdx].TxResults) {
				if !deltaMatrix[pIdx].TxResults[i].Ok {
					deltaAllOk = false
					break
				}
			}
		}

		// 决策：所有 Guard 验证和 Delta 冻结都成功 → COMMIT
		if guardAllOk && deltaAllOk {
			out.committed[i] = true
			dec.commitTx[dec.commitCount] = details[i].TxHash
			dec.commitCount++
		} else if attemptNo == 1 {
			// 第一轮失败 → Retry（使用最新状态进行逻辑树重试）
			out.needRetry[i] = true
			dec.retryTx[dec.retryCount] = details[i].TxHash
			dec.retryCount++
		} else {
			// 第二轮失败 → 最终失败
			out.callbackFail[i] = true
			dec.failTx[dec.failCount] = details[i].TxHash
			dec.failCount++
		}
	}

	return dec, out
}

// getInvolvedParticipants 获取交易涉及的参与者索引
func (c *joyueCoordinatorPrecompile) getInvolvedParticipants(detail IntentDetail, participants []Participant) []int {
	participantSet := make(map[Participant]bool)
	var involved []int

	// 收集所有涉及的参与者地址
	for _, guard := range detail.Guards {
		participantSet[Participant{Addr: guard.ContractAddr, ShardId: guard.ShardId}] = true
	}
	for _, delta := range detail.Deltas {
		participantSet[Participant{Addr: delta.ContractAddr, ShardId: delta.ShardId}] = true
	}

	// 找到对应的参与者索引
	for i, participant := range participants {
		if participantSet[participant] {
			involved = append(involved, i)
		}
	}

	return involved
}

// finalizeCommitRound 最终化提交轮次
func (c *joyueCoordinatorPrecompile) finalizeCommitRound(details []IntentDetail, committed []bool, finals []FinalResult, done []bool, attemptUsed uint8) {
	for i := range details {
		if done[i] {
			continue
		}
		if committed[i] {
			finals[i] = FinalResult{
				TxHash:       details[i].TxHash,
				Status:       0, // FINAL_COMMIT
				AttemptsUsed: attemptUsed,
			}
			done[i] = true
		}
	}
}

// finalizeFailRetryRound 最终化失败重试轮次
func (c *joyueCoordinatorPrecompile) finalizeFailRetryRound(details []IntentDetail, terminalFailRetry []bool, finals []FinalResult, done []bool) {
	for i := range details {
		if done[i] {
			continue
		}
		if terminalFailRetry[i] {
			finals[i] = FinalResult{
				TxHash:       details[i].TxHash,
				Status:       1, // FINAL_FAIL
				AttemptsUsed: 2,
			}
			done[i] = true
		}
	}
}

// finalizeCommitOrFailRound 最终化提交或失败轮次
func (c *joyueCoordinatorPrecompile) finalizeCommitOrFailRound(details []IntentDetail, committed []bool, fail []bool, finals []FinalResult, done []bool, attemptUsed uint8) {
	for i := range details {
		if done[i] {
			continue
		}
		if committed[i] {
			finals[i] = FinalResult{
				TxHash:       details[i].TxHash,
				Status:       0, // FINAL_COMMIT
				AttemptsUsed: attemptUsed,
			}
		} else if fail[i] {
			finals[i] = FinalResult{
				TxHash:       details[i].TxHash,
				Status:       1, // FINAL_FAIL
				AttemptsUsed: attemptUsed,
			}
		} else {
			finals[i] = FinalResult{
				TxHash:       details[i].TxHash,
				Status:       1, // FINAL_FAIL
				AttemptsUsed: attemptUsed,
			}
		}
		done[i] = true
	}
}

// allDone 检查是否全部完成
func (c *joyueCoordinatorPrecompile) allDone(done []bool) bool {
	for _, d := range done {
		if !d {
			return false
		}
	}
	return true
}

// encodeFinalResults 编码 FinalResult[] 数组
func (c *joyueCoordinatorPrecompile) encodeFinalResults(results []FinalResult) ([]byte, error) {
	// ABI 编码动态数组
	offset := 32 + 32 + len(results)*32 // 偏移量 + 长度 + 元素偏移量
	elementOffsets := make([]int, len(results))
	elementSize := 96 // txHash(32) + status(32) + attemptsUsed(32)

	for i := range results {
		elementOffsets[i] = offset
		offset += elementSize
	}

	output := make([]byte, offset)

	// 写入数组偏移量
	binary.BigEndian.PutUint64(output[24:32], 32)

	// 写入数组长度
	binary.BigEndian.PutUint64(output[56:64], uint64(len(results)))

	// 写入每个元素的偏移量
	for i, elemOffset := range elementOffsets {
		offsetPos := 64 + i*32
		binary.BigEndian.PutUint64(output[offsetPos+24:offsetPos+32], uint64(elemOffset))
	}

	// 写入每个元素的数据
	for i, result := range results {
		elemPos := elementOffsets[i]
		// txHash (bytes32)
		copy(output[elemPos:elemPos+32], result.TxHash.Bytes())
		// status (uint8) - 32-64 字节（左填充）
		output[elemPos+63] = result.Status
		// attemptsUsed (uint8) - 64-96 字节（左填充）
		output[elemPos+95] = result.AttemptsUsed
	}

	return output, nil
}

// ========== 异步 2PC 辅助函数 ==========

// savePendingBatch 保存 PendingBatch 到存储
func (c *joyueCoordinatorPrecompile) savePendingBatch(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, batch PendingBatch) {
	// 使用 RLP 编码存储
	encoded, err := rlp.EncodeToBytes(batch)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[JOYUE] failed to encode PendingBatch")
		return
	}

	// 存储到 mapping(bytes32 => bytes) _pendingBatches
	baseSlot := getMappingSlot(big.NewInt(storageSlotPendingBatch), batchId)

	// 使用 Blob 模式存储（类似 _frozenDeltas）
	// 前 4 字节存储长度，后续存储数据
	lengthBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(lengthBytes[0:4], uint32(len(encoded)))
	evm.StateDB.SetState(coordinatorAddr, baseSlot, common.BytesToHash(lengthBytes))

	// 存储数据（每个 slot 32 字节）
	slotsNeeded := (len(encoded) + 31) / 32
	for i := 0; i < slotsNeeded; i++ {
		slot := new(big.Int).Set(baseSlot.Big())
		slot.Add(slot, big.NewInt(int64(i+1)))
		slotHash := common.BigToHash(slot)

		start := i * 32
		end := start + 32
		if end > len(encoded) {
			end = len(encoded)
		}
		slotData := make([]byte, 32)
		copy(slotData, encoded[start:end])
		evm.StateDB.SetState(coordinatorAddr, slotHash, common.BytesToHash(slotData))
	}
}

// getPendingBatch 从存储读取 PendingBatch
func (c *joyueCoordinatorPrecompile) getPendingBatch(evm *EVM, coordinatorAddr common.Address, batchId common.Hash) (*PendingBatch, error) {
	baseSlot := getMappingSlot(big.NewInt(storageSlotPendingBatch), batchId)
	firstSlot := evm.StateDB.GetState(coordinatorAddr, baseSlot)

	// 读取长度
	length := binary.BigEndian.Uint32(firstSlot[0:4])
	if length == 0 {
		return nil, errors.New("JOYUE: PendingBatch not found")
	}
	if length > maxBlobSize {
		return nil, fmt.Errorf("JOYUE: corrupted PendingBatch length %d exceeds max %d", length, maxBlobSize)
	}

	// 读取数据
	slotsNeeded := (int(length) + 31) / 32
	encoded := make([]byte, 0, length)
	for i := 0; i < slotsNeeded; i++ {
		slot := new(big.Int).Set(baseSlot.Big())
		slot.Add(slot, big.NewInt(int64(i+1)))
		slotHash := common.BigToHash(slot)
		slotData := evm.StateDB.GetState(coordinatorAddr, slotHash).Bytes()

		remaining := int(length) - i*32
		if remaining <= 0 {
			break
		}
		if remaining > 32 {
			remaining = 32
		}
		encoded = append(encoded, slotData[:remaining]...)
	}

	// RLP 解码
	var batch PendingBatch
	err := rlp.DecodeBytes(encoded, &batch)
	if err != nil {
		return nil, err
	}

	return &batch, nil
}

// initResultMatrix 初始化结果矩阵
func (c *joyueCoordinatorPrecompile) initResultMatrix(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, participants []Participant, txCount int) {
	// 为每个参与者初始化结果（标记为未收集）
	for _, participant := range participants {
		// 使用 mapping(bytes32 => mapping(address => bool)) 标记是否已收集
		// 简化：使用 mapping(bytes32 => mapping(address => bytes32)) 存储结果哈希
		participantSlot := getNestedMappingSlot(big.NewInt(storageSlotResultMatrix), batchId, participantKey(participant))
		// 初始化为 0（未收集）
		evm.StateDB.SetState(coordinatorAddr, participantSlot, common.Hash{})
	}
}

// saveParticipantResult 保存参与者结果
func (c *joyueCoordinatorPrecompile) saveParticipantResult(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, participant Participant, guardResults []GuardResult, deltaResults []DeltaResult, success bool) {
	// 使用 RLP 编码结果
	result := ParticipantResult{
		Participant:  participant,
		GuardResults: guardResults,
		DeltaResults: deltaResults,
		Success:      success,
	}
	encoded, err := rlp.EncodeToBytes(result)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[JOYUE] failed to encode ParticipantResult")
		return
	}

	// 存储结果哈希（用于快速检查是否已收集）
	participantSlot := getNestedMappingSlot(big.NewInt(storageSlotResultMatrix), batchId, participantKey(participant))
	resultHash := crypto.Keccak256Hash(encoded)
	evm.StateDB.SetState(coordinatorAddr, participantSlot, resultHash)

	// 使用 Blob 模式存储完整结果（类似 PendingBatch）
	blobBaseSlot := getBlobSlotWithDomain(blobDomainParticipantResult, batchId, participantKey(participant))
	lengthBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(lengthBytes[0:4], uint32(len(encoded)))
	evm.StateDB.SetState(coordinatorAddr, blobBaseSlot, common.BytesToHash(lengthBytes))

	slotsNeeded := (len(encoded) + 31) / 32
	for i := 0; i < slotsNeeded; i++ {
		slot := new(big.Int).Set(blobBaseSlot.Big())
		slot.Add(slot, big.NewInt(int64(i+1)))
		slotHash := common.BigToHash(slot)

		start := i * 32
		end := start + 32
		if end > len(encoded) {
			end = len(encoded)
		}
		slotData := make([]byte, 32)
		copy(slotData, encoded[start:end])
		evm.StateDB.SetState(coordinatorAddr, slotHash, common.BytesToHash(slotData))
	}
}

// getParticipantResult 获取参与者结果
func (c *joyueCoordinatorPrecompile) getParticipantResult(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, participant Participant) (*ParticipantResult, error) {
	// 检查是否已收集
	participantSlot := getNestedMappingSlot(big.NewInt(storageSlotResultMatrix), batchId, participantKey(participant))
	resultHash := evm.StateDB.GetState(coordinatorAddr, participantSlot)
	if resultHash == (common.Hash{}) {
		return nil, errors.New("JOYUE: ParticipantResult not found")
	}

	// 读取 Blob 数据
	blobBaseSlot := getBlobSlotWithDomain(blobDomainParticipantResult, batchId, participantKey(participant))
	firstSlot := evm.StateDB.GetState(coordinatorAddr, blobBaseSlot)
	length := binary.BigEndian.Uint32(firstSlot[0:4])
	if length > maxBlobSize {
		return nil, fmt.Errorf("JOYUE: corrupted ParticipantResult length %d exceeds max %d", length, maxBlobSize)
	}

	slotsNeeded := (int(length) + 31) / 32
	encoded := make([]byte, 0, length)
	for i := 0; i < slotsNeeded; i++ {
		slot := new(big.Int).Set(blobBaseSlot.Big())
		slot.Add(slot, big.NewInt(int64(i+1)))
		slotHash := common.BigToHash(slot)
		slotData := evm.StateDB.GetState(coordinatorAddr, slotHash).Bytes()

		remaining := int(length) - i*32
		if remaining <= 0 {
			break
		}
		if remaining > 32 {
			remaining = 32
		}
		encoded = append(encoded, slotData[:remaining]...)
	}

	// RLP 解码
	var result ParticipantResult
	err := rlp.DecodeBytes(encoded, &result)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

// saveParticipantIdxMap 保存参与者的 idxMap（RLP 编码）
func (c *joyueCoordinatorPrecompile) saveParticipantIdxMap(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, participant Participant, idxMap []int) error {
	idxU64 := make([]uint64, len(idxMap))
	for i, v := range idxMap {
		if v < 0 {
			return errors.New("JOYUE: invalid idxMap value")
		}
		idxU64[i] = uint64(v)
	}
	encoded, err := rlp.EncodeToBytes(idxU64)
	if err != nil {
		return err
	}
	baseSlot := getBlobSlotWithDomain(blobDomainIdxMap, batchId, participantIdxMapKey(participant))
	lengthBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(lengthBytes[0:4], uint32(len(encoded)))
	evm.StateDB.SetState(coordinatorAddr, baseSlot, common.BytesToHash(lengthBytes))

	slotsNeeded := (len(encoded) + 31) / 32
	for i := 0; i < slotsNeeded; i++ {
		slot := new(big.Int).Set(baseSlot.Big())
		slot.Add(slot, big.NewInt(int64(i+1)))
		slotHash := common.BigToHash(slot)

		start := i * 32
		end := start + 32
		if end > len(encoded) {
			end = len(encoded)
		}
		slotData := make([]byte, 32)
		copy(slotData, encoded[start:end])
		evm.StateDB.SetState(coordinatorAddr, slotHash, common.BytesToHash(slotData))
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Int("idxMapLen", len(idxMap)).
		Int("slotsNeeded", slotsNeeded).
		Msg("[JOYUE Coordinator] saveParticipantIdxMap: saved idxMap")

	return nil
}

// getParticipantIdxMap 读取参与者 idxMap
func (c *joyueCoordinatorPrecompile) getParticipantIdxMap(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, participant Participant) ([]int, error) {
	baseSlot := getBlobSlotWithDomain(blobDomainIdxMap, batchId, participantIdxMapKey(participant))
	firstSlot := evm.StateDB.GetState(coordinatorAddr, baseSlot)
	length := binary.BigEndian.Uint32(firstSlot[0:4])
	if length == 0 {
		return nil, errors.New("JOYUE: Participant idxMap not found")
	}
	if length > maxBlobSize {
		return nil, fmt.Errorf("JOYUE: corrupted idxMap length %d exceeds max %d", length, maxBlobSize)
	}

	slotsNeeded := (int(length) + 31) / 32
	encoded := make([]byte, 0, length)
	for i := 0; i < slotsNeeded; i++ {
		slot := new(big.Int).Set(baseSlot.Big())
		slot.Add(slot, big.NewInt(int64(i+1)))
		slotHash := common.BigToHash(slot)
		slotData := evm.StateDB.GetState(coordinatorAddr, slotHash).Bytes()

		remaining := int(length) - i*32
		if remaining <= 0 {
			break
		}
		if remaining > 32 {
			remaining = 32
		}
		encoded = append(encoded, slotData[:remaining]...)
	}

	var idxU64 []uint64
	if err := rlp.DecodeBytes(encoded, &idxU64); err != nil {
		return nil, err
	}
	idxMap := make([]int, len(idxU64))
	for i, v := range idxU64 {
		idxMap[i] = int(v)
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Int("idxMapLen", len(idxMap)).
		Int("slotsNeeded", slotsNeeded).
		Msg("[JOYUE Coordinator] getParticipantIdxMap: loaded idxMap")

	return idxMap, nil
}

// checkAllResultsCollected 检查是否所有结果都已收集
func (c *joyueCoordinatorPrecompile) checkAllResultsCollected(evm *EVM, coordinatorAddr common.Address, batchId common.Hash, participants []Participant) bool {
	for _, participant := range participants {
		participantSlot := getNestedMappingSlot(big.NewInt(storageSlotResultMatrix), batchId, participantKey(participant))
		resultHash := evm.StateDB.GetState(coordinatorAddr, participantSlot)
		if resultHash == (common.Hash{}) {
			return false
		}
	}
	return true
}

// isLocalParticipant 判断参与者是否在本地分片

// toolContractsConfig 工具合约地址配置（与 cmd/joyue-deploy-all-shards/main.go 中的结构一致）
type toolContractsConfig struct {
	Shards map[string]shardToolContracts `json:"shards"`
}

// shardToolContracts 单个分片的工具合约地址
type shardToolContracts struct {
	CacheAddr     string            `json:"cacheAddr,omitempty"`
	RpcOracleAddr string            `json:"rpcOracleAddr,omitempty"`
	Contracts     map[string]string `json:"contracts,omitempty"`
}

// getTargetShardID 获取合约地址所在的分片 ID
// 优先从 joyue-tool-contracts.json 配置文件中查找，如果找不到则使用地址哈希算法
func (c *joyueCoordinatorPrecompile) getTargetShardID(evm *EVM, contractAddr common.Address) uint32 {
	// 尝试从配置文件读取
	configPath := "joyue-tool-contracts.json"
	if shardID, found := c.findShardIDFromConfig(configPath, contractAddr); found {
		return shardID
	}

	// 如果配置文件中找不到，使用地址哈希算法（与 Harmony 的分片分配算法一致）
	// Harmony 使用地址的最后字节进行分片分配
	return uint32(contractAddr[19]) % evm.Context.NumShards
}

// findShardIDFromConfig 从配置文件中查找合约地址所在的分片 ID
func (c *joyueCoordinatorPrecompile) findShardIDFromConfig(configPath string, contractAddr common.Address) (uint32, bool) {
	// 读取配置文件
	data, err := os.ReadFile(configPath)
	if err != nil {
		// 文件不存在或读取失败，返回 false
		return 0, false
	}

	var config toolContractsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		// 解析失败，返回 false
		return 0, false
	}

	contractAddrHex := contractAddr.Hex()

	// 遍历所有分片，查找匹配的合约地址
	for shardKey, shardConfig := range config.Shards {
		// 检查 cacheAddr
		if shardConfig.CacheAddr == contractAddrHex {
			if shardID, err := strconv.ParseUint(shardKey, 10, 32); err == nil {
				return uint32(shardID), true
			}
		}
		// 检查 rpcOracleAddr
		if shardConfig.RpcOracleAddr == contractAddrHex {
			if shardID, err := strconv.ParseUint(shardKey, 10, 32); err == nil {
				return uint32(shardID), true
			}
		}
		// 检查其他合约
		if shardConfig.Contracts != nil {
			for _, addr := range shardConfig.Contracts {
				if addr == contractAddrHex {
					if shardID, err := strconv.ParseUint(shardKey, 10, 32); err == nil {
						return uint32(shardID), true
					}
				}
			}
		}
	}

	return 0, false
}

// emitBatchVerifyAndFreezeRequest 发出跨分片 batchVerifyAndFreeze 请求
// 使用 Executor 预编译统一收口
func (c *joyueCoordinatorPrecompile) emitBatchVerifyAndFreezeRequest(evm *EVM, contract *Contract, coordinatorAddr common.Address, batchId common.Hash, participant Participant, col []ColumnTx) error {
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Int("columnTxCount", len(col)).
		Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: Starting")

	// 编码 ColumnTx[] 为 calldata
	calldata, err := c.encodeColumnTxsForCall(col)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("batchId", batchId.Hex()).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: encodeColumnTxsForCall failed")
		return err
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Int("calldataLen", len(calldata)).
		Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: Encoded ColumnTxs")

	// 构建 batchVerifyAndFreeze 的完整 calldata
	// 函数选择器: batchVerifyAndFreeze(bytes32,ColumnTx[])
	// ABI 编码格式：
	// - selector (4 bytes)
	// - batchId (32 bytes) - 固定类型
	// - txs offset (32 bytes) - 动态数组的 offset，相对于参数区域起始位置
	// - txs data (length + element offsets + element data) - encodeColumnTxsForCall 返回的数据部分
	batchVerifyAndFreezeSelector := []byte{0x94, 0xef, 0x8a, 0xd1}

	// 计算 txs offset：相对于参数区域起始位置（selector + batchId = 4 + 32 = 36）
	// offset 值 = 跳过 batchId (32) + offset 本身 (32) = 64
	txsOffset := uint64(64)

	targetCalldata := make([]byte, 0, 4+32+32+len(calldata))
	targetCalldata = append(targetCalldata, batchVerifyAndFreezeSelector...)
	targetCalldata = append(targetCalldata, batchId.Bytes()...)
	// 添加 txs offset (32 bytes)
	targetCalldata = append(targetCalldata, make([]byte, 32)...)
	binary.BigEndian.PutUint64(targetCalldata[len(targetCalldata)-8:], txsOffset)
	// 添加 txs data（length + element offsets + element data）
	targetCalldata = append(targetCalldata, calldata...)

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Hex("selector", batchVerifyAndFreezeSelector).
		Int("targetCalldataLen", len(targetCalldata)).
		Hex("targetCalldata0-4", targetCalldata[0:min(4, len(targetCalldata))]).
		Hex("targetCalldata4-36", targetCalldata[4:min(36, len(targetCalldata))]).
		Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: Built batchVerifyAndFreeze calldata")

	// 计算目标分片 ID：优先从配置文件读取，否则使用地址哈希
	targetShardID := participant.ShardId
	sourceShardID := evm.Context.ShardID

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Str("targetContract", participant.Addr.Hex()).
		Uint32("targetShardID", targetShardID).
		Uint32("sourceShardID", sourceShardID).
		Int("targetCalldataLen", len(targetCalldata)).
		Hex("targetCalldataSelector", targetCalldata[0:min(4, len(targetCalldata))]).
		Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: Prepared target calldata - Will call participant's Coordinator contract")

	// 回调地址和选择器
	callbackAddr := coordinatorAddr
	// 使用统一的回调函数: onCrossShardCallback(uint256,bool,bytes)
	// 函数选择器: keccak256("onCrossShardCallback(uint256,bool,bytes)")[:4]
	// 计算: crypto.Keccak256([]byte("onCrossShardCallback(uint256,bool,bytes)"))[:4]
	// 结果: 0x8f4ffcb1 (需要验证，但先使用这个值)
	callbackSelector := []byte{0x8f, 0x4f, 0xfc, 0xb1} // onCrossShardCallback(uint256,bool,bytes)

	// 生成 requestId（用于事件和回调识别）
	// 使用 Keccak256(batchId, participantKey, blockNumber) 的前 16 字节作为 requestId
	requestIdData := append(batchId.Bytes(), participantKey(participant).Bytes()...)
	requestIdData = append(requestIdData, evm.BlockNumber.Bytes()...)
	requestIdHash := crypto.Keccak256Hash(requestIdData)
	requestId := new(big.Int).SetBytes(requestIdHash[:16]).Uint64() // 使用前 16 字节，避免溢出

	// 保存 requestId 到 batchId 和 participant 的映射
	c.saveRequestIdMapping(evm, coordinatorAddr, requestId, batchId, participant)

	// 通过 RPC precompile (0x6D) 发送到目标分片的 Executor 预编译 (0x74)
	// 构建 Executor.executeAndCallback 的 calldata
	// executeAndCallback 选择器: 0x2b5d76a4
	executorSelector := []byte{0x2b, 0x5d, 0x76, 0xa4} // executeAndCallback 选择器
	executorCalldata := make([]byte, 0, 4+4+20+4+32+20+32+32+len(targetCalldata))
	executorCalldata = append(executorCalldata, executorSelector...)
	// sourceShardID (uint32, 4 bytes, 左填充到 32 bytes)
	sourceShardIDBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(sourceShardIDBytes[28:32], sourceShardID)
	executorCalldata = append(executorCalldata, sourceShardIDBytes...)
	// callbackAddr (address, 20 bytes, 左填充到 32 bytes)
	callbackAddrBytes := make([]byte, 32)
	copy(callbackAddrBytes[12:32], callbackAddr.Bytes())
	executorCalldata = append(executorCalldata, callbackAddrBytes...)
	// callbackSelector (bytes4, 4 bytes, 左填充到 32 bytes)
	callbackSelectorBytes := make([]byte, 32)
	copy(callbackSelectorBytes[0:4], callbackSelector)
	executorCalldata = append(executorCalldata, callbackSelectorBytes...)
	// requestId (uint256, 32 bytes)
	requestIdBigInt := new(big.Int).SetUint64(requestId)
	requestIdBytes := make([]byte, 32)
	requestIdBigInt.FillBytes(requestIdBytes)
	executorCalldata = append(executorCalldata, requestIdBytes...)
	// target (address, 20 bytes, 左填充到 32 bytes)
	targetBytes := make([]byte, 32)
	copy(targetBytes[12:32], participant.Addr.Bytes())
	executorCalldata = append(executorCalldata, targetBytes...)
	// targetValue (uint256, 32 bytes)
	targetValueBytes := make([]byte, 32)
	executorCalldata = append(executorCalldata, targetValueBytes...)
	// targetCalldata (bytes, 动态类型)
	// 偏移量 (32 bytes)
	calldataOffsetBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(calldataOffsetBytes[24:32], 224) // 7 * 32 = 224
	executorCalldata = append(executorCalldata, calldataOffsetBytes...)
	// 长度 (32 bytes)
	calldataLenBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(calldataLenBytes[24:32], uint64(len(targetCalldata)))
	executorCalldata = append(executorCalldata, calldataLenBytes...)
	// 数据（左填充到 32 字节的倍数）
	paddedLen := (len(targetCalldata) + 31) / 32 * 32
	paddedData := make([]byte, paddedLen)
	copy(paddedData, targetCalldata)
	executorCalldata = append(executorCalldata, paddedData...)

	// Executor 预编译地址 (0x74)
	executorAddr := common.BytesToAddress([]byte{116}) // 0x74

	// 转换 callbackSelector 为 [4]byte
	var callbackSelectorArray [4]byte
	copy(callbackSelectorArray[:], callbackSelector)

	// 复用统一的事件发出逻辑
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Uint32("targetShardID", targetShardID).
		Str("executorAddr", executorAddr.Hex()).
		Uint64("requestId", requestId).
		Int("executorCalldataLen", len(executorCalldata)).
		Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: Calling emitCrossShardRequestEvent")

	_, err = emitCrossShardRequestEvent(evm, contract, targetShardID, executorAddr, big.NewInt(0), executorCalldata, callbackAddr, callbackSelectorArray, requestId)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("batchId", batchId.Hex()).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: emitCrossShardRequestEvent failed")
		return err
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Uint32("targetShardID", targetShardID).
		Str("executorAddr", executorAddr.Hex()).
		Uint64("requestId", requestId).
		Msg("[JOYUE Coordinator] emitBatchVerifyAndFreezeRequest: Successfully emitted BatchVerifyAndFreezeRequest via Executor precompile")

	return nil
}

// emitAgentResultToAgent 向原 Agent 发送 Intent 执行结果（成功或失败）
// 通过跨分片调用 agent.onIntentResult(bytes32 requestId, bool success, bytes memory data)
// commitCompletionType/failCompletionType: 1-5，用于 AgentResultEmitted 事件
func (c *joyueCoordinatorPrecompile) emitAgentResultToAgent(evm *EVM, contract *Contract, coordinatorAddr common.Address, agentShardId uint32, agentContract common.Address, commitTxHashes []common.Hash, failTxHashes []common.Hash, commitCompletionType, failCompletionType uint8) {
	if agentContract == (common.Address{}) {
		return
	}
	executorAddr := common.BytesToAddress([]byte{116}) // 0x74
	callbackSelector := []byte{0x8f, 0x4f, 0xfc, 0xb1} // onCrossShardCallback(uint256,bool,bytes)
	masterShardId := evm.Context.ShardID

	// onIntentResult(bytes32,bool,bytes) 选择器
	onIntentResultSelector := crypto.Keccak256([]byte("onIntentResult(bytes32,bool,bytes)"))[:4]

	for _, txHash := range commitTxHashes {
		c.emitSingleAgentResult(evm, contract, coordinatorAddr, agentShardId, agentContract, executorAddr, masterShardId, callbackSelector, txHash, true, commitCompletionType, onIntentResultSelector)
	}
	for _, txHash := range failTxHashes {
		c.emitSingleAgentResult(evm, contract, coordinatorAddr, agentShardId, agentContract, executorAddr, masterShardId, callbackSelector, txHash, false, failCompletionType, onIntentResultSelector)
	}
}

// packAgentResultEmittedData 标准 ABI 编码 (bool success, uint8 completionType)，共 64 字节
func packAgentResultEmittedData(success bool, completionType uint8) []byte {
	data := make([]byte, 64)
	if success {
		data[31] = 1
	}
	data[63] = completionType
	return data
}

// emitSingleAgentResult 发送单条结果到 Agent
func (c *joyueCoordinatorPrecompile) emitSingleAgentResult(evm *EVM, contract *Contract, coordinatorAddr common.Address, agentShardId uint32, agentContract common.Address, executorAddr common.Address, masterShardId uint32, callbackSelector []byte, txHash common.Hash, success bool, completionType uint8, onIntentResultSelector []byte) {
	// 构建 agent.onIntentResult(txHash, success, "") 的 calldata
	// ABI: bytes4 selector + bytes32 requestId + bool success + bytes data(offset, len, empty)
	agentCalldata := make([]byte, 0, 4+32+32+32+32)
	agentCalldata = append(agentCalldata, onIntentResultSelector...)
	agentCalldata = append(agentCalldata, txHash.Bytes()...) // requestId = txHash
	successBytes := make([]byte, 32)
	if success {
		successBytes[31] = 1
	}
	agentCalldata = append(agentCalldata, successBytes...)
	// bytes data: offset=96 (3*32), length=0
	offsetBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(offsetBytes[24:32], 96)
	agentCalldata = append(agentCalldata, offsetBytes...)
	lenBytes := make([]byte, 32) // length=0
	agentCalldata = append(agentCalldata, lenBytes...)

	// requestId 用于回调：不保存映射，回调时 requestId not found 会静默忽略
	requestId := new(big.Int).SetBytes(txHash[:8]).Uint64()

	// 构建 executor.executeAndCallback 的 calldata
	executorSelector := []byte{0x2b, 0x5d, 0x76, 0xa4}
	executorCalldata := make([]byte, 0, 4+32*7+len(agentCalldata))
	executorCalldata = append(executorCalldata, executorSelector...)
	sourceShardIDBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(sourceShardIDBytes[28:32], masterShardId)
	executorCalldata = append(executorCalldata, sourceShardIDBytes...)
	callbackAddrBytes := make([]byte, 32)
	copy(callbackAddrBytes[12:32], coordinatorAddr.Bytes())
	executorCalldata = append(executorCalldata, callbackAddrBytes...)
	callbackSelectorBytes := make([]byte, 32)
	copy(callbackSelectorBytes[0:4], callbackSelector)
	executorCalldata = append(executorCalldata, callbackSelectorBytes...)
	requestIdBytes := make([]byte, 32)
	new(big.Int).SetUint64(requestId).FillBytes(requestIdBytes)
	executorCalldata = append(executorCalldata, requestIdBytes...)
	targetBytes := make([]byte, 32)
	copy(targetBytes[12:32], agentContract.Bytes())
	executorCalldata = append(executorCalldata, targetBytes...)
	executorCalldata = append(executorCalldata, make([]byte, 32)...) // targetValue=0
	calldataOffsetBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(calldataOffsetBytes[24:32], 224)
	executorCalldata = append(executorCalldata, calldataOffsetBytes...)
	calldataLenBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(calldataLenBytes[24:32], uint64(len(agentCalldata)))
	executorCalldata = append(executorCalldata, calldataLenBytes...)
	paddedLen := (len(agentCalldata) + 31) / 32 * 32
	paddedData := make([]byte, paddedLen)
	copy(paddedData, agentCalldata)
	executorCalldata = append(executorCalldata, paddedData...)

	var callbackSelectorArray [4]byte
	copy(callbackSelectorArray[:], callbackSelector)

	if _, err := emitCrossShardRequestEvent(evm, contract, agentShardId, executorAddr, big.NewInt(0), executorCalldata, coordinatorAddr, callbackSelectorArray, requestId); err != nil {
		utils.Logger().Error().Err(err).
			Str("agentContract", agentContract.Hex()).
			Uint32("agentShardId", agentShardId).
			Str("txHash", txHash.Hex()).
			Bool("success", success).
			Msg("[JOYUE Coordinator] emitAgentResultToAgent: failed")
		return
	}
	// 发出 AgentResultEmitted 事件（用于 joyue-metrics 指标统计）
	evm.StateDB.AddLog(&types.Log{
		Address:     coordinatorAddr,
		Topics:      []common.Hash{agentResultEmittedEventSig, txHash},
		Data:        packAgentResultEmittedData(success, completionType),
		BlockNumber: evm.BlockNumber.Uint64(),
	})
	utils.Logger().Info().
		Str("agentContract", agentContract.Hex()).
		Uint32("agentShardId", agentShardId).
		Str("txHash", txHash.Hex()).
		Bool("success", success).
		Uint8("completionType", completionType).
		Msg("[JOYUE Coordinator] emitAgentResultToAgent: sent result to agent")
}

// convertResults 转换 ColumnResult 为 GuardResult 和 DeltaResult
func (c *joyueCoordinatorPrecompile) convertResults(results []ColumnResult, totalDetails int) ([]GuardResult, []DeltaResult) {
	guardResults := make([]GuardResult, totalDetails)
	deltaResults := make([]DeltaResult, totalDetails)

	for i, result := range results {
		if i >= totalDetails {
			break
		}
		guardResults[i] = GuardResult{
			TxHash:           result.TxHash,
			Ok:               result.Ok && !result.GuardFailed,
			GuardFailed:      result.GuardFailed,
			FailedGuardIndex: result.FailedGuardIndex,
			LatestKey:        result.LatestKey,
			LatestVal:        result.LatestVal,
			LatestVer:        result.LatestVer,
		}
		deltaResults[i] = DeltaResult{
			TxHash:           result.TxHash,
			Ok:               result.Ok && !result.GuardFailed,
			FailedDeltaIndex: result.FailedDeltaIndex,
			LatestKey:        result.LatestKey,
			LatestVal:        result.LatestVal,
			LatestVer:        result.LatestVer,
		}
	}

	return guardResults, deltaResults
}

// encodeColumnTxsForCall 编码 ColumnTx[] 为 calldata（用于跨分片调用）
// 注意：只返回数组的数据部分（length + element offsets + element data），不包含 offset
// offset 由调用者根据实际参数位置手动添加
// ABI 编码格式：动态数组的数据部分
// - length (32 bytes) - 数组长度
// - element offsets (32 bytes each) - 每个元素的偏移量（相对于数组 head，即 length 之后的位置）
// - element data - 每个元素的实际数据
func (c *joyueCoordinatorPrecompile) encodeColumnTxsForCall(col []ColumnTx) ([]byte, error) {
	if len(col) == 0 {
		// 空数组：只返回 length (32 bytes)，值为 0
		result := make([]byte, 32)
		// length = 0
		return result, nil
	}

	// 计算总大小（不包含 offset）
	// 固定部分：length (32) + element offsets (32 * len(col))
	fixedSize := 32 + 32*len(col)

	// 编码每个 ColumnTx，计算数据区域大小
	dataArea := make([]byte, 0)
	elementOffsets := make([]int, len(col))

	for i, tx := range col {
		txData, err := c.encodeColumnTx(tx)
		if err != nil {
			return nil, err
		}
		// element offset 相对于数组 head（length 之后的位置）
		// 数组 head = length (32 bytes)
		// element offsets 区域 = 32 * len(col) bytes
		// element data 区域起始位置 = 32 + 32*len(col)
		// 所以 element offset = 32 + 32*len(col) + len(dataArea before this element)
		elementOffsets[i] = fixedSize + len(dataArea)
		dataArea = append(dataArea, txData...)
	}

	// 构建最终结果（不包含 offset）
	result := make([]byte, 0, fixedSize+len(dataArea))

	// length (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(len(col)))

	// element offsets (32 bytes each)
	// 注意：element offset 是相对于数组 head（length 之后的位置）的
	for _, offset := range elementOffsets {
		result = append(result, make([]byte, 32)...)
		binary.BigEndian.PutUint64(result[len(result)-8:], uint64(offset))
	}

	// element data
	result = append(result, dataArea...)

	return result, nil
}

// encodeColumnTx 编码单个 ColumnTx 结构
// ColumnTx 结构：txHash (bytes32) + guards (ColumnGuard[]) + deltas (Delta[])
// guards 和 deltas 是动态数组，所以 ColumnTx 是动态结构
func (c *joyueCoordinatorPrecompile) encodeColumnTx(tx ColumnTx) ([]byte, error) {
	// 固定部分：txHash (32) + guards offset (32) + deltas offset (32) = 96 bytes
	fixedSize := 96

	// 编码 guards 和 deltas
	guardsData, err := c.encodeColumnGuards(tx.Guards)
	if err != nil {
		return nil, err
	}
	deltasData, err := c.encodeDeltas(tx.Deltas)
	if err != nil {
		return nil, err
	}

	// 计算偏移量
	guardsOffset := fixedSize
	deltasOffset := fixedSize + len(guardsData)

	// 构建结果
	result := make([]byte, 0, fixedSize+len(guardsData)+len(deltasData))

	// txHash (32 bytes)
	result = append(result, tx.TxHash.Bytes()...)

	// guards offset (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(guardsOffset))

	// deltas offset (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(deltasOffset))

	// guards data
	result = append(result, guardsData...)

	// deltas data
	result = append(result, deltasData...)

	return result, nil
}

// encodeColumnGuards 编码 ColumnGuard[] 数组
// 注意：只返回数组的数据部分（length + element offsets + element data），不包含 leading offset。
// 父级 encodeColumnTx 已写入 guards 的 offset，解码器会直接跳到此处并期望第一个 32 字节为 length。
func (c *joyueCoordinatorPrecompile) encodeColumnGuards(guards []ColumnGuard) ([]byte, error) {
	if len(guards) == 0 {
		result := make([]byte, 32)
		// length = 0，不写 offset
		return result, nil
	}

	// 数据部分：length (32) + element offsets (32*len) + element data
	fixedSize := 32 + 32*len(guards)

	dataArea := make([]byte, 0)
	elementOffsets := make([]int, len(guards))

	for i, cg := range guards {
		elementOffsets[i] = fixedSize + len(dataArea)
		cgData, err := c.encodeColumnGuard(cg)
		if err != nil {
			return nil, err
		}
		dataArea = append(dataArea, cgData...)
	}

	result := make([]byte, 0, fixedSize+len(dataArea))

	// length (32 bytes)，不写 leading offset
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(len(guards)))

	// element offsets（相对于数组 head，即 length 之后）
	for _, offset := range elementOffsets {
		result = append(result, make([]byte, 32)...)
		binary.BigEndian.PutUint64(result[len(result)-8:], uint64(offset))
	}

	// element data
	result = append(result, dataArea...)

	return result, nil
}

// encodeColumnGuard 编码单个 ColumnGuard 结构
// ColumnGuard 结构：guardIndex (uint32) + guard (Guard)
// guard 是动态结构，所以 ColumnGuard 是动态结构
func (c *joyueCoordinatorPrecompile) encodeColumnGuard(cg ColumnGuard) ([]byte, error) {
	fixedSize := 64 // guardIndex (32) + guard offset (32)

	guardData, err := c.encodeGuard(cg.Guard)
	if err != nil {
		return nil, err
	}

	guardOffset := fixedSize

	result := make([]byte, 0, fixedSize+len(guardData))

	// guardIndex (uint32, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint32(result[len(result)-4:], cg.GuardIndex)

	// guard offset (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(guardOffset))

	// guard data
	result = append(result, guardData...)

	return result, nil
}

// encodeGuard 编码单个 Guard 结构
// Guard 结构：contractAddr (address) + shardId (uint32) + key (bytes32) + readVersion (uint64) + strategy (uint8) + op (uint8) + val (bytes)
// val 是动态类型，所以 Guard 是动态结构
func (c *joyueCoordinatorPrecompile) encodeGuard(guard Guard) ([]byte, error) {
	fixedSize := 224 // contractAddr (32) + shardId (32) + key (32) + readVersion (32) + strategy (32) + op (32) + val offset (32)

	// 编码 val (bytes)
	valPaddedLen := (len(guard.Val) + 31) / 32 * 32
	valSize := 32 + 32 + valPaddedLen // length (32) + data (padded)

	valOffset := fixedSize

	result := make([]byte, 0, fixedSize+valSize)

	// contractAddr (address, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	copy(result[len(result)-20:], guard.ContractAddr.Bytes())

	// shardId (uint32, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint32(result[len(result)-4:], guard.ShardId)

	// key (bytes32)
	result = append(result, guard.Key.Bytes()...)

	// readVersion (uint64, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], guard.ReadVersion)

	// strategy (uint8, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	result[len(result)-1] = guard.Strategy

	// op (uint8, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	result[len(result)-1] = guard.Op

	// val offset (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(valOffset))

	// val length (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(len(guard.Val)))

	// val data (padded to 32 bytes)
	valPadded := make([]byte, valPaddedLen)
	copy(valPadded, guard.Val)
	result = append(result, valPadded...)

	return result, nil
}

// encodeDeltas 编码 Delta[] 数组
// 注意：只返回数组的数据部分（length + element offsets + element data），不包含 leading offset。
// 父级 encodeColumnTx 已写入 deltas 的 offset，解码器会直接跳到此处并期望第一个 32 字节为 length。
func (c *joyueCoordinatorPrecompile) encodeDeltas(deltas []Delta) ([]byte, error) {
	if len(deltas) == 0 {
		result := make([]byte, 32)
		// length = 0，不写 offset
		return result, nil
	}

	// 数据部分：length (32) + element offsets (32*len) + element data
	fixedSize := 32 + 32*len(deltas)

	dataArea := make([]byte, 0)
	elementOffsets := make([]int, len(deltas))

	for i, delta := range deltas {
		elementOffsets[i] = fixedSize + len(dataArea)
		deltaData, err := c.encodeDelta(delta)
		if err != nil {
			return nil, err
		}
		dataArea = append(dataArea, deltaData...)
	}

	result := make([]byte, 0, fixedSize+len(dataArea))

	// length (32 bytes)，不写 leading offset
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(len(deltas)))

	// element offsets（相对于数组 head，即 length 之后）
	for _, offset := range elementOffsets {
		result = append(result, make([]byte, 32)...)
		binary.BigEndian.PutUint64(result[len(result)-8:], uint64(offset))
	}

	// element data
	result = append(result, dataArea...)

	return result, nil
}

// encodeDelta 编码单个 Delta 结构
// Delta 结构：contractAddr (address) + shardId (uint32) + key (bytes32) + op (uint8) + val (bytes)
// val 是动态类型，所以 Delta 是动态结构
func (c *joyueCoordinatorPrecompile) encodeDelta(delta Delta) ([]byte, error) {
	fixedSize := 160 // contractAddr (32) + shardId (32) + key (32) + op (32) + val offset (32)

	// 编码 val (bytes)
	valPaddedLen := (len(delta.Val) + 31) / 32 * 32
	valSize := 32 + 32 + valPaddedLen

	valOffset := fixedSize

	result := make([]byte, 0, fixedSize+valSize)

	// contractAddr (address, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	copy(result[len(result)-20:], delta.ContractAddr.Bytes())

	// shardId (uint32, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint32(result[len(result)-4:], delta.ShardId)

	// key (bytes32)
	result = append(result, delta.Key.Bytes()...)

	// op (uint8, 32 bytes, 左填充)
	result = append(result, make([]byte, 32)...)
	result[len(result)-1] = delta.Op

	// val offset (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(valOffset))

	// val length (32 bytes)
	result = append(result, make([]byte, 32)...)
	binary.BigEndian.PutUint64(result[len(result)-8:], uint64(len(delta.Val)))

	// val data (padded to 32 bytes)
	valPadded := make([]byte, valPaddedLen)
	copy(valPadded, delta.Val)
	result = append(result, valPadded...)

	return result, nil
}

// triggerDecision 触发决策逻辑（当所有结果收集完成后）
func (c *joyueCoordinatorPrecompile) triggerDecision(evm *EVM, contract *Contract, coordinatorAddr common.Address, batchId common.Hash, attemptNo uint8, participants []Participant, details []IntentDetail, done []bool, active []bool, finals []FinalResult) error {
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint8("attemptNo", attemptNo).
		Int("participantsCount", len(participants)).
		Int("detailsCount", len(details)).
		Int("doneLen", len(done)).
		Int("activeLen", len(active)).
		Msg("[JOYUE Coordinator] triggerDecision: start")

	// 读取 PendingBatch
	pendingBatch, err := c.getPendingBatch(evm, coordinatorAddr, batchId)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("batchId", batchId.Hex()).
			Msg("[JOYUE Coordinator] triggerDecision: getPendingBatch failed")
		return err
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint8("pendingAttemptNo", pendingBatch.AttemptNo).
		Int("pendingParticipantsCount", len(pendingBatch.Participants)).
		Int("pendingDetailsCount", len(pendingBatch.Details)).
		Msg("[JOYUE Coordinator] triggerDecision: loaded PendingBatch")

	// 读取所有参与者结果，构建 Guard 和 Delta 矩阵
	guardMatrix := make([]GuardMatrixResult, len(participants))
	deltaMatrix := make([]DeltaMatrixResult, len(participants))

	for i, participant := range participants {
		result, err := c.getParticipantResult(evm, coordinatorAddr, batchId, participant)
		if err != nil {
			utils.Logger().Warn().
				Err(err).
				Str("batchId", batchId.Hex()).
				Int("participantIndex", i).
				Str("participant", participant.Addr.Hex()).
				Uint32("participantShardId", participant.ShardId).
				Msg("[JOYUE Coordinator] triggerDecision: participant result missing, using failed defaults")
			// 结果未找到，标记为失败
			guardMatrix[i] = GuardMatrixResult{
				Participant: participant,
				TxResults:   c.createFailedGuardResults(details, done, active),
			}
			deltaMatrix[i] = DeltaMatrixResult{
				Participant: participant,
				TxResults:   c.createFailedDeltaResults(details, done, active),
			}
			continue
		}

		utils.Logger().Info().
			Str("batchId", batchId.Hex()).
			Int("participantIndex", i).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Int("guardResultsLen", len(result.GuardResults)).
			Int("deltaResultsLen", len(result.DeltaResults)).
			Bool("success", result.Success).
			Msg("[JOYUE Coordinator] triggerDecision: loaded participant result")

		guardMatrix[i] = GuardMatrixResult{
			Participant: participant,
			TxResults:   result.GuardResults,
		}
		deltaMatrix[i] = DeltaMatrixResult{
			Participant: participant,
			TxResults:   result.DeltaResults,
		}
	}

	// 基于矩阵决策
	dec, out := c.decideFromMatrix(attemptNo, participants, details, done, active, guardMatrix, deltaMatrix)

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint8("attemptNo", attemptNo).
		Uint64("commitCount", dec.commitCount).
		Uint64("retryCount", dec.retryCount).
		Uint64("failCount", dec.failCount).
		Msg("[JOYUE Coordinator] triggerDecision: matrix decision computed")

	switch attemptNo {
	case 1:
		// ── Phase 1 ──────────────────────────────────────────────────────────
		// 决定"谁去死，谁去活"：C1(commit) / R(retry) / F1(直接fail)
		// 立刻为 C1 发出业务完成事件；F1 需要回滚各分片冻结；R 进入第二轮
		var commitTxHashes []common.Hash
		var failTxHashes []common.Hash
		var retryTxHashes []common.Hash

		for i := range details {
			if done[i] || !active[i] {
				continue
			}
			if out.committed[i] {
				commitTxHashes = append(commitTxHashes, details[i].TxHash)
				finals[i] = FinalResult{TxHash: details[i].TxHash, Status: 0, AttemptsUsed: 1}
				done[i] = true
				utils.Logger().Info().
					Str("batchId", batchId.Hex()).
					Str("txHash", details[i].TxHash.Hex()).
					Msg("[JOYUE Coordinator] triggerDecision attempt1: TX COMMITTED - emitting completion event")
			} else if out.callbackFail[i] {
				// F1：直接 fail，收集起来准备回滚各分片冻结
				failTxHashes = append(failTxHashes, details[i].TxHash)
				finals[i] = FinalResult{TxHash: details[i].TxHash, Status: 1, AttemptsUsed: 1}
				done[i] = true
				utils.Logger().Info().
					Str("batchId", batchId.Hex()).
					Str("txHash", details[i].TxHash.Hex()).
					Msg("[JOYUE Coordinator] triggerDecision attempt1: TX FAILED directly")
			} else if out.needRetry[i] {
				retryTxHashes = append(retryTxHashes, details[i].TxHash)
				utils.Logger().Info().
					Str("batchId", batchId.Hex()).
					Str("txHash", details[i].TxHash.Hex()).
					Msg("[JOYUE Coordinator] triggerDecision attempt1: TX will RETRY")
			}
		}

		// 将 Phase 1 决策结果存入 PendingBatch，供 processSecondAttemptMatrix 使用
		pendingBatch.Phase1CommitTxHashes = commitTxHashes
		pendingBatch.Phase1RetryTxHashes = retryTxHashes
		c.savePendingBatch(evm, coordinatorAddr, batchId, *pendingBatch)

		utils.Logger().Info().
			Str("batchId", batchId.Hex()).
			Int("commitCount", len(commitTxHashes)).
			Int("failCount", len(failTxHashes)).
			Int("retryCount", len(retryTxHashes)).
			Msg("[JOYUE Coordinator] triggerDecision attempt1: phase 1 decision saved")

		if len(retryTxHashes) > 0 {
			// 有 retry 交易：进入第二轮
			// 先向 Agent 发送 C1/F1 的最终结果（R 的结果在 attempt2 发送）
			c.emitAgentResultToAgent(evm, contract, coordinatorAddr, pendingBatch.AgentShardId, pendingBatch.AgentContract, commitTxHashes, failTxHashes, CompletionAttempt1DirectSuccess, CompletionAttempt1DirectFail)
			// processSecondAttemptMatrix 内部会同时负责 finalize C1 + F1 + 重新冻结 R
			pendingBatch.Phase1FailTxHashes = failTxHashes
			c.savePendingBatch(evm, coordinatorAddr, batchId, *pendingBatch)
			return c.processSecondAttemptMatrix(evm, contract, coordinatorAddr, pendingBatch.Epoch, pendingBatch.RoundIdBase, participants, details, active, finals, done)
		}

		// 没有 retry 交易：直接对各分片 finalize commit 并回滚 fail
		// 使用 batch1（即当前 batchId）上的冻结状态
		if len(commitTxHashes) > 0 || len(failTxHashes) > 0 {
			// 向原 Agent 发送结果通知（成功或失败）
			c.emitAgentResultToAgent(evm, contract, coordinatorAddr, pendingBatch.AgentShardId, pendingBatch.AgentContract, commitTxHashes, failTxHashes, CompletionAttempt1DirectSuccess, CompletionAttempt1DirectFail)
			utils.Logger().Info().
				Str("batchId", batchId.Hex()).
				Int("commitCount", len(commitTxHashes)).
				Int("failCount", len(failTxHashes)).
				Msg("[JOYUE Coordinator] triggerDecision attempt1: no retry, directly finalizing on all shards")
			return c.processThirdAttemptMatrix(evm, contract, coordinatorAddr, pendingBatch.Epoch, pendingBatch.RoundIdBase, participants, batchId, commitTxHashes, failTxHashes)
		}

	case 2:
		// ── Phase 2 ──────────────────────────────────────────────────────────
		// attempt 2 的回调是"R 交易在各分片重新冻结后"的体检报告
		// 决定哪些 retry tx 可以 commit（C2），哪些最终 fail（F2）
		var commitTxHashes []common.Hash
		var failTxHashes []common.Hash

		for i := range details {
			if done[i] {
				continue
			}
			if out.committed[i] {
				commitTxHashes = append(commitTxHashes, details[i].TxHash)
				finals[i] = FinalResult{TxHash: details[i].TxHash, Status: 0, AttemptsUsed: 2}
				done[i] = true
				utils.Logger().Info().
					Str("batchId", batchId.Hex()).
					Str("txHash", details[i].TxHash.Hex()).
					Msg("[JOYUE Coordinator] triggerDecision attempt2: TX COMMITTED after retry - emitting completion event")
			} else {
				failTxHashes = append(failTxHashes, details[i].TxHash)
				finals[i] = FinalResult{TxHash: details[i].TxHash, Status: 1, AttemptsUsed: 2}
				done[i] = true
				utils.Logger().Info().
					Str("batchId", batchId.Hex()).
					Str("txHash", details[i].TxHash.Hex()).
					Msg("[JOYUE Coordinator] triggerDecision attempt2: TX FAILED after retry")
			}
		}

		// 将 Phase 2 决策结果存入 batch2 的 PendingBatch，供 processThirdAttemptMatrix 使用
		pendingBatch.Phase2CommitTxHashes = commitTxHashes
		pendingBatch.Phase2FailTxHashes = failTxHashes
		c.savePendingBatch(evm, coordinatorAddr, batchId, *pendingBatch)

		utils.Logger().Info().
			Str("batchId", batchId.Hex()).
			Int("commitCount", len(commitTxHashes)).
			Int("failCount", len(failTxHashes)).
			Msg("[JOYUE Coordinator] triggerDecision attempt2: phase 2 decision saved, starting finalize phase")

		// 向原 Agent 发送结果通知（成功或失败）
		c.emitAgentResultToAgent(evm, contract, coordinatorAddr, pendingBatch.AgentShardId, pendingBatch.AgentContract, commitTxHashes, failTxHashes, CompletionAttempt2Success, CompletionAttempt2Fail)

		// 触发 Phase 3：在所有分片上执行 finalize C2 + rollback F2
		return c.processThirdAttemptMatrix(evm, contract, coordinatorAddr, pendingBatch.Epoch, pendingBatch.RoundIdBase, participants, batchId, commitTxHashes, failTxHashes)

	case 3:
		// ── Phase 3 ──────────────────────────────────────────────────────────
		// finalize/rollback 已下发给各分片（fire-and-forget 风格）
		// 收到此回调即表示所有分片的最终化流程已触发，整批结束
		utils.Logger().Info().
			Str("batchId", batchId.Hex()).
			Msg("[JOYUE Coordinator] triggerDecision attempt3: batch fully finalized on all shards")

	default:
		utils.Logger().Warn().
			Str("batchId", batchId.Hex()).
			Uint8("attemptNo", attemptNo).
			Msg("[JOYUE Coordinator] triggerDecision: unknown attemptNo, ignoring")
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint8("attemptNo", attemptNo).
		Msg("[JOYUE Coordinator] triggerDecision: completed")

	return nil
}

// collectParticipantResult 收集参与者结果（内部辅助函数，由 handleOnCrossShardCallback 调用）
// 注意：这不是独立的回调入口，统一使用 onCrossShardCallback 作为回调入口
func (c *joyueCoordinatorPrecompile) collectParticipantResult(evm *EVM, contract *Contract, coordinatorAddr common.Address, batchId common.Hash, participant Participant, guardResults []GuardResult, deltaResults []DeltaResult, success bool) error {
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Bool("success", success).
		Int("guardResultsLen", len(guardResults)).
		Int("deltaResultsLen", len(deltaResults)).
		Msg("[JOYUE Coordinator] collectParticipantResult: saving participant result")

	// 1) 保存该参与者的结果
	c.saveParticipantResult(evm, coordinatorAddr, batchId, participant, guardResults, deltaResults, success)

	// 2) 读取 PendingBatch，便于后续决策
	pendingBatch, err := c.getPendingBatch(evm, coordinatorAddr, batchId)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("batchId", batchId.Hex()).
			Msg("[JOYUE Coordinator] collectParticipantResult: getPendingBatch failed")
		return err
	}

	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Uint8("attemptNo", pendingBatch.AttemptNo).
		Int("participantsCount", len(pendingBatch.Participants)).
		Int("detailsCount", len(pendingBatch.Details)).
		Msg("[JOYUE Coordinator] collectParticipantResult: loaded PendingBatch")

	// 3) 检查是否所有参与者结果都已收集
	allCollected := c.checkAllResultsCollected(evm, coordinatorAddr, batchId, pendingBatch.Participants)
	utils.Logger().Info().
		Str("batchId", batchId.Hex()).
		Bool("allCollected", allCollected).
		Msg("[JOYUE Coordinator] collectParticipantResult: checkAllResultsCollected")

	if allCollected {
		utils.Logger().Info().
			Str("batchId", batchId.Hex()).
			Uint8("attemptNo", pendingBatch.AttemptNo).
			Int("participantsCount", len(pendingBatch.Participants)).
			Msg("[JOYUE Coordinator] collectParticipantResult: all results collected, triggering decision")
		// 4) 所有结果收集完成，触发决策
		finals := make([]FinalResult, len(pendingBatch.Details))
		return c.triggerDecision(evm, contract, coordinatorAddr, batchId, pendingBatch.AttemptNo, pendingBatch.Participants, pendingBatch.Details, pendingBatch.Done, pendingBatch.Active, finals)
	}

	return nil
}

// saveRequestIdMapping 保存 requestId 到 batchId 和 participant 的映射
func (c *joyueCoordinatorPrecompile) saveRequestIdMapping(evm *EVM, addr common.Address, requestId uint64, batchId common.Hash, participant Participant) {
	// 使用 mapping(uint256 => ...) 存储
	// 包含 batchId (32 bytes) + participant addr (20 bytes) + shardId (4 bytes)
	requestIdHash := common.BigToHash(new(big.Int).SetUint64(requestId))

	// Slot 1: batchId (32 bytes)
	slot1 := getMappingSlot(big.NewInt(storageSlotRequestIdMap), requestIdHash)
	evm.StateDB.SetState(addr, slot1, batchId)

	// Slot 2: participant addr (20 bytes, 左填充到 32 bytes)
	slot2 := getMappingSlot(big.NewInt(storageSlotRequestIdMap+1), requestIdHash)
	participantHash := common.BytesToHash(participant.Addr.Bytes())
	evm.StateDB.SetState(addr, slot2, participantHash)

	// Slot 3: participant shardId (uint32, 左填充到 32 bytes)
	slot3 := getMappingSlot(big.NewInt(storageSlotRequestIdMap+2), requestIdHash)
	shardBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(shardBytes[28:32], participant.ShardId)
	evm.StateDB.SetState(addr, slot3, common.BytesToHash(shardBytes))
}

// getRequestIdMapping 从 requestId 恢复 batchId 和 participant
func (c *joyueCoordinatorPrecompile) getRequestIdMapping(evm *EVM, addr common.Address, requestId uint64) (common.Hash, Participant, error) {
	requestIdHash := common.BigToHash(new(big.Int).SetUint64(requestId))

	// Slot 1: batchId
	slot1 := getMappingSlot(big.NewInt(storageSlotRequestIdMap), requestIdHash)
	batchId := evm.StateDB.GetState(addr, slot1)

	// Slot 2: participant addr
	slot2 := getMappingSlot(big.NewInt(storageSlotRequestIdMap+1), requestIdHash)
	participantHash := evm.StateDB.GetState(addr, slot2)
	participantAddr := common.BytesToAddress(participantHash[12:32]) // 地址在最后 20 字节

	// Slot 3: participant shardId
	slot3 := getMappingSlot(big.NewInt(storageSlotRequestIdMap+2), requestIdHash)
	shardHash := evm.StateDB.GetState(addr, slot3)
	participantShardId := binary.BigEndian.Uint32(shardHash[28:32])

	// 检查是否有效（batchId 不为零）
	if batchId == (common.Hash{}) {
		return common.Hash{}, Participant{}, errors.New("requestId not found")
	}

	return batchId, Participant{Addr: participantAddr, ShardId: participantShardId}, nil
}

// handleOnCrossShardCallback 处理统一的跨分片回调函数
// onCrossShardCallback(uint256 requestId, bool ok, bytes returnData)
func (c *joyueCoordinatorPrecompile) handleOnCrossShardCallback(evm *EVM, contract *Contract, coordinatorAddr common.Address, params []byte) ([]byte, error) {
	// ABI 解码参数
	// - requestId: uint256 (32 bytes, 在 params[0:32])
	// - ok: bool (32 bytes, 在 params[32:64]，左填充，值在最后字节)
	// - returnData: bytes (动态类型，偏移量在 params[64:96])

	if len(params) < 96 {
		return nil, errors.New("JOYUE: invalid params length for onCrossShardCallback")
	}

	// 解析 requestId
	requestId := new(big.Int).SetBytes(params[0:32]).Uint64()

	// 解析 ok
	ok := params[63] != 0

	// 解析 returnData 偏移量
	calldataOffset := int(binary.BigEndian.Uint64(params[88:96]))
	if calldataOffset+32 > len(params) {
		return nil, errors.New("JOYUE: invalid returnData offset")
	}
	// 读取长度
	calldataLen := int(binary.BigEndian.Uint64(params[calldataOffset+24 : calldataOffset+32]))
	if calldataOffset+32+calldataLen > len(params) {
		return nil, errors.New("JOYUE: invalid returnData length")
	}
	returnData := params[calldataOffset+32 : calldataOffset+32+calldataLen]

	// 从 requestId 恢复 batchId 和 participant
	batchId, participant, err := c.getRequestIdMapping(evm, coordinatorAddr, requestId)
	if err != nil {
		// requestId 不存在有两种合理情况：
		//   1. fire-and-forget 请求（如 emitFinalizeRequest）故意不注册映射
		//   2. 重复回调 / 已清理的映射
		// 这两种情况都应该静默忽略，不 revert
		utils.Logger().Info().
			Uint64("requestId", requestId).
			Bool("callbackOk", ok).
			Msg("[JOYUE Coordinator] handleOnCrossShardCallback: requestId not found, treating as fire-and-forget, ignoring")
		return []byte{}, nil
	}

	utils.Logger().Info().
		Uint64("requestId", requestId).
		Bool("ok", ok).
		Int("returnDataLen", len(returnData)).
		Str("batchId", batchId.Hex()).
		Str("participant", participant.Addr.Hex()).
		Uint32("participantShardId", participant.ShardId).
		Msg("[JOYUE Coordinator] handleOnCrossShardCallback: parsed callback")

	// 解码 returnData 为 GuardResult[] 和 DeltaResult[]
	// returnData 格式：ColumnResult[] (来自 batchVerifyAndFreeze 的返回值)
	// 需要读取 PendingBatch 来获取 totalDetails 数量
	pendingBatch, err := c.getPendingBatch(evm, coordinatorAddr, batchId)
	if err != nil {
		return nil, err
	}

	// 读取 idxMap
	idxMap, err := c.getParticipantIdxMap(evm, coordinatorAddr, batchId, participant)
	if err != nil {
		return nil, err
	}

	// 从 returnData 解码 ColumnResult[]
	var guardResults []GuardResult
	var deltaResults []DeltaResult

	if ok && len(returnData) > 0 {
		guardResults, deltaResults = c.convertResultsFromReturnData(returnData, idxMap, len(pendingBatch.Details))
		utils.Logger().Info().
			Str("batchId", batchId.Hex()).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Int("idxMapLen", len(idxMap)).
			Int("guardResultsLen", len(guardResults)).
			Int("deltaResultsLen", len(deltaResults)).
			Msg("[JOYUE Coordinator] handleOnCrossShardCallback: decoded results")
	} else {
		// 如果执行失败，创建空的失败结果
		guardResults = make([]GuardResult, len(pendingBatch.Details))
		deltaResults = make([]DeltaResult, len(pendingBatch.Details))
		for i := range guardResults {
			guardResults[i] = GuardResult{Ok: false, GuardFailed: true}
			deltaResults[i] = DeltaResult{Ok: false}
		}
		utils.Logger().Warn().
			Str("batchId", batchId.Hex()).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Msg("[JOYUE Coordinator] handleOnCrossShardCallback: execution failed, using fallback results")
	}

	// 调用 collectParticipantResult
	err = c.collectParticipantResult(evm, contract, coordinatorAddr, batchId, participant, guardResults, deltaResults, ok)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Uint64("requestId", requestId).
			Str("batchId", batchId.Hex()).
			Str("participant", participant.Addr.Hex()).
			Uint32("participantShardId", participant.ShardId).
			Msg("[JOYUE Coordinator] failed to collect participant result")
		return nil, err
	}

	// 返回空字节（函数无返回值）
	return []byte{}, nil
}

// convertResultsFromReturnData 从 returnData 解码 ColumnResult[] 并转换为 GuardResult[] 和 DeltaResult[]
func (c *joyueCoordinatorPrecompile) convertResultsFromReturnData(returnData []byte, idxMap []int, totalDetails int) ([]GuardResult, []DeltaResult) {
	guardResults := make([]GuardResult, totalDetails)
	deltaResults := make([]DeltaResult, totalDetails)

	results, err := c.decodeColumnResults(returnData)
	if err != nil {
		return guardResults, deltaResults
	}

	for i, res := range results {
		if i >= len(idxMap) {
			break
		}
		globalIdx := idxMap[i]
		if globalIdx < 0 || globalIdx >= totalDetails {
			continue
		}
		guardResults[globalIdx] = GuardResult{
			TxHash:           res.TxHash,
			Ok:               res.Ok,
			GuardFailed:      res.GuardFailed,
			FailedGuardIndex: res.FailedGuardIndex,
			LatestKey:        res.LatestKey,
			LatestVal:        res.LatestVal,
			LatestVer:        res.LatestVer,
		}
		deltaResults[globalIdx] = DeltaResult{
			TxHash:           res.TxHash,
			Ok:               res.Ok && !res.GuardFailed,
			FailedDeltaIndex: res.FailedDeltaIndex,
			LatestKey:        res.LatestKey,
			LatestVal:        res.LatestVal,
			LatestVer:        res.LatestVer,
		}
	}

	return guardResults, deltaResults
}

// convertResultsFromColumnResults 将本地执行结果映射为 Guard/Delta 结果（按 idxMap 映射回全局）
func (c *joyueCoordinatorPrecompile) convertResultsFromColumnResults(results []ColumnResult, idxMap []int, totalDetails int) ([]GuardResult, []DeltaResult) {
	guardResults := make([]GuardResult, totalDetails)
	deltaResults := make([]DeltaResult, totalDetails)

	for i, res := range results {
		if i >= len(idxMap) {
			break
		}
		globalIdx := idxMap[i]
		if globalIdx < 0 || globalIdx >= totalDetails {
			continue
		}
		guardResults[globalIdx] = GuardResult{
			TxHash:           res.TxHash,
			Ok:               res.Ok,
			GuardFailed:      res.GuardFailed,
			FailedGuardIndex: res.FailedGuardIndex,
			LatestKey:        res.LatestKey,
			LatestVal:        res.LatestVal,
			LatestVer:        res.LatestVer,
		}
		deltaResults[globalIdx] = DeltaResult{
			TxHash:           res.TxHash,
			Ok:               res.Ok && !res.GuardFailed,
			FailedDeltaIndex: res.FailedDeltaIndex,
			LatestKey:        res.LatestKey,
			LatestVal:        res.LatestVal,
			LatestVer:        res.LatestVer,
		}
	}

	return guardResults, deltaResults
}

// decodeColumnResults 解码 ColumnResult[]（对应 encodeColumnResults）
func (c *joyueCoordinatorPrecompile) decodeColumnResults(data []byte) ([]ColumnResult, error) {
	if len(data) < 64 {
		return nil, errors.New("JOYUE: invalid ColumnResult[] data length")
	}

	arrayOffset := int(binary.BigEndian.Uint64(data[24:32]))
	if arrayOffset+32 > len(data) {
		return nil, errors.New("JOYUE: invalid ColumnResult[] offset")
	}
	arrayLen := int(binary.BigEndian.Uint64(data[arrayOffset+24 : arrayOffset+32]))
	if arrayLen < 0 {
		return nil, errors.New("JOYUE: invalid ColumnResult[] length")
	}

	results := make([]ColumnResult, arrayLen)
	for i := 0; i < arrayLen; i++ {
		offsetPos := arrayOffset + 32 + i*32
		if offsetPos+32 > len(data) {
			return nil, errors.New("JOYUE: invalid ColumnResult element offset")
		}
		elemOffset := int(binary.BigEndian.Uint64(data[offsetPos+24 : offsetPos+32]))
		elemPos := elemOffset
		if elemPos+256 > len(data) {
			return nil, errors.New("JOYUE: invalid ColumnResult element data")
		}

		var r ColumnResult
		r.TxHash = common.BytesToHash(data[elemPos : elemPos+32])
		r.Ok = data[elemPos+63] != 0
		r.GuardFailed = data[elemPos+95] != 0
		r.FailedGuardIndex = binary.BigEndian.Uint32(data[elemPos+124 : elemPos+128])
		r.FailedDeltaIndex = binary.BigEndian.Uint32(data[elemPos+156 : elemPos+160])
		r.LatestKey = common.BytesToHash(data[elemPos+160 : elemPos+192])
		r.LatestVal = new(big.Int).SetBytes(data[elemPos+192 : elemPos+224])
		r.LatestVer = binary.BigEndian.Uint64(data[elemPos+216 : elemPos+224])

		results[i] = r
	}

	return results, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// emitApplyCommitAndRetryRequest
// 向远端分片发送 attempt 2 的组合请求（一条消息顺序执行：finalize C1 + unfreeze R + re-freeze R）
// ABI 编码：applyCommitAndRetry(bytes32 batch1, bytes32[] commitTxHashes, bytes32[] retryTxHashes, bytes32 batch2, bytes columnTxsData)
// ──────────────────────────────────────────────────────────────────────────────
func (c *joyueCoordinatorPrecompile) emitApplyCommitAndRetryRequest(
	evm *EVM,
	contract *Contract,
	coordinatorAddr common.Address,
	batch1 common.Hash,
	commitTxHashes []common.Hash,
	retryTxHashes []common.Hash,
	batch2 common.Hash,
	participant Participant,
	col []ColumnTx,
) error {
	// 编码 ColumnTxs → bytes（用于 ABI bytes 参数）
	var columnTxsData []byte
	if len(col) > 0 {
		encoded, err := c.encodeColumnTxsForCall(col)
		if err != nil {
			return fmt.Errorf("JOYUE: emitApplyCommitAndRetryRequest encodeColumnTxs failed: %w", err)
		}
		columnTxsData = encoded
	}

	// ABI 编码
	// head: 5 个 32 字节 slot（batch1 + commitOffset + retryOffset + batch2 + colOffset）
	headSize := 5 * 32 // = 160
	commitOffset := headSize
	retryOffset := commitOffset + 32 + 32*len(commitTxHashes)
	colOffset := retryOffset + 32 + 32*len(retryTxHashes)

	paddedColLen := (len(columnTxsData) + 31) / 32 * 32
	payload := make([]byte, 0, headSize+32+32*len(commitTxHashes)+32+32*len(retryTxHashes)+32+paddedColLen)

	// head
	payload = append(payload, batch1.Bytes()...)
	payload = append(payload, abiUint256(uint64(commitOffset))...)
	payload = append(payload, abiUint256(uint64(retryOffset))...)
	payload = append(payload, batch2.Bytes()...)
	payload = append(payload, abiUint256(uint64(colOffset))...)

	// commitTxHashes body: length + elements
	payload = append(payload, abiUint256(uint64(len(commitTxHashes)))...)
	for _, h := range commitTxHashes {
		payload = append(payload, h.Bytes()...)
	}
	// retryTxHashes body: length + elements
	payload = append(payload, abiUint256(uint64(len(retryTxHashes)))...)
	for _, h := range retryTxHashes {
		payload = append(payload, h.Bytes()...)
	}
	// columnTxsData body: length + padded bytes
	payload = append(payload, abiUint256(uint64(len(columnTxsData)))...)
	padded := make([]byte, paddedColLen)
	copy(padded, columnTxsData)
	payload = append(payload, padded...)

	// selector + payload
	selectorBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(selectorBytes, applyCommitAndRetrySelector)
	targetCalldata := append(selectorBytes, payload...)

	// requestId 映射到 batch2（回调进入 triggerDecision(attemptNo=2)）
	requestIdData := append(batch2.Bytes(), participantKey(participant).Bytes()...)
	requestIdData = append(requestIdData, evm.BlockNumber.Bytes()...)
	requestId := new(big.Int).SetBytes(crypto.Keccak256Hash(requestIdData).Bytes()[:16]).Uint64()
	c.saveRequestIdMapping(evm, coordinatorAddr, requestId, batch2, participant)

	return c.sendCrossShardToExecutor(evm, contract, coordinatorAddr, participant, targetCalldata, requestId)
}

// ──────────────────────────────────────────────────────────────────────────────
// emitFinalizeRequest
// 向远端分片发送 attempt 3 的 finalize 请求（fire-and-forget）
// ABI 编码：applyFinalize(bytes32 batch2, bytes32[] commitTxHashes, bytes32[] failTxHashes)
// ──────────────────────────────────────────────────────────────────────────────
func (c *joyueCoordinatorPrecompile) emitFinalizeRequest(
	evm *EVM,
	contract *Contract,
	coordinatorAddr common.Address,
	batch2 common.Hash,
	commitTxHashes []common.Hash,
	failTxHashes []common.Hash,
	participant Participant,
) error {
	// ABI 编码
	// head: 3 个 32 字节 slot（batch2 + commitOffset + failOffset）
	headSize := 3 * 32 // = 96
	commitOffset := headSize
	failOffset := commitOffset + 32 + 32*len(commitTxHashes)

	payload := make([]byte, 0, headSize+32+32*len(commitTxHashes)+32+32*len(failTxHashes))

	// head
	payload = append(payload, batch2.Bytes()...)
	payload = append(payload, abiUint256(uint64(commitOffset))...)
	payload = append(payload, abiUint256(uint64(failOffset))...)

	// commitTxHashes body: length + elements
	payload = append(payload, abiUint256(uint64(len(commitTxHashes)))...)
	for _, h := range commitTxHashes {
		payload = append(payload, h.Bytes()...)
	}
	// failTxHashes body: length + elements
	payload = append(payload, abiUint256(uint64(len(failTxHashes)))...)
	for _, h := range failTxHashes {
		payload = append(payload, h.Bytes()...)
	}

	selectorBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(selectorBytes, applyFinalizeSelector)
	targetCalldata := append(selectorBytes, payload...)

	// fire-and-forget：不注册 requestId mapping
	// handleOnCrossShardCallback 找不到对应 batch 会直接返回 error，无副作用
	requestIdData := append(batch2.Bytes(), participantKey(participant).Bytes()...)
	requestIdData = append(requestIdData, []byte("finalize")...)
	requestIdData = append(requestIdData, evm.BlockNumber.Bytes()...)
	requestId := new(big.Int).SetBytes(crypto.Keccak256Hash(requestIdData).Bytes()[:16]).Uint64()

	return c.sendCrossShardToExecutor(evm, contract, coordinatorAddr, participant, targetCalldata, requestId)
}

// ──────────────────────────────────────────────────────────────────────────────
// sendCrossShardToExecutor
// 公共辅助：将 targetCalldata 通过 Executor(0x74) 发到目标分片，带 onCrossShardCallback 回调
// ──────────────────────────────────────────────────────────────────────────────
func (c *joyueCoordinatorPrecompile) sendCrossShardToExecutor(
	evm *EVM,
	contract *Contract,
	coordinatorAddr common.Address,
	participant Participant,
	targetCalldata []byte,
	requestId uint64,
) error {
	sourceShardID := evm.Context.ShardID
	callbackAddr := coordinatorAddr
	callbackSelector := []byte{0x8f, 0x4f, 0xfc, 0xb1} // onCrossShardCallback(uint256,bool,bytes)

	executorSelector := []byte{0x2b, 0x5d, 0x76, 0xa4} // executeAndCallback
	executorCalldata := make([]byte, 0, 4+7*32+((len(targetCalldata)+31)/32*32))
	executorCalldata = append(executorCalldata, executorSelector...)
	sourceShardIDBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(sourceShardIDBytes[28:32], sourceShardID)
	executorCalldata = append(executorCalldata, sourceShardIDBytes...)
	callbackAddrBytes := make([]byte, 32)
	copy(callbackAddrBytes[12:32], callbackAddr.Bytes())
	executorCalldata = append(executorCalldata, callbackAddrBytes...)
	callbackSelectorBytes := make([]byte, 32)
	copy(callbackSelectorBytes[0:4], callbackSelector)
	executorCalldata = append(executorCalldata, callbackSelectorBytes...)
	requestIdBytes := make([]byte, 32)
	new(big.Int).SetUint64(requestId).FillBytes(requestIdBytes)
	executorCalldata = append(executorCalldata, requestIdBytes...)
	targetAddrBytes := make([]byte, 32)
	copy(targetAddrBytes[12:32], participant.Addr.Bytes())
	executorCalldata = append(executorCalldata, targetAddrBytes...)
	targetValueBytes := make([]byte, 32)
	executorCalldata = append(executorCalldata, targetValueBytes...)
	calldataOffsetBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(calldataOffsetBytes[24:32], 224)
	executorCalldata = append(executorCalldata, calldataOffsetBytes...)
	calldataLenBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(calldataLenBytes[24:32], uint64(len(targetCalldata)))
	executorCalldata = append(executorCalldata, calldataLenBytes...)
	paddedLen := (len(targetCalldata) + 31) / 32 * 32
	paddedData := make([]byte, paddedLen)
	copy(paddedData, targetCalldata)
	executorCalldata = append(executorCalldata, paddedData...)

	executorAddr := common.BytesToAddress([]byte{116}) // 0x74
	var callbackSelectorArray [4]byte
	copy(callbackSelectorArray[:], callbackSelector)

	utils.Logger().Info().
		Str("participant", participant.Addr.Hex()).
		Uint32("targetShardID", participant.ShardId).
		Uint64("requestId", requestId).
		Int("targetCalldataLen", len(targetCalldata)).
		Msg("[JOYUE Coordinator] sendCrossShardToExecutor: emitting")

	_, err := emitCrossShardRequestEvent(evm, contract, participant.ShardId, executorAddr, big.NewInt(0), executorCalldata, callbackAddr, callbackSelectorArray, requestId)
	if err != nil {
		utils.Logger().Error().Err(err).
			Str("participant", participant.Addr.Hex()).
			Uint32("targetShardID", participant.ShardId).
			Msg("[JOYUE Coordinator] sendCrossShardToExecutor: failed")
		return err
	}
	utils.Logger().Info().
		Str("participant", participant.Addr.Hex()).
		Uint32("targetShardID", participant.ShardId).
		Uint64("requestId", requestId).
		Msg("[JOYUE Coordinator] sendCrossShardToExecutor: success")
	return nil
}
