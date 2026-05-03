package vm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/crypto/hash"
	"github.com/harmony-one/harmony/internal/params"
	"github.com/harmony-one/harmony/internal/utils"
)

/*
JOYUE Cross-Shard Executor Precompile

预编译地址：0x0000000000000000000000000000000000000074 (0x74 = 116)

功能：
- 统一收口所有跨分片合约调用
- 在目标分片执行目标合约调用
- 通过 RPC precompile (0x6D) 回调源分片

调用格式：
executeAndCallback(uint32,address,bytes4,uint256,address,uint256,bytes)
- sourceShardID: 源分片 ID
- callbackAddr: 回调合约地址
- callbackSelector: 回调函数选择器
- requestId: 请求 ID
- target: 目标合约地址
- targetValue: 调用目标合约时的 value
- targetCalldata: 调用目标合约的 calldata

返回：
- bool: 执行是否成功
- bytes: 执行返回值
- bytes32: 回调交易哈希
*/

// joyueExecutorPrecompile 已注册到 WriteCapablePrecompiledContractsJoyue

// 函数选择器常量
// 注意：函数选择器是通过 keccak256("函数签名") 的前 4 字节计算的
// 函数签名：executeAndCallback(uint32,address,bytes4,uint256,address,uint256,bytes)
// 实际收到的 selector: 0x2b5d76a4 (727545508 十进制)
// 之前错误的 selector: 0x8f4ffcb1, 0x2B5E8F24
const (
	executeAndCallbackSelector = 0x2b5d76a4 // 使用实际收到的 selector
)

// joyueExecutorPrecompile 实现 JOYUE Executor 的预编译合约
type joyueExecutorPrecompile struct{}

// RequiredGas 返回执行 precompile 所需的 gas
func (c *joyueExecutorPrecompile) RequiredGas(evm *EVM, contract *Contract, input []byte) (uint64, error) {
	if len(input) < 4 {
		return 0, errors.New("JOYUE Executor: invalid input length")
	}

	// 基础 gas
	baseGas := uint64(5000)
	dataGas := uint64(len(input)+31) / 32 * params.IdentityPerWordGas

	// 根据函数选择器调整 gas
	selector := binary.BigEndian.Uint32(input[0:4])
	switch selector {
	case executeAndCallbackSelector:
		// 基础 gas + 数据 gas + 执行目标合约的 gas（估算）
		return baseGas + dataGas + uint64(10000), nil
	default:
		return baseGas + dataGas, nil
	}
}

// RunWriteCapable 执行预编译合约
func (c *joyueExecutorPrecompile) RunWriteCapable(evm *EVM, contract *Contract, input []byte) ([]byte, error) {
	if len(input) < 4 {
		return nil, errors.New("JOYUE Executor: invalid input length")
	}

	// 解析函数选择器
	selector := binary.BigEndian.Uint32(input[0:4])
	paramsInfo := input[4:]

	// contract.Address() 是 Executor 预编译的地址（0x74）
	// 这里不需要 coordinatorAddr，因为 Executor 是独立的预编译

	utils.Logger().Info().
		Str("executorAddr", contract.Address().Hex()).
		Str("caller", contract.CallerAddress.Hex()).
		Uint32("selector", selector).
		Int("inputLen", len(input)).
		Msg("[JOYUE Executor] RunWriteCapable called")

	switch selector {
	case executeAndCallbackSelector:
		return c.handleExecuteAndCallback(evm, contract, paramsInfo)
	default:
		utils.Logger().Warn().
			Uint32("selector", selector).
			Str("expected", fmt.Sprintf("0x%08x", executeAndCallbackSelector)).
			Msg("[JOYUE Executor] unknown function selector")
		return nil, errors.New("JOYUE Executor: unknown function selector")
	}
}

// handleExecuteAndCallback 处理 executeAndCallback 调用
func (c *joyueExecutorPrecompile) handleExecuteAndCallback(evm *EVM, contract *Contract, params []byte) ([]byte, error) {
	utils.Logger().Info().
		Str("executorAddr", contract.Address().Hex()).
		Str("caller", contract.CallerAddress.Hex()).
		Int("paramsLen", len(params)).
		Msg("[JOYUE Executor] handleExecuteAndCallback called")

	// 解码参数
	// executeAndCallback(uint32,address,bytes4,uint256,address,uint256,bytes)
	// ABI 编码格式：所有参数都是 32 字节对齐
	// - sourceShardID: uint32 (32 bytes, 在 params[0:32]，左填充，值在最后 4 字节)
	// - callbackAddr: address (32 bytes, 在 params[32:64]，左填充，地址在 [12:32])
	// - callbackSelector: bytes4 (32 bytes, 在 params[64:96]，右填充，数据在 [0:4])
	// - requestId: uint256 (32 bytes, 在 params[96:128])
	// - target: address (32 bytes, 在 params[128:160]，左填充，地址在 [12:32])
	// - targetValue: uint256 (32 bytes, 在 params[160:192])
	// - targetCalldata: bytes (动态类型，偏移量在 params[192:224])

	if len(params) < 224 {
		utils.Logger().Error().
			Int("paramsLen", len(params)).
			Int("expectedMin", 224).
			Msg("[JOYUE Executor] invalid params length")
		return nil, errors.New("JOYUE Executor: invalid params length")
	}

	// 解析固定参数（按照 ABI 标准格式）
	// sourceShardID: uint32 (左填充到 32 字节，值在最后 4 字节)
	sourceShardID := binary.BigEndian.Uint32(params[28:32])
	// callbackAddr: address (左填充到 32 字节，地址在 [12:32])
	callbackAddr := common.BytesToAddress(params[44:64])
	// callbackSelector: bytes4 (右填充到 32 字节，数据在 [0:4])
	callbackSelector := params[64:68]
	// requestId: uint256 (32 bytes)
	requestId := new(big.Int).SetBytes(params[96:128])
	// target: address (左填充到 32 字节，地址在 [12:32])
	target := common.BytesToAddress(params[140:160])
	// targetValue: uint256 (32 bytes)
	targetValue := new(big.Int).SetBytes(params[160:192])

	utils.Logger().Info().
		Uint32("sourceShardID", sourceShardID).
		Str("callbackAddr", callbackAddr.Hex()).
		Str("target", target.Hex()).
		Str("requestId", requestId.String()).
		Str("targetValue", targetValue.String()).
		Msg("[JOYUE Executor] parsed parameters")

	// 解析动态参数 targetCalldata
	// 偏移量在 params[192:224]（第 7 个参数的位置）
	calldataOffset := int(binary.BigEndian.Uint64(params[216:224])) // 偏移量是 uint256，但实际值不会超过 uint64
	if calldataOffset+32 > len(params) {
		utils.Logger().Error().
			Int("calldataOffset", calldataOffset).
			Int("paramsLen", len(params)).
			Msg("[JOYUE Executor] invalid calldata offset")
		return nil, errors.New("JOYUE Executor: invalid calldata offset")
	}
	// 读取长度（32 字节）
	calldataLen := int(binary.BigEndian.Uint64(params[calldataOffset+24 : calldataOffset+32]))
	if calldataOffset+32+calldataLen > len(params) {
		utils.Logger().Error().
			Int("calldataOffset", calldataOffset).
			Int("calldataLen", calldataLen).
			Int("paramsLen", len(params)).
			Msg("[JOYUE Executor] invalid calldata length")
		return nil, errors.New("JOYUE Executor: invalid calldata length")
	}
	targetCalldata := params[calldataOffset+32 : calldataOffset+32+calldataLen]

	utils.Logger().Info().
		Str("target", target.Hex()).
		Int("targetCalldataLen", len(targetCalldata)).
		Hex("targetCalldataSelector", targetCalldata[:min(4, len(targetCalldata))]).
		Hex("targetCalldata4-36", targetCalldata[4:min(36, len(targetCalldata))]).
		Msg("[JOYUE Executor] handleExecuteAndCallback: Parsed targetCalldata - Will call target contract")

	// 验证 msg.value 是否等于 targetValue
	if contract.Value().Cmp(targetValue) != 0 {
		return nil, errors.New("JOYUE Executor: msg.value != targetValue")
	}

	// 执行目标合约调用
	ok, returnData := c.executeTarget(evm, contract, target, targetValue, targetCalldata)

	// 发送回调
	callbackTxHash, err := c.sendCallback(evm, contract, sourceShardID, callbackAddr, callbackSelector, requestId, ok, returnData)
	if err != nil {
		// 即使回调失败，也返回执行结果
		utils.Logger().Error().
			Err(err).
			Uint32("sourceShardID", sourceShardID).
			Str("callbackAddr", callbackAddr.Hex()).
			Msg("[JOYUE Executor] failed to send callback")
	}

	// 编码返回结果
	// 返回: (bool ok, bytes returnData, bytes32 callbackTxHash)
	return c.encodeExecuteAndCallbackResult(ok, returnData, callbackTxHash), nil
}

// executeTarget 执行目标合约调用
func (c *joyueExecutorPrecompile) executeTarget(evm *EVM, contract *Contract, target common.Address, value *big.Int, calldata []byte) (bool, []byte) {
	utils.Logger().Info().
		Str("target", target.Hex()).
		Str("caller", contract.CallerAddress.Hex()).
		Int("calldataLen", len(calldata)).
		Hex("calldataSelector", calldata[:min(4, len(calldata))]).
		Msg("[JOYUE Executor] executeTarget: Calling target contract")

	// 使用 EVM.Call 执行目标合约
	ret, leftOverGas, err := evm.Call(contract, target, calldata, contract.Gas, value)
	if err != nil {
		utils.Logger().Error().
			Err(err).
			Str("target", target.Hex()).
			Int("calldataLen", len(calldata)).
			Int("returnDataLen", len(ret)).
			Msg("[JOYUE Executor] executeTarget: target call failed")
		// 将 revert / require 的 returnData 透传给源分片回调（如 ChainspaceWalletSimulator 的自定义 error），便于解码 finalReason
		return false, ret
	}

	utils.Logger().Info().
		Str("target", target.Hex()).
		Int("returnDataLen", len(ret)).
		Msg("[JOYUE Executor] executeTarget: target call succeeded")

	// 更新剩余 gas
	contract.Gas = leftOverGas

	return true, ret
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sendCallback 发送回调到源分片
// 复用 joyueRpcOracleSendTxPrecompile 的事件发出逻辑
func (c *joyueExecutorPrecompile) sendCallback(evm *EVM, contract *Contract, sourceShardID uint32, callbackAddr common.Address, callbackSelector []byte, requestId *big.Int, ok bool, returnData []byte) (common.Hash, error) {
	// 构建回调 calldata
	// callbackSelector(requestId, ok, returnData)
	callbackCalldata := make([]byte, 0, 4+32+32+32+len(returnData))
	callbackCalldata = append(callbackCalldata, callbackSelector...)
	// requestId (uint256, 32 bytes)
	requestIdBytes := make([]byte, 32)
	requestId.FillBytes(requestIdBytes)
	callbackCalldata = append(callbackCalldata, requestIdBytes...)
	// ok (bool, 32 bytes, 左填充)
	okBytes := make([]byte, 32)
	if ok {
		okBytes[31] = 1
	}
	callbackCalldata = append(callbackCalldata, okBytes...)
	// returnData (bytes, 动态类型)
	// 偏移量 (32 bytes)
	offsetBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(offsetBytes[24:32], 96) // 3 * 32 = 96
	callbackCalldata = append(callbackCalldata, offsetBytes...)
	// 长度 (32 bytes)
	lenBytes := make([]byte, 32)
	binary.BigEndian.PutUint64(lenBytes[24:32], uint64(len(returnData)))
	callbackCalldata = append(callbackCalldata, lenBytes...)
	// 数据（左填充到 32 字节的倍数）
	paddedLen := (len(returnData) + 31) / 32 * 32
	paddedData := make([]byte, paddedLen)
	copy(paddedData, returnData)
	callbackCalldata = append(callbackCalldata, paddedData...)

	// 生成 requestId（用于事件）
	// 使用与 joyueRpcOracleSendTxPrecompile 相同的逻辑
	txHash := evm.StateDB.TxHash()
	caller := contract.CallerAddress
	logIndex := evm.StateDB.TxIndex()

	requestIdData := append(txHash.Bytes(), caller.Bytes()...)
	requestIdData = append(requestIdData, make([]byte, 4)...)
	binary.BigEndian.PutUint32(requestIdData[len(requestIdData)-4:], uint32(logIndex))
	if len(callbackCalldata) > 0 {
		calldataLen := len(callbackCalldata)
		if calldataLen > 32 {
			calldataLen = 32
		}
		requestIdData = append(requestIdData, callbackCalldata[:calldataLen]...)
	}
	requestIdHash := hash.Keccak256Hash(requestIdData)
	requestIdUint64 := new(big.Int).SetBytes(requestIdHash[:]).Uint64()

	// 复用统一的事件发出逻辑
	callbackSelectorBytes := [4]byte{}
	copy(callbackSelectorBytes[:], callbackSelector)
	requestIdHashForTopic, err := emitCrossShardRequestEvent(evm, contract, sourceShardID, callbackAddr, big.NewInt(0), callbackCalldata, callbackAddr, callbackSelectorBytes, requestIdUint64)
	if err != nil {
		return common.Hash{}, err
	}

	utils.Logger().Info().
		Uint32("sourceShardID", sourceShardID).
		Str("callbackAddr", callbackAddr.Hex()).
		Str("requestId", requestId.String()).
		Msg("[JOYUE Executor] sent callback via event precompile")

	// 返回回调交易哈希（使用 requestIdHashForTopic）
	return requestIdHashForTopic, nil
}

// encodeExecuteAndCallbackResult 编码 executeAndCallback 的返回结果
func (c *joyueExecutorPrecompile) encodeExecuteAndCallbackResult(ok bool, returnData []byte, callbackTxHash common.Hash) []byte {
	// 返回格式: (bool ok, bytes returnData, bytes32 callbackTxHash)
	// ABI 编码：
	// - ok: bool (32 bytes, 左填充)
	// - returnData: bytes (偏移量 32 bytes + 长度 32 bytes + 数据)
	// - callbackTxHash: bytes32 (32 bytes)

	// 计算偏移量
	returnDataOffset := 32
	returnDataLenOffset := 64
	returnDataDataOffset := 96
	callbackTxHashOffset := 96 + ((len(returnData) + 31) / 32 * 32)

	totalLen := callbackTxHashOffset + 32
	output := make([]byte, totalLen)

	// ok (bool, 32 bytes, 左填充)
	if ok {
		output[31] = 1
	}

	// returnData 偏移量 (32 bytes)
	binary.BigEndian.PutUint64(output[returnDataOffset+24:returnDataOffset+32], uint64(returnDataLenOffset))

	// returnData 长度 (32 bytes)
	binary.BigEndian.PutUint64(output[returnDataLenOffset+24:returnDataLenOffset+32], uint64(len(returnData)))

	// returnData 数据（左填充到 32 字节的倍数）
	copy(output[returnDataDataOffset:returnDataDataOffset+len(returnData)], returnData)

	// callbackTxHash (32 bytes)
	copy(output[callbackTxHashOffset:callbackTxHashOffset+32], callbackTxHash.Bytes())

	return output
}
