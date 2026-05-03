package main

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/harmony-one/harmony/internal/joyuetrigger"
)

// scenario 描述一种「CSV 行 → calldata + receipt 中 tx_id 语义」；含 wallet-mevarb-sparrow、wallet-mevarb-chainspace、wallet-mevarb-2pc 等。
type scenario struct {
	name   string
	is2PC  bool // 传给 joyuetrigger.ParseTxIDFromReceipt；与 joyue-trigger 的 Job.Is2PC 一致
	encode func(pair addressPair, amount *big.Int) ([]byte, error)
}

// clientVersion 仅 wallet-amm-sparrow / amm-sparrow 使用；其它场景可传任意非 nil 值（通常由 main 统一解析 -client-version）。
func scenarioByFlag(name string, minAmountOut *big.Int, clientVersion *big.Int) (scenario, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "", "wallet-2pc":
		return scenarioWallet2PC(), nil
	case "wallet-chainspace", "chainspace-wallet":
		return scenarioWalletChainspace(), nil
	case "wallet-sparrow", "sparrow-wallet":
		return scenarioWalletSparrow(), nil
	case "wallet-joyue", "joyue-wallet":
		return scenarioWalletJoyue(), nil
	case "amm-2pc", "wallet-amm-2pc":
		if minAmountOut == nil || minAmountOut.Sign() <= 0 {
			return scenario{}, fmt.Errorf("amm-2pc 需要 -min-amount-out 为大于 0 的十进制整数")
		}
		return scenarioAmm2PC(minAmountOut), nil
	case "wallet-amm-chainspace", "amm-chainspace":
		if minAmountOut == nil || minAmountOut.Sign() <= 0 {
			return scenario{}, fmt.Errorf("wallet-amm-chainspace 需要 -min-amount-out 为大于 0 的十进制整数")
		}
		return scenarioWalletAmmChainspace(minAmountOut), nil
	case "wallet-amm-sparrow", "amm-sparrow":
		if minAmountOut == nil || minAmountOut.Sign() <= 0 {
			return scenario{}, fmt.Errorf("wallet-amm-sparrow 需要 -min-amount-out 为大于 0 的十进制整数")
		}
		if clientVersion == nil {
			return scenario{}, fmt.Errorf("wallet-amm-sparrow: internal clientVersion nil")
		}
		return scenarioWalletAmmSparrow(minAmountOut, clientVersion), nil
	case "wallet-amm-joyue", "amm-joyue":
		if minAmountOut == nil || minAmountOut.Sign() <= 0 {
			return scenario{}, fmt.Errorf("wallet-amm-joyue 需要 -min-amount-out 为大于 0 的十进制整数")
		}
		return scenarioWalletAmmJoyue(minAmountOut), nil
	case "nft-2pc", "wallet-nft-2pc":
		return scenarioNft2PC(), nil
	case "nft-chainspace", "wallet-nft-chainspace":
		return scenarioWalletNftChainspace(), nil
	case "wallet-nft-sparrow", "nft-sparrow":
		return scenarioWalletNftSparrow(), nil
	case "wallet-nft-joyue", "nft-joyue":
		return scenarioWalletNftJoyue(), nil
	case "wallet-mevarb-2pc", "mevarb-2pc":
		if minAmountOut == nil || minAmountOut.Sign() < 0 {
			return scenario{}, fmt.Errorf("wallet-mevarb-2pc 需要 -min-amount-out 为 >= 0 的十进制整数（最小净利 minNetProfitA）")
		}
		return scenarioWalletMevArb2PC(minAmountOut), nil
	case "wallet-mevarb-chainspace", "mevarb-chainspace":
		if minAmountOut == nil || minAmountOut.Sign() < 0 {
			return scenario{}, fmt.Errorf("wallet-mevarb-chainspace 需要 -min-amount-out 为 >= 0 的十进制整数（minProfitA）")
		}
		return scenarioWalletMevArbChainspace(minAmountOut), nil
	case "wallet-mevarb-sparrow", "mevarb-sparrow":
		if minAmountOut == nil || minAmountOut.Sign() < 0 {
			return scenario{}, fmt.Errorf("wallet-mevarb-sparrow 需要 -min-amount-out 为 >= 0 的十进制整数（minNetProfitA）")
		}
		return scenarioWalletMevArbSparrow(minAmountOut), nil
	case "wallet-mevarb-joyue", "mevarb-joyue":
		if minAmountOut == nil || minAmountOut.Sign() < 0 {
			return scenario{}, fmt.Errorf("wallet-mevarb-joyue 需要 -min-amount-out 为 >= 0 的十进制整数（minNetProfitA）")
		}
		return scenarioWalletMevArbJoyue(minAmountOut), nil
	default:
		return scenario{}, fmt.Errorf("未知 -scenario %q（支持 wallet-2pc、wallet-chainspace、wallet-sparrow、wallet-joyue、amm-2pc、wallet-amm-chainspace、wallet-amm-sparrow、wallet-amm-joyue、nft-2pc、nft-chainspace、wallet-nft-sparrow / nft-sparrow、wallet-nft-joyue / nft-joyue、wallet-mevarb-2pc / mevarb-2pc、wallet-mevarb-chainspace / mevarb-chainspace、wallet-mevarb-sparrow / mevarb-sparrow、wallet-mevarb-joyue / mevarb-joyue）", name)
	}
}

func scenarioWallet2PC() scenario {
	return scenario{
		name:  "wallet-2pc",
		is2PC: true,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), pair.B.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("startTransfer(address,address,uint256)", args)
		},
	}
}

// scenarioWalletChainspace 调用 ChainspaceUserClient.transferLine(from,to,amount)；收据按 ChainspaceIntentStarted 解析 intentId（is2PC=false）。
func scenarioWalletChainspace() scenario {
	return scenario{
		name:  "wallet-chainspace",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), pair.B.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("transferLine(address,address,uint256)", args)
		},
	}
}

// scenarioWalletSparrow 调用 SparrowTransferIntentShop.transferIntentExplicit(from,to,amount)；收据按 SparrowTransferIntent 解析 intentId（is2PC=false）。
func scenarioWalletSparrow() scenario {
	return scenario{
		name:  "wallet-sparrow",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), pair.B.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("transferIntentExplicit(address,address,uint256)", args)
		},
	}
}

// scenarioWalletJoyue 调用 PeerTransferAgentV2.transferExplicit(from,to,amount)；收据按 IntentSent(bytes32,address,address,uint256) 解析 tx_id（is2PC=false）。
func scenarioWalletJoyue() scenario {
	return scenario{
		name:  "wallet-joyue",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), pair.B.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("transferExplicit(address,address,uint256)", args)
		},
	}
}

// scenarioAmm2PC 调用 PeerAmmSwapCoordinator2PC.startSwap(user, amountIn, minAmountOut)；收据按 PeerAmmSwap2PCStarted 解析 tx_id（is2PC=true）。
func scenarioAmm2PC(minAmountOut *big.Int) scenario {
	minCopy := new(big.Int).Set(minAmountOut)
	return scenario{
		name:  "amm-2pc",
		is2PC: true,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String()}
			return joyuetrigger.EncodeCalldata("startSwap(address,uint256,uint256)", args)
		},
	}
}

// scenarioWalletAmmChainspace 调用 AmmChainspaceUserClient.startSwap(user, amountIn, minOut)；收据按 AmmChainspaceIntentStarted 解析 tx_id（is2PC=false）。
func scenarioWalletAmmChainspace(minAmountOut *big.Int) scenario {
	minCopy := new(big.Int).Set(minAmountOut)
	return scenario{
		name:  "wallet-amm-chainspace",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String()}
			return joyuetrigger.EncodeCalldata("startSwap(address,uint256,uint256)", args)
		},
	}
}

// scenarioWalletAmmSparrow 调用 SparrowAmmIntentShop.swapIntentExplicit(user, amountIn, minOut, clientVersion)；收据按 SparrowAmmSwapIntent 解析 intentId（is2PC=false）。
func scenarioWalletAmmSparrow(minAmountOut *big.Int, clientVersion *big.Int) scenario {
	minCopy := new(big.Int).Set(minAmountOut)
	cvCopy := new(big.Int).Set(clientVersion)
	return scenario{
		name:  "wallet-amm-sparrow",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String(), cvCopy.String()}
			return joyuetrigger.EncodeCalldata("swapIntentExplicit(address,uint256,uint256,uint256)", args)
		},
	}
}

// scenarioWalletAmmJoyue 调用 AmmSwapAgentV2.swapExplicit(user, amountIn, minAmountOut)；收据按 IntentSent(bytes32,address,uint256,uint256,uint256) 解析 tx_id（is2PC=false）。
func scenarioWalletAmmJoyue(minAmountOut *big.Int) scenario {
	minCopy := new(big.Int).Set(minAmountOut)
	return scenario{
		name:  "wallet-amm-joyue",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String()}
			return joyuetrigger.EncodeCalldata("swapExplicit(address,uint256,uint256)", args)
		},
	}
}

// scenarioNft2PC 调用 PeerNftPurchaseCoordinator2PC.startPurchase(buyer, quantity)；收据按 PeerNftPurchase2PCStarted 解析 tx_id（is2PC=true）；-amount 为每笔 quantity。
func scenarioNft2PC() scenario {
	return scenario{
		name:  "nft-2pc",
		is2PC: true,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("startPurchase(address,uint256)", args)
		},
	}
}

// scenarioWalletNftChainspace 调用 NftPurchaseChainspaceUserClient.startPurchase(buyer, quantity)；收据按 NftChainspaceIntentStarted 解析 tx_id（is2PC=false）；-amount 为每笔 quantity；须 bootstrap -kind nft-chainspace-payment 对 NFTPaymentSimulator 灌 note。
func scenarioWalletNftChainspace() scenario {
	return scenario{
		name:  "nft-chainspace",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("startPurchase(address,uint256)", args)
		},
	}
}

// scenarioWalletNftSparrow 调用 SparrowNftIntentShop.buyNftIntentExplicit(buyer, quantity)；收据按 SparrowNftIntent 解析 intentId（is2PC=false）；-amount 为每笔 quantity；须 bootstrap -kind nft-sparrow-wallet 与 sparrow-nft-batcher。
func scenarioWalletNftSparrow() scenario {
	return scenario{
		name:  "wallet-nft-sparrow",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("buyNftIntentExplicit(address,uint256)", args)
		},
	}
}

// scenarioWalletNftJoyue 调用 NftJoyueShopAgentV2.buyExplicit(buyer, quantity)；收据按 IntentSent(bytes32,address,uint256) 解析 tx_id（is2PC=false）；-amount 为每笔 quantity；须 bootstrap -kind nft-joyue-wallet；多分片与 wallet-joyue 相同使用 -joyue-rpcs/-joyue-agents。
func scenarioWalletNftJoyue() scenario {
	return scenario{
		name:  "wallet-nft-joyue",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String()}
			return joyuetrigger.EncodeCalldata("buyExplicit(address,uint256)", args)
		},
	}
}

// scenarioWalletMevArb2PC 调用 PeerMevArbCoordinator2PC.startArb(user, borrowA, minNetProfitA)；收据按 PeerMevArb2PCStarted 解析 tx_id（is2PC=true）；-amount=borrowA；-min-amount-out=minNetProfitA（可为 0）。
func scenarioWalletMevArb2PC(minNetProfitA *big.Int) scenario {
	minCopy := new(big.Int).Set(minNetProfitA)
	return scenario{
		name:  "wallet-mevarb-2pc",
		is2PC: true,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String()}
			return joyuetrigger.EncodeCalldata("startArb(address,uint256,uint256)", args)
		},
	}
}

// scenarioWalletMevArbChainspace 调用 MevBotChainspaceUserClient.startFlashArb(user, borrowAmount, minProfitA)；收据按 MevBotChainspaceIntentStarted 解析 tx_id（is2PC=false）；-amount=borrowAmount；-min-amount-out=minProfitA（可为 0）；须 bootstrap -kind mevarb-chainspace-wallet。
func scenarioWalletMevArbChainspace(minProfitA *big.Int) scenario {
	minCopy := new(big.Int).Set(minProfitA)
	return scenario{
		name:  "wallet-mevarb-chainspace",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String()}
			return joyuetrigger.EncodeCalldata("startFlashArb(address,uint256,uint256)", args)
		},
	}
}

// scenarioWalletMevArbSparrow 调用 SparrowMevArbIntentShop.mevIntentExplicit(user, borrowA, minNetProfitA)；收据按 SparrowMevArbIntent 解析 intentId（is2PC=false）；须 sparrow-mev-batcher 与 bootstrap -kind mevarb-sparrow-profit-wallet（可选）。
func scenarioWalletMevArbSparrow(minNetProfitA *big.Int) scenario {
	minCopy := new(big.Int).Set(minNetProfitA)
	return scenario{
		name:  "wallet-mevarb-sparrow",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String()}
			return joyuetrigger.EncodeCalldata("mevIntentExplicit(address,uint256,uint256)", args)
		},
	}
}

// scenarioWalletMevArbJoyue 调用 MevArbBotAgentV2.arbExplicit(user, borrowA, minNetProfitA)；收据按 IntentSent(bytes32,address,uint256,uint256,uint256) 解析 tx_id（is2PC=false）；可选 bootstrap -kind mevarb-joyue-profit-wallet；多分片与 wallet-amm-joyue 相同使用 -joyue-rpcs/-joyue-agents。
func scenarioWalletMevArbJoyue(minNetProfitA *big.Int) scenario {
	minCopy := new(big.Int).Set(minNetProfitA)
	return scenario{
		name:  "wallet-mevarb-joyue",
		is2PC: false,
		encode: func(pair addressPair, amount *big.Int) ([]byte, error) {
			args := []string{pair.A.Hex(), amount.String(), minCopy.String()}
			return joyuetrigger.EncodeCalldata("arbExplicit(address,uint256,uint256)", args)
		},
	}
}
