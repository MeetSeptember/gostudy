package genesis

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"fmt"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	internalcommon "github.com/harmony-one/harmony/internal/common"
)

// localnetJoyueRelayerAll 为 localnet Joyue Relayer 预充值地址（最多 16 分片）。
// 前两个与历史 2 分片配置一致；索引 2–15 由确定性种子派生 ECDSA，便于文档化私钥与 cmd/joyue-relayer 对齐。
var localnetJoyueRelayerAll []ethcommon.Address

func init() {
	buildLocalnetJoyueRelayerAll()
}

func buildLocalnetJoyueRelayerAll() {
	out := make([]ethcommon.Address, 0, 16)
	out = append(out,
		internalcommon.MustBech32ToAddress("one1lylsfclkm6q575dyg4ue47vdcyad6q0d4kge45"),
		internalcommon.MustBech32ToAddress("one1jq3ut362u970xzt9yqls7e2tq096wd6wkl4td4"),
	)
	curveN := crypto.S256().Params().N
	for i := 2; i < 16; i++ {
		pk := mustLocalnetJoyueRelayerECDSA(i, curveN)
		out = append(out, crypto.PubkeyToAddress(pk.PublicKey))
	}
	localnetJoyueRelayerAll = out
}

func mustLocalnetJoyueRelayerECDSA(shardIndex int, curveN *big.Int) *ecdsa.PrivateKey {
	for variant := 0; variant < 256; variant++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("HMY_JOYUE_RELAYER:%d:%d", shardIndex, variant)))
		d := new(big.Int).SetBytes(h[:])
		d.Mod(d, new(big.Int).Sub(curveN, big.NewInt(1)))
		d.Add(d, big.NewInt(1))
		bs := make([]byte, 32)
		db := d.Bytes()
		copy(bs[32-len(db):], db)
		pk, err := crypto.ToECDSA(bs)
		if err == nil {
			return pk
		}
	}
	panic(fmt.Sprintf("cannot derive localnet joyue relayer key for shard %d", shardIndex))
}

// LocalnetJoyueRelayerFundingAddresses 返回 genesis 中需预充值的 Joyue Relayer 地址（按分片下标 0..n-1）。
func LocalnetJoyueRelayerFundingAddresses(numShards int) []ethcommon.Address {
	if numShards < 1 {
		numShards = 2
	}
	if numShards > len(localnetJoyueRelayerAll) {
		numShards = len(localnetJoyueRelayerAll)
	}
	out := make([]ethcommon.Address, numShards)
	copy(out, localnetJoyueRelayerAll[:numShards])
	return out
}

// LocalnetJoyueRelayerPrivateKeyHex 返回分片 2..15 的确定性 Relayer 私钥（hex，无 0x），用于 cmd/joyue-relayer 等。
// 分片 0、1 沿用既有密钥文件，不在此函数中提供。
func LocalnetJoyueRelayerPrivateKeyHex(shardIndex int) (string, error) {
	if shardIndex < 2 || shardIndex > 15 {
		return "", fmt.Errorf("LocalnetJoyueRelayerPrivateKeyHex: shardIndex must be in [2,15], got %d", shardIndex)
	}
	pk := mustLocalnetJoyueRelayerECDSA(shardIndex, crypto.S256().Params().N)
	return fmt.Sprintf("%x", crypto.FromECDSA(pk)), nil
}
