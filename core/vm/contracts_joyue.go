package vm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
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
	common.BytesToAddress([]byte{101}): &joyueCacheWritePrecompile{}, // 0x65 = 101 (写缓存)
}

// joyueCachePrecompile 实现 P2P 缓存访问的 precompile
type joyueCachePrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueCachePrecompile) RequiredGas(input []byte) uint64 {
	// 基础 gas + 输入数据 gas
	// input = 20 bytes (contractAddr) + 32 bytes (key) = 52 bytes
	baseGas := uint64(100)
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas
}

// Run 执行 precompile，返回缓存值
func (c *joyueCachePrecompile) Run(input []byte) ([]byte, error) {
	// 输入格式：20 bytes (contractAddr) + 32 bytes (key)
	if len(input) < 52 {
		return nil, errors.New("JOYUE: invalid input length")
	}

	// 解析输入
	contractAddr := common.BytesToAddress(input[0:20])
	key := common.BytesToHash(input[20:52])

	// 从全局缓存广播器获取缓存
	cacheBroadcaster := joyue.GetGlobalCacheBroadcaster()
	if cacheBroadcaster == nil {
		// 缓存未启用，返回空值
		return encodeCacheResult(nil, 0, false), nil
	}

	// 获取缓存值
	value, version, ok := cacheBroadcaster.GetCache(contractAddr, key)

	// 编码返回结果：abi.encode(bytes value, uint64 version, bool ok)
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
	// 输入格式：20 bytes (contractAddr)
	if len(input) < 20 {
		return nil, errors.New("JOYUE: invalid input length for cache list")
	}

	// 解析输入
	contractAddr := common.BytesToAddress(input[0:20])

	// 从全局缓存广播器获取所有缓存
	cacheBroadcaster := joyue.GetGlobalCacheBroadcaster()
	if cacheBroadcaster == nil {
		// 缓存未启用，返回空列表
		return encodeCacheListResult(nil), nil
	}

	// 获取该合约的所有缓存条目
	entries := cacheBroadcaster.GetAllCacheForContract(contractAddr)

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
	// input = 20 bytes (contractAddr) + 32 bytes (key) + 32 bytes (value) + 32 bytes (version) = 116 bytes
	baseGas := uint64(200) // 写入操作需要更多 gas
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas
	return baseGas + dataGas, nil
}

// RunWriteCapable 执行 precompile，更新本地缓存（不广播）
func (c *joyueCacheWritePrecompile) RunWriteCapable(evm *EVM, contract *Contract, input []byte) ([]byte, error) {
	// 输入格式：20 bytes (contractAddr) + 32 bytes (key) + 32 bytes (value) + 32 bytes (version)
	if len(input) < 116 {
		return nil, errors.New("JOYUE: invalid input length for cache write")
	}

	// 解析输入
	contractAddr := common.BytesToAddress(input[0:20])
	key := common.BytesToHash(input[20:52])
	value := input[52:84]         // 32 bytes value
	versionBytes := input[84:116] // 32 bytes version (实际只有最后 8 bytes 有效)

	// 解析 version (uint64, 从最后 8 bytes 读取)
	version := binary.BigEndian.Uint64(versionBytes[24:32])

	// 从全局缓存广播器更新本地缓存（不广播）
	cacheBroadcaster := joyue.GetGlobalCacheBroadcaster()
	if cacheBroadcaster == nil {
		utils.Logger().Error().
			Str("contract", contractAddr.Hex()).
			Str("key", key.Hex()).
			Uint64("version", version).
			Msg("[JOYUE] cache broadcaster not initialized in precompile")
		return nil, errors.New("JOYUE: cache broadcaster not initialized")
	}

	valueUint := new(big.Int).SetBytes(value)
	utils.Logger().Info().
		Str("contract", contractAddr.Hex()).
		Str("key", key.Hex()).
		Uint64("version", version).
		Str("value", valueUint.String()).
		Int("valueLen", len(value)).
		Msg("[JOYUE] precompile: calling SetCacheLocal")

	// 更新本地缓存（不广播，只有主合约的修改才广播）
	cacheBroadcaster.SetCacheLocal(contractAddr, key, value, version)

	utils.Logger().Info().
		Str("contract", contractAddr.Hex()).
		Str("key", key.Hex()).
		Uint64("version", version).
		Msg("[JOYUE] precompile: SetCacheLocal completed")

	// 返回空表示成功
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
