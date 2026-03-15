package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE P2P 缓存查询工具

用途：
- 通过 precompile 查询节点层面的 P2P 缓存
- 支持查询单个键或列出某个合约的所有缓存

Precompile 地址：
- 读单个缓存：0x0000000000000000000000000000000000000064 (100)
- 查询所有缓存：0x0000000000000000000000000000000000000066 (102)
*/

const (
	// Precompile 地址
	cacheGetPrecompileAddr  = "0x0000000000000000000000000000000000000064" // 100
	cacheListPrecompileAddr = "0x0000000000000000000000000000000000000066" // 102
)

// 缓存查询结果 ABI（用于解码返回值）
const cacheResultABI = `[
  {
    "name": "get",
    "type": "function",
    "outputs": [
      {"name": "value", "type": "bytes"},
      {"name": "version", "type": "uint64"},
      {"name": "ok", "type": "bool"}
    ]
  }
]`

func main() {
	var (
		rpcURL   = flag.String("rpc", "http://127.0.0.1:9500", "RPC URL")
		contract = flag.String("contract", "", "合约地址（必需）")
		shardID  = flag.Uint("shard", 0, "合约所在分片 ID（可选，默认 0）")
		key      = flag.String("key", "", "缓存键（32 字节 hex，不带 0x）")
		list     = flag.Bool("list", false, "列出合约的所有缓存键")
	)
	flag.Parse()

	if *contract == "" {
		log.Fatal("必须指定 --contract 参数")
	}

	contractAddr := common.HexToAddress(*contract)
	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}

	if *list {
		// 查询所有缓存
		queryAllCache(ctx, client, contractAddr)
	} else {
		if *key == "" {
			log.Fatal("查询单个缓存时必须指定 --key 参数")
		}
		// 查询单个缓存
		querySingleCache(ctx, client, contractAddr, *key)
	}
}

// querySingleCache 查询单个缓存键
func querySingleCache(ctx context.Context, client *ethclient.Client, contractAddr common.Address, keyHex string) {
	// 解析 key
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		log.Fatalf("解析 key 失败: %v", err)
	}
	if len(keyBytes) != 32 {
		log.Fatalf("key 必须是 32 字节，当前是 %d 字节", len(keyBytes))
	}
	keyHash := common.BytesToHash(keyBytes)

	// 构造调用数据：contractAddr (20 bytes) + key (32 bytes)
	shardBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(shardBytes, uint32(*shardID))
	input := append(contractAddr.Bytes(), shardBytes...)
	input = append(input, keyHash.Bytes()...)

	// 调试：确认地址格式
	fmt.Printf("🔍 查询参数确认:\n")
	fmt.Printf("   合约地址: %s\n", contractAddr.Hex())
	fmt.Printf("   键 (原始): %s\n", keyHex)
	fmt.Printf("   键 (解析后): %s\n", keyHash.Hex())

	// 调用 precompile
	precompileAddr := common.HexToAddress(cacheGetPrecompileAddr)
	fmt.Printf("🔍 调用 precompile:\n")
	fmt.Printf("   Precompile 地址: %s\n", precompileAddr.Hex())
	fmt.Printf("   合约地址: %s\n", contractAddr.Hex())
	fmt.Printf("   键: %s\n", keyHash.Hex())
	fmt.Printf("   输入数据长度: %d 字节\n", len(input))
	fmt.Printf("   输入数据 (hex): %s\n", hex.EncodeToString(input))

	result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &precompileAddr,
		Data: input,
	}, nil)
	if err != nil {
		log.Fatalf("调用 precompile 失败: %v", err)
	}

	// 调试：打印原始返回数据
	fmt.Printf("🔍 Precompile 返回:\n")
	if len(result) == 0 {
		fmt.Printf("   ⚠️  返回数据为空（缓存未命中）\n")
	} else {
		fmt.Printf("   返回数据长度: %d 字节\n", len(result))
		fmt.Printf("   前 128 字节 (hex): %s\n", hex.EncodeToString(result[:min(128, len(result))]))
	}

	// 解析返回结果
	abiObj, err := abi.JSON(strings.NewReader(cacheResultABI))
	if err != nil {
		log.Fatalf("解析 ABI 失败: %v", err)
	}

	var value []byte
	var version uint64
	var ok bool

	err = abiObj.UnpackIntoInterface([]interface{}{&value, &version, &ok}, "get", result)
	if err != nil {
		// 如果 ABI 解析失败，尝试手动解析
		fmt.Printf("⚠️  ABI 解析失败，尝试手动解析...\n")
		fmt.Printf("🔍 错误信息: %v\n", err)
		value, version, ok = parseCacheResultManually(result)
		if !ok && len(result) >= 128 {
			// 调试：打印解析过程中的值
			offset := new(big.Int).SetBytes(result[0:32]).Uint64()
			length := new(big.Int).SetBytes(result[32:64]).Uint64()
			versionDebug := new(big.Int).SetBytes(result[64:96]).Uint64()
			okDebug := new(big.Int).SetBytes(result[96:128]).Uint64() != 0
			fmt.Printf("🔍 手动解析结果: offset=%d, length=%d, version=%d, ok=%v\n", offset, length, versionDebug, okDebug)
		}
	}

	if !ok {
		fmt.Printf("❌ 缓存未命中或 precompile 未启用\n")
		return
	}

	// 将 value 解析为 uint256（如果可能）
	// value 可能是任意长度的 bytes，需要转换为 uint256
	var valueUint *big.Int
	if len(value) > 0 {
		// 如果 value 长度小于 32 字节，需要左填充到 32 字节
		if len(value) < 32 {
			paddedValue := make([]byte, 32)
			copy(paddedValue[32-len(value):], value)
			valueUint = new(big.Int).SetBytes(paddedValue)
		} else {
			valueUint = new(big.Int).SetBytes(value[:32])
		}
	}

	fmt.Printf("✅ 缓存命中\n")
	fmt.Printf("合约地址: %s\n", contractAddr.Hex())
	fmt.Printf("键: %s\n", keyHash.Hex())
	fmt.Printf("版本: %d\n", version)
	fmt.Printf("值 (hex): %s\n", hex.EncodeToString(value))
	if valueUint != nil {
		fmt.Printf("值 (uint256): %s\n", valueUint.String())
	}
}

// queryAllCache 查询合约的所有缓存
func queryAllCache(ctx context.Context, client *ethclient.Client, contractAddr common.Address) {
	// 构造调用数据：contractAddr (20 bytes)
	shardBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(shardBytes, uint32(*shardID))
	input := append(contractAddr.Bytes(), shardBytes...)

	// 调用 precompile
	precompileAddr := common.HexToAddress(cacheListPrecompileAddr)
	result, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &precompileAddr,
		Data: input,
	}, nil)
	if err != nil {
		log.Fatalf("调用 precompile 失败: %v", err)
	}

	// 手动解析返回结果（格式：keys[] + values[] + versions[]）
	if len(result) == 0 {
		fmt.Printf("❌ 没有找到缓存条目\n")
		return
	}

	// 解析格式：keysOffset (32) + valuesOffset (32) + versionsOffset (32) + keys[] + values[] + versions[]
	// 前 96 字节是三个 offset
	keysOffset := new(big.Int).SetBytes(result[0:32]).Uint64()
	valuesOffset := new(big.Int).SetBytes(result[32:64]).Uint64()
	versionsOffset := new(big.Int).SetBytes(result[64:96]).Uint64()

	// 读取 keys 数组长度（在 keysOffset 位置）
	keysLen := new(big.Int).SetBytes(result[keysOffset : keysOffset+32]).Uint64()

	fmt.Printf("✅ 找到 %d 个缓存条目\n", keysLen)
	fmt.Printf("合约地址: %s\n\n", contractAddr.Hex())

	// 解析 keys（从 keysOffset + 32 开始，跳过长度字段）
	keysStart := int(keysOffset) + 32
	for i := uint64(0); i < keysLen; i++ {
		keyOffset := keysStart + int(i)*32
		if keyOffset+32 > len(result) {
			fmt.Printf("⚠️  警告：键 #%d 超出结果范围\n", i+1)
			break
		}
		keyHash := common.BytesToHash(result[keyOffset : keyOffset+32])

		// 解析 value（values 数组格式：length (32) + offsets[] (每个 32 bytes) + lengths[] (每个 32 bytes) + data[]）
		// 注意：encodeCacheListResult 中，每个 value 的 offset 指向 value data 的开始位置
		// value data 只包含实际数据（padded），不包含 offset 和 length
		valuesArrayStart := int(valuesOffset)
		valuesLength := new(big.Int).SetBytes(result[valuesArrayStart : valuesArrayStart+32]).Uint64()
		if i >= valuesLength {
			fmt.Printf("⚠️  警告：值 #%d 超出范围\n", i+1)
			break
		}
		// 读取第 i 个 value 的 offset（指向 value data 的开始位置）
		valueOffsetPtr := valuesArrayStart + 32 + int(i)*32
		if valueOffsetPtr+32 > len(result) {
			fmt.Printf("⚠️  警告：value offset #%d 超出结果范围\n", i+1)
			break
		}
		valueDataOffset := int(new(big.Int).SetBytes(result[valueOffsetPtr : valueOffsetPtr+32]).Uint64())
		// 读取第 i 个 value 的 length（在 offsets[] 之后）
		valueLengthPtr := valuesArrayStart + 32 + int(valuesLength)*32 + int(i)*32
		if valueLengthPtr+32 > len(result) {
			fmt.Printf("⚠️  警告：value length #%d 超出结果范围\n", i+1)
			break
		}
		valueLength := int(new(big.Int).SetBytes(result[valueLengthPtr : valueLengthPtr+32]).Uint64())
		// 边界检查
		if valueDataOffset < 0 || valueDataOffset >= len(result) {
			fmt.Printf("⚠️  警告：value data offset #%d 超出范围: %d (结果长度: %d)\n", i+1, valueDataOffset, len(result))
			break
		}
		if valueDataOffset+valueLength > len(result) {
			fmt.Printf("⚠️  警告：value data #%d 超出范围: offset=%d, length=%d, 结果长度=%d\n", i+1, valueDataOffset, valueLength, len(result))
			// 只读取能读取的部分
			valueLength = len(result) - valueDataOffset
		}
		// 读取 value data（只包含实际数据，不包含 offset 和 length）
		valueBytes := result[valueDataOffset : valueDataOffset+valueLength]
		// 如果 value 是 uint256，转换为 big.Int
		var valueUint *big.Int
		if len(valueBytes) >= 32 {
			valueUint = new(big.Int).SetBytes(valueBytes[:32])
		} else {
			paddedValue := make([]byte, 32)
			copy(paddedValue[32-len(valueBytes):], valueBytes)
			valueUint = new(big.Int).SetBytes(paddedValue)
		}

		// 解析 version（versions 数组格式：length (32) + versions[] (每个 32 bytes)）
		versionsArrayStart := int(versionsOffset)
		versionsLength := new(big.Int).SetBytes(result[versionsArrayStart : versionsArrayStart+32]).Uint64()
		if i >= versionsLength {
			fmt.Printf("⚠️  警告：版本 #%d 超出范围\n", i+1)
			break
		}
		versionOffset := versionsArrayStart + 32 + int(i)*32
		version := new(big.Int).SetBytes(result[versionOffset : versionOffset+32]).Uint64()

		fmt.Printf("条目 #%d:\n", i+1)
		fmt.Printf("  键: %s\n", keyHash.Hex())
		fmt.Printf("  值: %s (uint256: %s)\n", hex.EncodeToString(valueBytes), valueUint.String())
		fmt.Printf("  版本: %d\n\n", version)
	}
}

// parseCacheResultManually 手动解析缓存查询结果
// 格式：offset (32) + length (32) + version (32) + ok (32) + data (padded)
func parseCacheResultManually(result []byte) ([]byte, uint64, bool) {
	if len(result) < 128 {
		return nil, 0, false
	}

	// offset for bytes (应该为 96 = 3*32)
	offset := new(big.Int).SetBytes(result[0:32]).Uint64()
	if offset != 96 {
		return nil, 0, false
	}

	// length of bytes
	length := new(big.Int).SetBytes(result[32:64]).Uint64()

	// version
	version := new(big.Int).SetBytes(result[64:96]).Uint64()

	// ok
	ok := new(big.Int).SetBytes(result[96:128]).Uint64() != 0

	// value data
	var value []byte
	if length > 0 && int(offset)+int(length) <= len(result) {
		value = make([]byte, length)
		copy(value, result[int(offset):int(offset)+int(length)])
	}

	return value, version, ok
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
