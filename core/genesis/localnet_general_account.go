package genesis

import (
	ethcommon "github.com/ethereum/go-ethereum/common"

	internalcommon "github.com/harmony-one/harmony/internal/common"
)

// localnetGeneralPurposeFundingAll 为 localnet 通用预充值地址（与分片数无关；与 Joyue relayer 列表独立）。
// 仅在此追加 one1… 地址即可，无需在仓库中配置私钥（与 localnet_joyue_relayers 中前若干条 bech32 的写法一致）。
var localnetGeneralPurposeFundingAll []ethcommon.Address

func init() {
	buildLocalnetGeneralPurposeFundingAll()
}

func buildLocalnetGeneralPurposeFundingAll() {
	out := make([]ethcommon.Address, 0, 8)
	out = append(out,
		internalcommon.MustBech32ToAddress("one1fy8ahmf8dzvp30eg0uk9zkq5xft009cyejaa3c"),
		// 在此追加更多通用预充值 one1… 地址。
	)
	localnetGeneralPurposeFundingAll = out
}

// LocalnetGeneralPurposeFundingAddresses 返回上述地址列表的副本，供 genesis alloc 使用。
func LocalnetGeneralPurposeFundingAddresses() []ethcommon.Address {
	out := make([]ethcommon.Address, len(localnetGeneralPurposeFundingAll))
	copy(out, localnetGeneralPurposeFundingAll)
	return out
}
