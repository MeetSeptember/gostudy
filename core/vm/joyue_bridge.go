package vm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// ShadowReader 定义了 EVM 读取外部状态所需的接口
// Node 层需要实现此接口并注入进来
type ShadowReader interface {
	GetShadowState(shardID uint32, addr common.Address, key []byte) []byte
}

// GlobalShadowReader 是一个全局钩子，用于实验阶段的依赖注入
// 避免修改 NewEVM 的深层调用链
var GlobalShadowReader ShadowReader

// SetGlobalShadowReader 允许 Node 层注入具体的实现
func SetGlobalShadowReader(r ShadowReader) {
	GlobalShadowReader = r
}

// shadowReadContract 是预编译合约的实现结构体
type shadowReadContract struct{}

// RequiredGas 计算执行此预编译合约所需的 Gas [新增修正]
// 由于这是内存读取操作，成本非常低，我们给它一个固定的低 Gas 费 (例如 200)
// input: 调用输入数据
func (c *shadowReadContract) RequiredGas(input []byte) uint64 {
	// 您可以根据 input 长度计算动态 Gas，例如: base + len(input) * price
	// 这里为了简单，返回固定值
	return 200
}

// Run 执行预编译合约逻辑
// input 格式必须严格为: [ShardID (32 bytes)] [Address (20 bytes)] [Key (32 bytes)] = 84 bytes
func (c *shadowReadContract) Run(input []byte) ([]byte, error) {
	const (
		ShardIDSize = 32
		AddressSize = 20
		KeySize     = 32
		TotalSize   = ShardIDSize + AddressSize + KeySize // 84 bytes
	)

	// 1. 长度检查
	// 如果输入数据不足，直接返回 nil (不报错，视为读取空)
	if len(input) < TotalSize {
		return nil, nil
	}

	// 2. 解析 ShardID (前 32 字节)
	// 使用 BigInt 解析以防溢出，然后转为 uint64/uint32
	shardIDBytes := input[0:ShardIDSize]
	shardIDBig := new(big.Int).SetBytes(shardIDBytes)
	if !shardIDBig.IsUint64() {
		return nil, nil // ShardID 过大，非法
	}
	shardID := uint32(shardIDBig.Uint64())

	// 3. 解析 Address (中间 20 字节)
	addrBytes := input[ShardIDSize : ShardIDSize+AddressSize]
	address := common.BytesToAddress(addrBytes)

	// 4. 解析 StorageKey (最后 32 字节)
	keyBytes := input[ShardIDSize+AddressSize : TotalSize]

	// 5. 调用注入的读取器
	if GlobalShadowReader != nil {
		return GlobalShadowReader.GetShadowState(shardID, address, keyBytes), nil
	}

	// 如果服务未初始化，返回 nil
	return nil, nil
}
