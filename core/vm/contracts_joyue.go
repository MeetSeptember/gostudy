package vm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/crypto/hash"
	joyue "github.com/harmony-one/harmony/internal/joyue"
	"github.com/harmony-one/harmony/internal/params"
	"github.com/harmony-one/harmony/internal/utils"
)

/*
JOYUE P2P 缓存 Precompile

读缓存地址：0x0000000000000000000000000000000000000100 (0x64 = 100)
写缓存地址：0x0000000000000000000000000000000000000101 (0x65 = 101)

功能：
- 读缓存：允许合约通过 precompile 访问节点层面的 P2P 缓存
- 写缓存：允许代理合约修改本地 P2P 缓存（不广播，只有主合约的修改才广播）

调用格式：
读缓存：
  input = contractAddr (20 bytes) + key (32 bytes)
  output = abi.encode(bytes value, uint64 version, bool ok)

写缓存：
  input = contractAddr (20 bytes) + key (32 bytes) + value (32 bytes) + version (8 bytes, padded to 32)
  output = 空（成功）或错误

注意：当前实现不考虑安全性，仅实现基本功能
*/

// PrecompiledContractsJoyue 包含 JOYUE 相关的只读 precompile
var PrecompiledContractsJoyue = map[common.Address]PrecompiledContract{
	common.BytesToAddress([]byte{100}): &joyueCachePrecompile{},     // 0x64 = 100 (读单个缓存)
	common.BytesToAddress([]byte{102}): &joyueCacheListPrecompile{}, // 0x66 = 102 (查询合约的所有缓存)
	common.BytesToAddress([]byte{103}): &joyueRpcOraclePrecompile{}, // 0x67 = 103 (RPC Oracle 查询权威状态)
}

// WriteCapablePrecompiledContractsJoyue 包含 JOYUE 相关的可写 precompile
var WriteCapablePrecompiledContractsJoyue = map[common.Address]WriteCapablePrecompiledContract{
	common.BytesToAddress([]byte{101}): &joyueCacheWritePrecompile{},      // 0x65 = 101 (写缓存)
	common.BytesToAddress([]byte{108}): &joyueCurrentShardPrecompile{},    // 0x6C = 108 (获取当前分片 ID，虽然是只读但需要 EVM 上下文)
	common.BytesToAddress([]byte{109}): &joyueRpcOracleSendTxPrecompile{}, // 0x6D = 109 (RPC Oracle 发送交易)
	common.BytesToAddress([]byte{115}): &joyueCoordinatorPrecompile{},     // 0x73 = 115 (Coordinator)
	common.BytesToAddress([]byte{116}): &joyueExecutorPrecompile{},        // 0x74 = 116 (Executor)
	// 注释掉批量发送 precompile，改为串行调用以避免并发数据库访问问题
	// common.BytesToAddress([]byte{110}): &joyueRpcOracleSendTxBatchPrecompile{}, // 0x6E = 110 (RPC Oracle 批量发送交易)
}

// joyueCachePrecompile 实现 P2P 缓存访问的 precompile
type joyueCachePrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueCachePrecompile) RequiredGas(input []byte) uint64 {
	// 基础 gas + 输入数据 gas
	// input = 20 bytes (contractAddr) + 4 bytes (shardId) + 32 bytes (key) = 56 bytes
	baseGas := uint64(100)
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas
}

// Run 执行 precompile，返回可用量（authValue - totalFrozen）
// 输入格式：紧凑 contractAddr(20)+shardId(4)+key(32)，或 ABI abi.encode(address,uint32,bytes32)(96 bytes)
func (c *joyueCachePrecompile) Run(input []byte) ([]byte, error) {
	if len(input) < 56 {
		return nil, errors.New("JOYUE: invalid input length")
	}

	var contractAddr common.Address
	var shardId uint32
	var key common.Hash
	if len(input) >= 96 {
		contractAddr = common.BytesToAddress(input[12:32])
		shardId = binary.BigEndian.Uint32(input[60:64])
		key = common.BytesToHash(input[64:96])
	} else {
		contractAddr = common.BytesToAddress(input[0:20])
		shardId = binary.BigEndian.Uint32(input[20:24])
		key = common.BytesToHash(input[24:56])
	}

	cacheBroadcaster := joyue.GetGlobalCacheBroadcaster()
	if cacheBroadcaster == nil {
		return encodeCacheResult(nil, 0, false), nil
	}

	// 返回可用量（权威缓存值 - 本地冻结量），而非原始权威值
	value, version, ok := cacheBroadcaster.GetAvailable(contractAddr, shardId, key)
	return encodeCacheResult(value, version, ok), nil
}

// encodeCacheResult 编码缓存查询结果
// 返回格式：abi.encode(bytes value, uint64 version, bool ok)
func encodeCacheResult(value []byte, version uint64, ok bool) []byte {
	// ABI 编码：
	// - bytes: offset (32 bytes) + length (32 bytes) + data (padded to 32 bytes)
	// - uint64: 32 bytes (padded)
	// - bool: 32 bytes (padded)

	result := make([]byte, 0, 96+len(value)+31)

	// offset for bytes (32 bytes)
	offset := big.NewInt(96).Bytes() // 3 * 32 = 96 (offset, length, version, ok)
	result = append(result, common.LeftPadBytes(offset, 32)...)

	// length of bytes (32 bytes)
	length := big.NewInt(int64(len(value))).Bytes()
	result = append(result, common.LeftPadBytes(length, 32)...)

	// version (32 bytes, padded)
	versionBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(versionBytes, version)
	result = append(result, common.LeftPadBytes(versionBytes, 32)...)

	// ok (bool, 32 bytes, padded)
	okBytes := big.NewInt(0).Bytes()
	if ok {
		okBytes = big.NewInt(1).Bytes()
	}
	result = append(result, common.LeftPadBytes(okBytes, 32)...)

	// bytes data (padded to 32 bytes)
	if len(value) > 0 {
		paddedValue := common.RightPadBytes(value, (len(value)+31)/32*32)
		result = append(result, paddedValue...)
	}

	return result
}

// joyueCacheListPrecompile 实现查询某个合约的所有缓存的 precompile
type joyueCacheListPrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueCacheListPrecompile) RequiredGas(input []byte) uint64 {
	// 基础 gas + 输入数据 gas
	// input = 20 bytes (contractAddr)
	baseGas := uint64(500) // 查询所有缓存需要更多 gas
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas
}

// Run 执行 precompile，返回某个合约的所有缓存键
func (c *joyueCacheListPrecompile) Run(input []byte) ([]byte, error) {
	// 输入格式：20 bytes (contractAddr) + 4 bytes (shardId)
	// 或 ABI 编码格式：abi.encode(address,uint32)
	if len(input) < 24 {
		return nil, errors.New("JOYUE: invalid input length for cache list")
	}

	var contractAddr common.Address
	var shardId uint32
	if len(input) >= 64 {
		contractAddr = common.BytesToAddress(input[12:32])
		shardId = binary.BigEndian.Uint32(input[60:64])
	} else {
		contractAddr = common.BytesToAddress(input[0:20])
		shardId = binary.BigEndian.Uint32(input[20:24])
	}

	// 从全局缓存广播器获取所有缓存
	cacheBroadcaster := joyue.GetGlobalCacheBroadcaster()
	if cacheBroadcaster == nil {
		// 缓存未启用，返回空列表
		return encodeCacheListResult(nil), nil
	}

	// 获取该合约的所有缓存条目
	entries := cacheBroadcaster.GetAllCacheForContract(contractAddr, shardId)

	// 编码返回结果：abi.encode(bytes32[] keys, bytes[] values, uint64[] versions)
	return encodeCacheListResult(entries), nil
}

// encodeCacheListResult 编码缓存列表查询结果
// 返回格式：abi.encode(bytes32[] keys, bytes[] values, uint64[] versions)
func encodeCacheListResult(entries []*joyue.CacheEntry) []byte {
	if len(entries) == 0 {
		// 返回空列表的编码
		result := make([]byte, 96)
		// keys offset = 96
		copy(result[0:32], common.LeftPadBytes(big.NewInt(96).Bytes(), 32))
		// values offset = 128 (96 + 32)
		copy(result[32:64], common.LeftPadBytes(big.NewInt(128).Bytes(), 32))
		// versions offset = 160 (128 + 32)
		copy(result[64:96], common.LeftPadBytes(big.NewInt(160).Bytes(), 32))
		// keys length = 0
		// values length = 0
		// versions length = 0
		return result
	}

	// 计算总长度
	keysLen := len(entries)
	totalValuesLength := uint64(0)
	for _, entry := range entries {
		// 每个 value 需要：offset (32) + length (32) + data (padded)
		totalValuesLength += 64 + uint64((len(entry.Value)+31)/32*32)
	}

	// 计算 offsets
	keysOffset := uint64(96)                                // 3 * 32 = 96 (keys offset, values offset, versions offset)
	valuesOffset := keysOffset + 32 + uint64(keysLen)*32    // keys length + keys data
	versionsOffset := valuesOffset + 32 + totalValuesLength // values length + values data

	// 构建结果
	// 计算总容量：versionsOffset + 32 (versions length) + keysLen * 32 (versions data, 每个 version 32 bytes)
	resultCapacity := int(versionsOffset) + 32 + int(keysLen)*32
	result := make([]byte, 0, resultCapacity)

	// keys offset (32 bytes)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(keysOffset)).Bytes(), 32)...)
	// values offset (32 bytes)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(valuesOffset)).Bytes(), 32)...)
	// versions offset (32 bytes)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(versionsOffset)).Bytes(), 32)...)

	// keys array: length (32 bytes) + keys (32 bytes each)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(keysLen)).Bytes(), 32)...)
	for _, entry := range entries {
		result = append(result, entry.Key.Bytes()...)
	}

	// values array: length (32 bytes) + values (offset + length + data for each)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(keysLen)).Bytes(), 32)...)
	valueDataOffset := valuesOffset + 32 + uint64(keysLen)*64 // length + offsets for all values
	currentValueOffset := valueDataOffset
	for _, entry := range entries {
		// value offset (32 bytes)
		result = append(result, common.LeftPadBytes(big.NewInt(int64(currentValueOffset)).Bytes(), 32)...)
		// value length (32 bytes)
		result = append(result, common.LeftPadBytes(big.NewInt(int64(len(entry.Value))).Bytes(), 32)...)
		currentValueOffset += 64 + uint64((len(entry.Value)+31)/32*32)
	}
	// value data
	for _, entry := range entries {
		paddedValue := common.RightPadBytes(entry.Value, (len(entry.Value)+31)/32*32)
		result = append(result, paddedValue...)
	}

	// versions array: length (32 bytes) + versions (32 bytes each, padded)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(keysLen)).Bytes(), 32)...)
	for _, entry := range entries {
		versionBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(versionBytes, entry.Version)
		result = append(result, common.LeftPadBytes(versionBytes, 32)...)
	}

	return result
}

// joyueCacheWritePrecompile 实现 P2P 缓存本地写入的 precompile（不广播）
type joyueCacheWritePrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueCacheWritePrecompile) RequiredGas(evm *EVM, contract *Contract, input []byte) (uint64, error) {
	// 基础 gas + 输入数据 gas
	// input = 20 bytes (contractAddr) + 4 bytes (shardId) + 32 bytes (key) + 32 bytes (value) + 32 bytes (version) = 120 bytes
	baseGas := uint64(200) // 写入操作需要更多 gas
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas, nil
}

// RunWriteCapable 执行 precompile，支持两种语义（均为 ABI 编码，共 160 bytes）：
//
//  1. Freeze（D_SUB 冻结，由 JoyueLib._applyCacheWrite 在 txId != 0 时调用）：
//     abi.encode(address contractAddr, uint32 shardId, bytes32 key, bytes32 txId, uint256 amount)
//     第 4 个参数为 bytes32（txId），第 5 个参数为 uint256（amount）。
//     返回 abi(bytes32) 其中值为 1 表示成功，0 表示可用量不足。
//
//  2. Write（D_ADD/D_SET 快进写缓存，由 JoyueLib._applyCacheWrite 在非 D_SUB 时调用）：
//     abi.encode(address contractAddr, uint32 shardId, bytes32 key, uint256 value, uint64 version)
//     第 4 个参数为 uint256（value），第 5 个参数为 uint64（version）。
//     返回空表示成功。
//
// 区分方式：检查第 4 个参数（input[96:128]）的高 16 字节（input[96:112]）：
//   - 若有非零 → 第 4 参数是 txId（bytes32 keccak256），视为 Freeze 语义。
//   - 若全零 → 第 4 参数是 value（uint256 小数值左填充），视为 Write 语义。
//
// 注：原逻辑检查第 5 参数 input[128:156] 会误判——Freeze 的 amount 较小时高字节全零，
//
//	被当作 Write，导致 txId 被当作 value 解析，出现巨大数字。
func (c *joyueCacheWritePrecompile) RunWriteCapable(evm *EVM, contract *Contract, input []byte) ([]byte, error) {
	if len(input) < 160 {
		return nil, errors.New("JOYUE: invalid input length for cache write/freeze (need 160 bytes ABI)")
	}

	contractAddr := common.BytesToAddress(input[12:32])
	shardId := binary.BigEndian.Uint32(input[60:64])
	key := common.BytesToHash(input[64:96])

	cacheBroadcaster := joyue.GetGlobalCacheBroadcaster()
	if cacheBroadcaster == nil {
		return nil, errors.New("JOYUE: cache broadcaster not initialized")
	}

	// 区分 Freeze 和 Write：检查第 4 个参数（input[96:128]）的高 16 字节
	// - Freeze：第 4 参数是 txId（bytes32 keccak256），高字节几乎必然非零
	// - Write：第 4 参数是 value（uint256），小数值时高字节为零
	// 注：原逻辑检查 input[128:156]（第 5 参数）会误判：Freeze 的 amount 较小时高字节全零，被当作 Write，
	//     导致 txId 被当作 value 解析，出现巨大数字（如 18515326...）
	isFreeze := false
	for _, b := range input[96:112] {
		if b != 0 {
			isFreeze = true
			break
		}
	}

	if isFreeze {
		// Freeze 语义：abi.encode(address, uint32, bytes32, bytes32 txId, uint256 amount)
		txId := common.BytesToHash(input[96:128])
		amount := new(big.Int).SetBytes(input[128:160])

		utils.Logger().Info().
			Str("contract", contractAddr.Hex()).
			Uint32("shardId", shardId).
			Str("key", key.Hex()).
			Str("txId", txId.Hex()).
			Str("amount", amount.String()).
			Msg("[JOYUE] precompile: Freeze request")

		ok := cacheBroadcaster.Freeze(contractAddr, shardId, key, txId, amount)

		// 返回 32 bytes：[31]=0x01 成功，[31]=0x00 失败
		result := make([]byte, 32)
		if ok {
			result[31] = 0x01
		}
		return result, nil
	}

	// Write 语义：abi.encode(address, uint32, bytes32, uint256 value, uint64 version)
	valueCopy := make([]byte, 32)
	copy(valueCopy, input[96:128])
	version := binary.BigEndian.Uint64(input[152:160])

	valueUint := new(big.Int).SetBytes(valueCopy)
	utils.Logger().Info().
		Str("contract", contractAddr.Hex()).
		Uint32("shardId", shardId).
		Str("key", key.Hex()).
		Uint64("version", version).
		Str("value", valueUint.String()).
		Msg("[JOYUE] precompile: SetCacheLocal (fast-forward write)")

	cacheBroadcaster.SetCacheLocal(contractAddr, shardId, key, valueCopy, version)
	return nil, nil
}

// joyueRpcOraclePrecompile 实现 RPC Oracle 查询的 precompile
// 用于通过 RPC 查询其他分片的主合约权威状态（非缓存）
type joyueRpcOraclePrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueRpcOraclePrecompile) RequiredGas(input []byte) uint64 {
	// 基础 gas + 输入数据 gas
	// input = 4 bytes (shardID) + 20 bytes (contractAddr) + 32 bytes (key) = 56 bytes
	// 或者 input = 4 bytes (shardID) + 20 bytes (contractAddr) + 4 bytes (calldataLen) + calldata
	baseGas := uint64(1000) // RPC 调用需要更多 gas（网络 I/O）
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas
}

// Run 执行 precompile，通过 RPC 查询其他分片的主合约状态
func (c *joyueRpcOraclePrecompile) Run(input []byte) ([]byte, error) {
	// 输入格式1（查询 storage）：4 bytes (shardID) + 20 bytes (contractAddr) + 32 bytes (key)
	// 输入格式2（调用 getter）：4 bytes (shardID) + 20 bytes (contractAddr) + 4 bytes (calldataLen) + calldata
	if len(input) < 56 {
		return nil, errors.New("JOYUE: invalid input length for RPC oracle")
	}

	// 解析 shardID (4 bytes)
	shardID := binary.BigEndian.Uint32(input[0:4])

	// 解析 contractAddr (20 bytes)
	contractAddr := common.BytesToAddress(input[4:24])

	// 判断是查询 storage 还是调用 getter
	// 如果剩余数据是 32 字节，则是查询 storage
	// 如果剩余数据 >= 4 字节且前 4 字节是长度，则是调用 getter
	if len(input) == 56 {
		// 查询 storage
		key := common.BytesToHash(input[24:56])

		// 从全局 RPC Oracle 查询状态
		rpcOracle := joyue.GetGlobalRpcOracle()
		if rpcOracle == nil {
			return nil, errors.New("JOYUE: RPC oracle not initialized")
		}

		value, version, ok := rpcOracle.QueryContractState(shardID, contractAddr, key)
		if !ok {
			return encodeRpcOracleResult(nil, 0, false), nil
		}

		return encodeRpcOracleResult(value, version, ok), nil
	} else if len(input) >= 28 {
		// 调用 getter 函数
		calldataLen := binary.BigEndian.Uint32(input[24:28])
		if len(input) < int(28+calldataLen) {
			return nil, errors.New("JOYUE: invalid calldata length")
		}
		calldata := input[28 : 28+calldataLen]

		// 从全局 RPC Oracle 调用 getter
		rpcOracle := joyue.GetGlobalRpcOracle()
		if rpcOracle == nil {
			return nil, errors.New("JOYUE: RPC oracle not initialized")
		}

		result, err := rpcOracle.QueryContractGetter(shardID, contractAddr, calldata)
		if err != nil {
			return nil, fmt.Errorf("JOYUE: RPC call failed: %w", err)
		}

		// 返回 getter 的结果（直接返回，不包装）
		return result, nil
	}

	return nil, errors.New("JOYUE: invalid input format")
}

// encodeRpcOracleResult 编码 RPC Oracle 查询结果
// 返回格式：abi.encode(bytes value, uint64 version, bool ok)
func encodeRpcOracleResult(value []byte, version uint64, ok bool) []byte {
	// 使用与 encodeCacheResult 相同的格式
	return encodeCacheResult(value, version, ok)
}

// joyueCurrentShardPrecompile 获取当前分片 ID 的 precompile
// 注意：虽然是只读操作，但使用 WriteCapable 接口以访问 EVM 上下文
type joyueCurrentShardPrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueCurrentShardPrecompile) RequiredGas(evm *EVM, contract *Contract, input []byte) (uint64, error) {
	return uint64(50), nil // 简单的查询操作
}

// RunWriteCapable 执行 precompile，返回当前分片 ID
func (c *joyueCurrentShardPrecompile) RunWriteCapable(evm *EVM, contract *Contract, input []byte) ([]byte, error) {
	shardID := evm.Context.ShardID
	utils.Logger().Info().
		Uint32("ShardID", shardID).
		Str("caller", contract.CallerAddress.Hex()).
		Msg("[JOYUE] precompile 0x6C getCurrentShardID: evm.Context.ShardID")
	result := make([]byte, 32)
	binary.BigEndian.PutUint32(result[28:32], shardID)
	return result, nil
}

// joyueRpcOracleSendTxPrecompile 通过 RPC 发送交易的 precompile
type joyueRpcOracleSendTxPrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueRpcOracleSendTxPrecompile) RequiredGas(evm *EVM, contract *Contract, input []byte) (uint64, error) {
	// 基础 gas + 输入数据 gas
	// input = 4 bytes (shardID) + 20 bytes (to) + 32 bytes (value) + 4 bytes (calldataLen) + calldata
	baseGas := uint64(5000) // 发送交易需要更多 gas（网络 I/O + 签名）
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas, nil
}

// RunWriteCapable 执行 precompile，通过 RPC 发送交易到其他分片
func (c *joyueRpcOracleSendTxPrecompile) RunWriteCapable(evm *EVM, contract *Contract, input []byte) ([]byte, error) {
	// 调试：确认 0x6D 是否被调用（排查 buyFruit 无事件问题）
	utils.Logger().Info().
		Int("inputLen", len(input)).
		Str("caller", contract.CallerAddress.Hex()).
		Msg("[JOYUE] precompile 0x6D RunWriteCapable ENTERED")

	// 输入格式：4 bytes (shardID) + 20 bytes (to) + 32 bytes (value) + 4 bytes (calldataLen) + calldata
	if len(input) < 60 {
		utils.Logger().Warn().Int("inputLen", len(input)).Msg("[JOYUE] 0x6D: invalid input length, returning error")
		return nil, errors.New("JOYUE: invalid input length for RPC send transaction")
	}

	// 解析参数
	shardID := binary.BigEndian.Uint32(input[0:4])
	to := common.BytesToAddress(input[4:24])
	value := new(big.Int).SetBytes(input[24:56])
	calldataLen := binary.BigEndian.Uint32(input[56:60])
	if len(input) < int(60+calldataLen) {
		return nil, errors.New("JOYUE: invalid calldata length")
	}
	calldata := input[60 : 60+calldataLen]

	// 方案二：Event + Relayer
	// 发出事件而不是直接发送 RPC，由 Relayer 监听事件并发送交易

	// 生成唯一的 requestId（基于交易哈希和调用者地址）
	// 使用 txHash + caller + 当前 log index 生成唯一 ID
	txHash := evm.StateDB.TxHash()
	caller := contract.CallerAddress
	logIndex := evm.StateDB.TxIndex()

	// 生成 requestId：keccak256(txHash + caller + logIndex + calldata[:32])
	requestIdData := append(txHash.Bytes(), caller.Bytes()...)
	requestIdData = append(requestIdData, make([]byte, 4)...)
	binary.BigEndian.PutUint32(requestIdData[len(requestIdData)-4:], uint32(logIndex))
	if len(calldata) > 0 {
		calldataLen := len(calldata)
		if calldataLen > 32 {
			calldataLen = 32
		}
		requestIdData = append(requestIdData, calldata[:calldataLen]...)
	}
	requestIdHash := hash.Keccak256Hash(requestIdData)
	requestId := new(big.Int).SetBytes(requestIdHash[:]).Uint64()

	// 从 calldata 中提取 callbackAddr 和 callbackSelector（如果存在）
	// 注意：当前实现假设 calldata 是 executor.executeAndCallback 的调用
	// 格式：function signature (4 bytes) + params...
	// executeAndCallback(uint32 sourceShardID, address callbackAddr, bytes4 callbackSelector, uint256 requestId, address target, uint256 targetValue, bytes targetCalldata)
	// ABI 编码规则：
	// - address: 32 bytes，左填充（LeftPad），地址在最后 20 字节 [12:32]
	// - bytes4: 32 bytes，右填充（RightPad），selector 在前 4 字节 [0:4]
	// - uint32: 32 bytes，左填充
	// - uint256: 32 bytes
	// - bytes: 动态类型，先 offset (32 bytes)，后 length (32 bytes)，再 data
	var callbackAddr common.Address
	var callbackSelector [4]byte

	// 尝试从 calldata 解析
	if len(calldata) >= 4 {
		// 跳过 function selector (4 bytes)
		calldataParams := calldata[4:]
		// 参数布局：
		// [0:32]   sourceShardID (uint32, 32 bytes)
		// [32:64]  callbackAddr (address, 32 bytes, 地址在 [44:64])
		// [64:96]  callbackSelector (bytes4, 32 bytes, selector 在 [64:68])
		// [96:128] requestId (uint256, 32 bytes)
		// [128:160] target (address, 32 bytes)
		// [160:192] targetValue (uint256, 32 bytes)
		// [192:224] targetCalldata offset (uint256, 32 bytes)
		// [224:256] targetCalldata length (uint256, 32 bytes)
		// [256:...] targetCalldata data
		if len(calldataParams) >= 96 {
			// callbackAddr: address 类型，32 字节，左填充，地址在最后 20 字节
			callbackAddr = common.BytesToAddress(calldataParams[44:64]) // [32+12:32+32] = [44:64]
			// callbackSelector: bytes4 类型，32 字节，右填充，selector 在前 4 字节
			copy(callbackSelector[:], calldataParams[64:68]) // [64:68]
		}
	}

	// 如果无法解析，使用调用者地址作为默认回调地址
	if callbackAddr == (common.Address{}) {
		callbackAddr = caller
		utils.Logger().Warn().
			Str("caller", caller.Hex()).
			Msg("[JOYUE] failed to parse callbackAddr from calldata, using caller as default")
	}

	// 复用统一的事件发出逻辑
	requestIdHashForTopic, err := emitCrossShardRequestEvent(evm, contract, shardID, to, value, calldata, callbackAddr, callbackSelector, requestId)
	if err != nil {
		return nil, err
	}

	utils.Logger().Info().
		Uint64("requestId", requestId).
		Uint32("shardID", shardID).
		Str("to", to.Hex()).
		Str("callbackAddr", callbackAddr.Hex()).
		Str("caller", caller.Hex()).
		Msg("[JOYUE] emitted CrossShardRequest event (Event + Relayer)")

	// 返回 requestId 的哈希（32 bytes），合约可以用这个来追踪请求
	return requestIdHashForTopic.Bytes(), nil
}

// joyueRpcOracleSendTxBatchPrecompile 批量发送跨分片交易 precompile
type joyueRpcOracleSendTxBatchPrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueRpcOracleSendTxBatchPrecompile) RequiredGas(evm *EVM, contract *Contract, input []byte) (uint64, error) {
	// 基础 gas + 每个请求的 gas
	// input = 4 bytes (count) + [4 bytes (shardID) + 20 bytes (to) + 32 bytes (value) + 4 bytes (calldataLen) + calldata] * count
	baseGas := uint64(10000) // 批量发送需要更多基础 gas
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas, nil
}

// RunWriteCapable 执行批量发送 precompile
// 输入格式：4 bytes (count) + [4 bytes (shardID) + 20 bytes (to) + 32 bytes (value) + 4 bytes (calldataLen) + calldata] * count
// 返回格式：abi.encode(bytes32[] txHashes, uint32 successCount)
func (c *joyueRpcOracleSendTxBatchPrecompile) RunWriteCapable(evm *EVM, contract *Contract, input []byte) ([]byte, error) {
	if len(input) < 4 {
		return nil, errors.New("JOYUE: invalid input length for batch send")
	}

	count := binary.BigEndian.Uint32(input[0:4])
	if count == 0 {
		// 返回空数组
		return encodeBatchResult(nil, 0), nil
	}

	rpcOracle := joyue.GetGlobalRpcOracle()
	if rpcOracle == nil {
		return nil, errors.New("JOYUE: RPC oracle not initialized")
	}

	// 解析所有请求
	requests := make([]joyue.SendTransactionRequest, 0, count)

	offset := 4
	for i := uint32(0); i < count; i++ {
		if len(input) < offset+60 {
			return nil, fmt.Errorf("JOYUE: invalid request %d: insufficient input", i)
		}

		shardID := binary.BigEndian.Uint32(input[offset : offset+4])
		to := common.BytesToAddress(input[offset+4 : offset+24])
		value := new(big.Int).SetBytes(input[offset+24 : offset+56])
		calldataLen := binary.BigEndian.Uint32(input[offset+56 : offset+60])

		if len(input) < offset+60+int(calldataLen) {
			return nil, fmt.Errorf("JOYUE: invalid request %d: calldata length mismatch", i)
		}

		calldata := input[offset+60 : offset+60+int(calldataLen)]

		requests = append(requests, joyue.SendTransactionRequest{
			ShardID:  shardID,
			To:       to,
			Calldata: calldata,
			Value:    value,
		})

		offset += 60 + int(calldataLen)
	}

	// 批量并发发送
	txHashes, successCount, err := rpcOracle.SendTransactionBatch(requests)
	if err != nil {
		return nil, fmt.Errorf("JOYUE: failed to send batch transactions: %w", err)
	}

	// 编码返回结果：abi.encode(bytes32[] txHashes, uint32 successCount)
	return encodeBatchResult(txHashes, successCount), nil
}

// encodeBatchResult 编码批量发送结果
// 返回格式：abi.encode(bytes32[] txHashes, uint32 successCount)
func encodeBatchResult(txHashes []common.Hash, successCount uint32) []byte {
	// ABI 编码：
	// - bytes32[]: offset (32 bytes) + length (32 bytes) + data (32 bytes * count)
	// - uint32: 32 bytes (padded)

	if len(txHashes) == 0 {
		// 返回空数组 + successCount
		result := make([]byte, 96)
		// txHashes offset = 64
		copy(result[0:32], common.LeftPadBytes(big.NewInt(64).Bytes(), 32))
		// txHashes length = 0
		// successCount = 0
		binary.BigEndian.PutUint32(result[64:68], successCount)
		return result
	}

	// 计算 offsets
	txHashesOffset := uint64(64) // 2 * 32 = 64 (txHashes offset + length)

	// 构建结果
	result := make([]byte, 0, int(txHashesOffset)+32+len(txHashes)*32)

	// txHashes offset (32 bytes)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(txHashesOffset)).Bytes(), 32)...)

	// txHashes length (32 bytes)
	result = append(result, common.LeftPadBytes(big.NewInt(int64(len(txHashes))).Bytes(), 32)...)

	// txHashes data (32 bytes each)
	for _, txHash := range txHashes {
		result = append(result, txHash.Bytes()...)
	}

	// successCount (32 bytes, padded)
	successCountBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(successCountBytes[28:32], successCount)
	result = append(result, successCountBytes...)

	return result
}

// emitCrossShardRequestEvent 发出 CrossShardRequest 事件的统一函数
// 事件签名：CrossShardRequest(uint256 indexed requestId, uint32 targetShardID, address target, bytes calldata, uint256 value, address callbackAddr, bytes4 callbackSelector)
// 返回 requestId 的哈希（用于作为事件 topic），以及可能的错误
func emitCrossShardRequestEvent(evm *EVM, contract *Contract, targetShardID uint32, targetAddr common.Address, value *big.Int, calldata []byte, callbackAddr common.Address, callbackSelector [4]byte, requestId uint64) (common.Hash, error) {
	// 事件签名：CrossShardRequest(uint256 indexed requestId, uint32 targetShardID, address target, bytes calldata, uint256 value, address callbackAddr, bytes4 callbackSelector)
	// Topics[0] = keccak256("CrossShardRequest(uint256,uint32,address,bytes,uint256,address,bytes4)")
	// Topics[1] = requestId (indexed)
	// Data = abi.encode(targetShardID, target, calldata, value, callbackAddr, callbackSelector)

	eventSignature := hash.Keccak256Hash([]byte("CrossShardRequest(uint256,uint32,address,bytes,uint256,address,bytes4)"))
	requestIdHashForTopic := common.BigToHash(new(big.Int).SetUint64(requestId))

	// 编码事件数据：targetShardID (32 bytes) + target (32 bytes) + calldata (offset + length + data) + value (32 bytes) + callbackAddr (32 bytes) + callbackSelector (32 bytes)
	// 简化：使用紧凑格式，避免复杂的 ABI 编码
	// 格式：shardID (4 bytes) + target (20 bytes) + calldataLen (4 bytes) + calldata + value (32 bytes) + callbackAddr (20 bytes) + callbackSelector (4 bytes)
	eventData := make([]byte, 0, 4+20+4+len(calldata)+32+20+4)
	eventData = append(eventData, make([]byte, 4)...)
	binary.BigEndian.PutUint32(eventData[0:4], targetShardID)
	eventData = append(eventData, targetAddr.Bytes()...)
	eventData = append(eventData, make([]byte, 4)...)
	binary.BigEndian.PutUint32(eventData[len(eventData)-4:], uint32(len(calldata)))
	eventData = append(eventData, calldata...)
	// value (32 bytes)
	valueBytes := make([]byte, 32)
	value.FillBytes(valueBytes)
	eventData = append(eventData, valueBytes...)
	// callbackAddr (20 bytes)
	eventData = append(eventData, callbackAddr.Bytes()...)
	// callbackSelector (4 bytes)
	eventData = append(eventData, callbackSelector[:]...)

	// Precompile 地址（0x6D = 109）
	precompileAddr := common.BytesToAddress([]byte{109})

	// 发出事件
	evm.StateDB.AddLog(&types.Log{
		Address:     precompileAddr,
		Topics:      []common.Hash{eventSignature, requestIdHashForTopic},
		Data:        eventData,
		BlockNumber: evm.BlockNumber.Uint64(),
	})

	utils.Logger().Info().
		Uint64("requestId", requestId).
		Uint32("targetShardID", targetShardID).
		Str("targetAddr", targetAddr.Hex()).
		Str("callbackAddr", callbackAddr.Hex()).
		Str("caller", contract.CallerAddress.Hex()).
		Msg("[JOYUE] emitted CrossShardRequest event")

	// 返回 requestId 的哈希（32 bytes），合约可以用这个来追踪请求
	return requestIdHashForTopic, nil
}
