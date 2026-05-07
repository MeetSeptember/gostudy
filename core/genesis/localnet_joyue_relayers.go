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
// 前 7 条为固定 bech32（与历史 2 分片前两条一致并扩展）；其后为 HMY_JOYUE_RELAYER 确定性 ECDSA（i=2..15），与 cmd/joyue-relayer 私钥派生一致。
var localnetJoyueRelayerAll []ethcommon.Address

func init() {
	buildLocalnetJoyueRelayerAll()
}

func buildLocalnetJoyueRelayerAll() {
	out := make([]ethcommon.Address, 0, 16)
	out = append(out,
		internalcommon.MustBech32ToAddress("one1lylsfclkm6q575dyg4ue47vdcyad6q0d4kge45"),
		internalcommon.MustBech32ToAddress("one1jq3ut362u970xzt9yqls7e2tq096wd6wkl4td4"),
		internalcommon.MustBech32ToAddress("one1ddvsh3xpj6zkjfveazw5wpnfw4wynar24pxx4k"),
		internalcommon.MustBech32ToAddress("one14djrp3nrh2pg5gksvm6jcuh299teu9xf3fhwel"),
		internalcommon.MustBech32ToAddress("one1lg9mv8fsupjga5x9d2h4v58pldmxjyzschqg2p"),
		internalcommon.MustBech32ToAddress("one1gxx4vd429jle8acff2hraud4hyf508r3skhjua"),
		internalcommon.MustBech32ToAddress("one13pz6hwkntv5pzml5fnxgpytd4tajjps2n5lq39"),
		internalcommon.MustBech32ToAddress("one1fy8ahmf8dzvp30eg0uk9zkq5xft009cyejaa3c"),
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
