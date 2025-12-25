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
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var sigRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\(([^)]*)\)(?:\(([^)]*)\))?$`)

/*
JOYUE 小工具：调用合约（读/写）

目标：让你不需要写脚本/SDK，也能快速：
- 读：eth_call（不发交易，不改状态）
- 写：发送一笔合约调用交易（eth_sendRawTransaction）

支持的输入方式：
1）--sig "funcName(type1,type2)(ret1,ret2)" + --arg 多个参数（推荐）
2）高级用户可直接 --data 传入完整 calldata（0x...），工具不再帮你编码

注意：
- 本仓库 go-ethereum replace 到 v1.11.2，因此尽量使用稳定 API。
- 参数解析做了“够用”的实现：支持 address/uint/int/bool/bytes/bytes32/string
*/

func main() {
	var (
		rpcURL = flag.String("rpc", "http://127.0.0.1:9500", "RPC URL（例如 shard0: http://127.0.0.1:9500）")
		toStr  = flag.String("to", "", "合约地址（0x...）")

		// 读/写共用：函数签名与参数
		sig  = flag.String("sig", "", `函数签名，如: "masterShardId()(uint32)" 或 "initialize(address,uint32,uint32)()"`)
		data = flag.String("data", "", "完整 calldata（0x...），传了就忽略 --sig/--arg")
		args multiStringFlag

		// 模式：默认 call；--send 表示发交易
		send = flag.Bool("send", false, "发送交易（写）；默认 false 表示 eth_call（读）")

		// 发送交易需要的私钥
		privateKeyHex = flag.String("private-key", "", "发送交易的私钥 hex（不带 0x；仅 send=true 时需要）")
		valueWeiStr   = flag.String("value", "0", "交易 value（wei，十进制字符串；仅 send=true 时使用）")

		gasLimit   = flag.Uint64("gas", 300000, "gasLimit（send=true 时使用；call 会尝试估算）")
		gasTipGwei = flag.Int64("gas-tip-gwei", 1, "EIP-1559 priority fee（gwei）")

		waitReceipt = flag.Bool("wait", false, "send=true 时等待 receipt 并打印 status/tx/block")
		timeout     = flag.Duration("timeout", 60*time.Second, "等待 receipt 超时")
	)
	flag.Var(&args, "arg", "参数（可多次指定）。例如: --arg 0xabc... --arg 1 --arg true")
	flag.Parse()

	if strings.TrimSpace(*toStr) == "" {
		flag.Usage()
		log.Fatal("缺少参数：--to")
	}
	to := common.HexToAddress(*toStr)

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		log.Fatalf("连接 RPC 失败: %v", err)
	}

	var calldata []byte
	if strings.TrimSpace(*data) != "" {
		calldata, err = decodeHexBytes(*data)
		if err != nil {
			log.Fatalf("data 解析失败: %v", err)
		}
	} else {
		if strings.TrimSpace(*sig) == "" {
			flag.Usage()
			log.Fatal("缺少参数：--sig 或 --data")
		}
		calldata, err = encodeCalldata(*sig, []string(args))
		if err != nil {
			log.Fatalf("编码 calldata 失败: %v", err)
		}
	}

	if !*send {
		// ---------- 读：eth_call ----------
		msg := ethereum.CallMsg{To: &to, Data: calldata}
		// 尝试估算 gas（非必须）
		if g, err := client.EstimateGas(ctx, msg); err == nil {
			msg.Gas = g
		}
		out, err := client.CallContract(ctx, msg, nil)
		if err != nil {
			log.Fatalf("eth_call 失败: %v", err)
		}
		fmt.Printf("to=%s\n", to.Hex())
		fmt.Printf("calldata=0x%s\n", hex.EncodeToString(calldata))
		fmt.Printf("ret=0x%s\n", hex.EncodeToString(out))

		// 如果 sig 里带了返回类型，则尝试解码并打印
		if strings.TrimSpace(*data) == "" {
			if _, _, outTypes, ok := parseSig(*sig); ok && len(outTypes) > 0 {
				decoded, err := decodeReturn(outTypes, out)
				if err != nil {
					fmt.Printf("decodeErr=%v\n", err)
				} else {
					fmt.Printf("decoded=%s\n", decoded)
				}
			}
		}
		return
	}

	// ---------- 写：发送交易 ----------
	if strings.TrimSpace(*privateKeyHex) == "" {
		flag.Usage()
		log.Fatal("send=true 时缺少参数：--private-key")
	}
	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(*privateKeyHex, "0x"))
	if err != nil {
		log.Fatalf("私钥格式错误: %v", err)
	}
	from := crypto.PubkeyToAddress(privKey.PublicKey)

	valueWei, ok := new(big.Int).SetString(strings.TrimSpace(*valueWeiStr), 10)
	if !ok {
		log.Fatalf("value 解析失败（需要十进制 wei）: %q", *valueWeiStr)
	}

	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Fatalf("获取 chainID 失败: %v", err)
	}
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		log.Fatalf("获取 nonce 失败: %v", err)
	}

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
			To:        &to,
			Value:     valueWei,
			Data:      calldata,
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
			To:       &to,
			Value:    valueWei,
			Data:     calldata,
		})
	}

	signer := ethtypes.LatestSignerForChainID(chainID)
	signedTx, err := ethtypes.SignTx(tx, signer, privKey)
	if err != nil {
		log.Fatalf("签名失败: %v", err)
	}
	if err := client.SendTransaction(ctx, signedTx); err != nil {
		log.Fatalf("发送失败: %v", err)
	}

	fmt.Printf("from=%s\n", from.Hex())
	fmt.Printf("to=%s\n", to.Hex())
	fmt.Printf("tx=%s\n", signedTx.Hash().Hex())
	fmt.Printf("calldata=0x%s\n", hex.EncodeToString(calldata))

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
	fmt.Printf("block=%d\n", receipt.BlockNumber.Uint64())
}

// multiStringFlag 支持重复 --arg
type multiStringFlag []string

func (m *multiStringFlag) String() string { return strings.Join(*m, ",") }
func (m *multiStringFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// encodeCalldata 根据 --sig 和 --arg 编码 calldata
func encodeCalldata(signature string, args []string) ([]byte, error) {
	name, inTypes, _, ok := parseSig(signature)
	if !ok {
		return nil, fmt.Errorf("sig 格式不对: %q", signature)
	}
	// 1) 计算 4-byte selector
	sel := crypto.Keccak256([]byte(fmt.Sprintf("%s(%s)", name, strings.Join(inTypes, ","))))[:4]

	// 2) 解析参数类型并 pack
	arguments, err := buildArguments(inTypes)
	if err != nil {
		return nil, err
	}
	values, err := parseArgs(inTypes, args)
	if err != nil {
		return nil, err
	}
	enc, err := arguments.Pack(values...)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+len(enc))
	out = append(out, sel...)
	out = append(out, enc...)
	return out, nil
}

// decodeReturn 根据输出类型解码返回值并转成字符串
func decodeReturn(outTypes []string, ret []byte) (string, error) {
	arguments, err := buildArguments(outTypes)
	if err != nil {
		return "", err
	}
	vals, err := arguments.Unpack(ret)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(vals))
	for i, v := range vals {
		parts = append(parts, fmt.Sprintf("%s=%v", outTypes[i], v))
	}
	return strings.Join(parts, ", "), nil
}

// parseSig 解析 "name(t1,t2)(r1,r2)"，返回 name、输入类型列表、输出类型列表
func parseSig(signature string) (string, []string, []string, bool) {
	m := sigRe.FindStringSubmatch(strings.TrimSpace(signature))
	if m == nil {
		return "", nil, nil, false
	}
	name := m[1]
	in := splitCSV(m[2])
	out := splitCSV(m[3])
	// 允许空返回：out 可能为空
	return name, in, out, true
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func buildArguments(types []string) (abi.Arguments, error) {
	args := make(abi.Arguments, 0, len(types))
	for _, t := range types {
		ty, err := abi.NewType(t, "", nil)
		if err != nil {
			return nil, err
		}
		args = append(args, abi.Argument{Type: ty})
	}
	return args, nil
}

// parseArgs 把字符串参数转成 abi.Pack 需要的 Go 值
func parseArgs(types []string, args []string) ([]interface{}, error) {
	if len(types) != len(args) {
		return nil, fmt.Errorf("参数数量不匹配：sig 需要 %d 个，实际给了 %d 个", len(types), len(args))
	}
	out := make([]interface{}, 0, len(args))
	for i := range types {
		v, err := parseOne(types[i], args[i])
		if err != nil {
			return nil, fmt.Errorf("arg[%d] 解析失败: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func parseOne(typ string, s string) (interface{}, error) {
	typ = strings.TrimSpace(typ)
	s = strings.TrimSpace(s)
	switch typ {
	case "address":
		return common.HexToAddress(s), nil
	case "bool":
		if s == "true" || s == "1" {
			return true, nil
		}
		if s == "false" || s == "0" {
			return false, nil
		}
		return nil, errors.New("bool 只能是 true/false/1/0")
	case "string":
		return s, nil
	case "bytes":
		return decodeHexBytes(s)
	}
	if strings.HasPrefix(typ, "bytes") && typ != "bytes" {
		// bytesN（如 bytes32）
		b, err := decodeHexBytes(s)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	if strings.HasPrefix(typ, "uint") || strings.HasPrefix(typ, "int") {
		// 统一用 big.Int 承接（abi pack 会按类型截断/检查）
		z := new(big.Int)
		// 允许 0x 前缀
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			b, err := decodeHexBytes(s)
			if err != nil {
				return nil, err
			}
			z.SetBytes(b)
			return z, nil
		}
		if _, ok := z.SetString(s, 10); !ok {
			return nil, errors.New("整数参数解析失败（需要十进制或 0x 十六进制）")
		}
		return z, nil
	}
	return nil, fmt.Errorf("暂不支持的类型：%s（你可以改用 --data 传完整 calldata）", typ)
}

func decodeHexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if s == "" {
		return nil, errors.New("empty hex")
	}
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

// 防止某些 toolchain 对未使用导入误判
var _ = os.Stdout
var _ *ecdsa.PrivateKey
