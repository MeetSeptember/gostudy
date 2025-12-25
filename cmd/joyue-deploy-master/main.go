package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

/*
JOYUE 部署工具：部署 JoyueMaster（shard0 或任意 shard）

你现在的 Master 合约构造函数签名是：
  constructor(bytes32 salt, bytes agentCreationCode, uint32 masterShardId)

其中 agentCreationCode 是“完整的 Agent creation code”（也就是你在其它 shard
做合约创建交易时要放到 tx.data 里的那段 bytes）。

这个工具做的事情：
1）读取 JoyueMaster 的 creation bytecode（不包含 constructor args）。
2）读取/解析 agentCreationCode（完整 bytes）。
3）用 ABI 规则编码 constructor 参数（特别是动态 bytes 的编码，很容易手拼出错）。
4）拼出部署交易 data = masterBin || ctorArgs，并发送合约创建交易（to=nil）。
5）可选等待 receipt 并输出部署出来的 master 合约地址。

注意：
- 把 agentCreationCode 作为 bytes 传入 constructor，会让部署交易变得很大、很贵。
- 本工具只是帮助你把“动态 bytes 参数”正确编码，避免手工拼接出错。
*/

// =========================
// 内置 BIN（按你的需求：后续直接 go run 就行）
//
// 说明：
// - 你可以先用 solc 编译一次，把 .bin 的 hex 粘到下面两个常量里（不要带 0x；不要包含空格/换行）。
// - 之后运行时如果不传 --master-bin/--master-bin-file，将默认使用 embeddedJoyueMasterBinHex。
// - 同理，agentCreationCode 默认使用 embeddedJoyueAgentCreationCodeHex。
//
// 注意：
// - JoyueMaster 当前 constructor 需要的是“完整 agentCreationCode（bytes）”，这里我们默认传入 agent 的 creation bytecode。
// - 如果你未来希望把 constructor args 也包含进去，可以把“bin+args 拼好的完整 tx.data”粘到 embeddedJoyueAgentCreationCodeHex。
// =========================

// JoyueMaster creation bytecode（不含 constructor args）
const embeddedJoyueMasterBinHex = "60e060405234801561001057600080fd5b5060405161057c38038061057c8339818101604052810190610032919061029d565b8260808181525050818051906020012060a081815250508063ffffffff1660c08163ffffffff1681525050823073ffffffffffffffffffffffffffffffffffffffff167fefdeb09d7e10be4d0f0a1008af53d985776824c3026f0505ac302986d23765e684846040516100a6929190610370565b60405180910390a35050506103a0565b6000604051905090565b600080fd5b600080fd5b6000819050919050565b6100dd816100ca565b81146100e857600080fd5b50565b6000815190506100fa816100d4565b92915050565b600080fd5b600080fd5b6000601f19601f8301169050919050565b7f4e487b7100000000000000000000000000000000000000000000000000000000600052604160045260246000fd5b6101538261010a565b810181811067ffffffffffffffff821117156101725761017161011b565b5b80604052505050565b60006101856100b6565b9050610191828261014a565b919050565b600067ffffffffffffffff8211156101b1576101b061011b565b5b6101ba8261010a565b9050602081019050919050565b60005b838110156101e55780820151818401526020810190506101ca565b60008484015250505050565b60006102046101ff84610196565b61017b565b9050828152602081018484840111156102205761021f610105565b5b61022b8482856101c7565b509392505050565b600082601f83011261024857610247610100565b5b81516102588482602086016101f1565b91505092915050565b600063ffffffff82169050919050565b61027a81610261565b811461028557600080fd5b50565b60008151905061029781610271565b92915050565b6000806000606084860312156102b6576102b56100c0565b5b60006102c4868287016100eb565b935050602084015167ffffffffffffffff8111156102e5576102e46100c5565b5b6102f186828701610233565b925050604061030286828701610288565b9150509250925092565b600081519050919050565b600082825260208201905092915050565b60006103338261030c565b61033d8185610317565b935061034d8185602086016101c7565b6103568161010a565b840191505092915050565b61036a81610261565b82525050565b6000604082019050818103600083015261038a8185610328565b90506103996020830184610361565b9392505050565b60805160a05160c0516101b06103cc600039600060a20152600060ea0152600060c601526101b06000f3fe608060405234801561001057600080fd5b50600436106100415760003560e01c80630e2fcc0214610046578063bfa0b13314610064578063ce2ef08314610082575b600080fd5b61004e6100a0565b60405161005b919061012b565b60405180910390f35b61006c6100c4565b604051610079919061015f565b60405180910390f35b61008a6100e8565b604051610097919061015f565b60405180910390f35b7f000000000000000000000000000000000000000000000000000000000000000081565b7f000000000000000000000000000000000000000000000000000000000000000081565b7f000000000000000000000000000000000000000000000000000000000000000081565b600063ffffffff82169050919050565b6101258161010c565b82525050565b6000602082019050610140600083018461011c565b92915050565b6000819050919050565b61015981610146565b82525050565b60006020820190506101746000830184610150565b9291505056fea2646970667358221220850e04054708a6a6be47be438cf393fa92964bd644c3b7748057a5a03841963264736f6c634300081f0033"

// 默认的 agentCreationCode（完整 bytes，会被 master emit 到事件里，并被 relayer 转发）
const embeddedJoyueAgentCreationCodeHex = "60e060405234801561000f575f5ffd5b506040516101f33803806101f383398101604081905261002e91610068565b6001600160a01b0390921660805263ffffffff90811660a0521660c0526100b5565b805163ffffffff81168114610063575f5ffd5b919050565b5f5f5f6060848603121561007a575f5ffd5b83516001600160a01b0381168114610090575f5ffd5b925061009e60208501610050565b91506100ac60408501610050565b90509250925092565b60805160a05160c0516101176100dc5f395f608201525f604201525f60a801526101175ff3fe6080604052348015600e575f5ffd5b5060043610603a575f3560e01c80630e2fcc0214603e578063af556eeb14607e578063ee97f7f31460a4575b5f5ffd5b60647f000000000000000000000000000000000000000000000000000000000000000081565b60405163ffffffff90911681526020015b60405180910390f35b60647f000000000000000000000000000000000000000000000000000000000000000081565b60ca7f000000000000000000000000000000000000000000000000000000000000000081565b6040516001600160a01b039091168152602001607556fea26469706673582212209dcd5f6d1f68d0c743823d9189b35908adc03f07d283101a7787df82289b660664736f6c634300081f0033"

func main() {
	var (
		rpcURL = flag.String("rpc", "http://127.0.0.1:9500", "目标分片 RPC（HTTP），默认 shard0:9500")

		privateKeyHex = flag.String("private-key", "", "部署账户私钥 hex（不带 0x）")

		// master creation bytecode（不含 constructor args）
		masterBinFile = flag.String("master-bin-file", "", "JoyueMaster.bin 文件路径（可选；不传则用内置 embeddedJoyueMasterBinHex）")
		masterBinHex  = flag.String("master-bin", "", "JoyueMaster creation bytecode hex（可选；不传则用内置 embeddedJoyueMasterBinHex）")

		// agentCreationCode：完整的 agent creation code（将被 emit 到事件里，也将被 relayer 转发到其它 shard）
		agentCodeFile = flag.String("agent-code-file", "", "JoyueAgent.bin 文件路径（可选；不传则用内置 embeddedJoyueAgentCreationCodeHex）")
		agentCodeHex  = flag.String("agent-code", "", "完整 agent creation code hex（可选；不传则用内置 embeddedJoyueAgentCreationCodeHex）")

		saltHex = flag.String("salt", "0x0", "bytes32 salt（0x... 或不带 0x；不足 32 字节左侧补 0）")
		shardID = flag.Uint64("master-shard-id", 0, "masterShardId（写入事件，默认 0）")

		gasLimit   = flag.Uint64("gas", 3_500_000, "gasLimit")
		gasTipGwei = flag.Int64("gas-tip-gwei", 1, "EIP-1559 priority fee（gwei），仅在 baseFee 存在时使用")

		waitReceipt = flag.Bool("wait", true, "是否等待 receipt 并打印 master 合约地址")
		timeout     = flag.Duration("timeout", 90*time.Second, "等待 receipt 超时")

		warnLarge = flag.Uint64("warn-large-agent-bytes", 128*1024, "agentCreationCode 超过该字节数时打印警告（默认 128KB）")
	)
	flag.Parse()

	if *privateKeyHex == "" {
		flag.Usage()
		log.Fatal("缺少参数：--private-key")
	}

	masterBin, err := readHexFromFileOrFlagOrEmbedded(*masterBinFile, *masterBinHex, embeddedJoyueMasterBinHex, "master-bin")
	if err != nil {
		log.Fatalf("读取 master-bin 失败: %v", err)
	}
	agentCreationCode, err := readHexFromFileOrFlagOrEmbedded(*agentCodeFile, *agentCodeHex, embeddedJoyueAgentCreationCodeHex, "agent-code")
	if err != nil {
		log.Fatalf("读取 agentCreationCode 失败: %v", err)
	}

	if *warnLarge > 0 && uint64(len(agentCreationCode)) > *warnLarge {
		log.Printf("[警告] agentCreationCode 很大：len=%d bytes（部署会很贵，且可能受 block gas / tx size 限制影响）", len(agentCreationCode))
	}

	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*privateKeyHex, "0x"))
	if err != nil {
		log.Fatalf("私钥格式错误: %v", err)
	}
	from := crypto.PubkeyToAddress(privKey.PublicKey)

	salt, err := parseBytes32(*saltHex)
	if err != nil {
		log.Fatalf("salt 解析失败: %v", err)
	}

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}

	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Fatalf("获取 chainID 失败: %v", err)
	}

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		log.Fatalf("获取 nonce 失败: %v", err)
	}

	ctorArgs, err := packJoyueMasterCtor(salt, agentCreationCode, uint32(*shardID))
	if err != nil {
		log.Fatalf("ABI 编码 constructor args 失败: %v", err)
	}

	// 合约创建 tx.data = creationBytecode || ctorArgs
	data := append(common.CopyBytes(masterBin), ctorArgs...)

	// 计算一下 hash 方便你核对（合约里也会存这个 hash）
	agentHash := crypto.Keccak256Hash(agentCreationCode)
	log.Printf("from=%s chainID=%s nonce=%d", from.Hex(), chainID.String(), nonce)
	log.Printf("agentCreationCode len=%d hash=%s", len(agentCreationCode), agentHash.Hex())
	log.Printf("deploy tx.data len=%d", len(data))

	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		log.Fatalf("获取 header 失败: %v", err)
	}

	var tx *ethtypes.Transaction
	if head.BaseFee != nil {
		tipCap, tipErr := client.SuggestGasTipCap(ctx)
		if tipErr != nil || tipCap == nil {
			tipCap = new(big.Int).Mul(big.NewInt(*gasTipGwei), big.NewInt(1_000_000_000))
		}
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tipCap)
		tx = ethtypes.NewTx(&ethtypes.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       *gasLimit,
			To:        nil, // contract creation
			Value:     big.NewInt(0),
			Data:      data,
		})
	} else {
		gasPrice, err := client.SuggestGasPrice(ctx)
		if err != nil {
			log.Fatalf("获取 gasPrice 失败: %v", err)
		}
		tx = ethtypes.NewTx(&ethtypes.LegacyTx{
			Nonce:    nonce,
			GasPrice: gasPrice,
			Gas:      *gasLimit,
			To:       nil, // contract creation
			Value:    big.NewInt(0),
			Data:     data,
		})
	}

	signer := ethtypes.LatestSignerForChainID(chainID)
	signedTx, err := ethtypes.SignTx(tx, signer, privKey)
	if err != nil {
		log.Fatalf("签名失败: %v", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		log.Fatalf("发送交易失败: %v", err)
	}

	fmt.Printf("tx=%s\n", signedTx.Hash().Hex())

	if !*waitReceipt {
		return
	}

	ctx2, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	receipt, err := waitForReceipt(ctx2, client, signedTx.Hash())
	if err != nil {
		log.Fatalf("等待 receipt 失败: %v", err)
	}

	fmt.Printf("status=%d\n", receipt.Status)
	fmt.Printf("contract=%s\n", receipt.ContractAddress.Hex())
}

// packJoyueMasterCtor 使用 go-ethereum 的 ABI pack 编码 constructor(args)
func packJoyueMasterCtor(salt common.Hash, agentCreationCode []byte, masterShardID uint32) ([]byte, error) {
	tBytes32, err := abi.NewType("bytes32", "", nil)
	if err != nil {
		return nil, err
	}
	tBytes, err := abi.NewType("bytes", "", nil)
	if err != nil {
		return nil, err
	}
	tUint32, err := abi.NewType("uint32", "", nil)
	if err != nil {
		return nil, err
	}

	args := abi.Arguments{
		{Type: tBytes32},
		{Type: tBytes},
		{Type: tUint32},
	}
	return args.Pack(salt, agentCreationCode, masterShardID)
}

func parseBytes32(s string) (common.Hash, error) {
	b, err := decodeHexBytes(s)
	if err != nil {
		return common.Hash{}, err
	}
	if len(b) > 32 {
		return common.Hash{}, fmt.Errorf("bytes32 太长: %d", len(b))
	}
	var h common.Hash
	// 左侧补 0
	copy(h[32-len(b):], b)
	return h, nil
}

func readHexFromFileOrFlag(path string, hexStr string) ([]byte, error) {
	// 优先读文件
	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		hexStr = strings.TrimSpace(string(raw))
	}
	hexStr = strings.TrimSpace(hexStr)
	if hexStr == "" {
		return nil, errors.New("empty hex (need --*-bin-file or --*-bin)")
	}
	return decodeHexBytes(hexStr)
}

// readHexFromFileOrFlagOrEmbedded 优先级：flag(hex) > file > embedded const
func readHexFromFileOrFlagOrEmbedded(path string, hexStr string, embedded string, name string) ([]byte, error) {
	hexStr = strings.TrimSpace(hexStr)
	if hexStr != "" {
		return decodeHexBytes(hexStr)
	}
	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return decodeHexBytes(strings.TrimSpace(string(raw)))
	}
	if strings.TrimSpace(embedded) == "" {
		return nil, errors.New("缺少 " + name + "：请在 cmd/joyue-deploy-master/main.go 里填 embedded 常量，或用 --" + name + "-file / --" + name + " 传入")
	}
	return decodeHexBytes(embedded)
}

func decodeHexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

func waitForReceipt(ctx context.Context, client *ethclient.Client, txHash common.Hash) (*ethtypes.Receipt, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		receipt, err := client.TransactionReceipt(ctx, txHash)
		if err == nil && receipt != nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// 防止某些 go toolchain 报“未使用 import”误判
var _ *ecdsa.PrivateKey
