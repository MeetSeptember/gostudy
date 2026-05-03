// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "./NftJoyueSalesMasterV2.sol";

/**
 * @title NftJoyueShopAgentV2
 * @dev JOYUE 简化购「份」：递减 `nft.joyue.remainingSupply`，从 `nft.joyue.wallet.balance:` 扣 `unitPrice * quantity`。
 *      无 tokenId、无 per-user NFT 计数（方案 B）。买家由 `buyExplicit` 显式传入，与链下 CSV / bootstrap 对齐。
 */
contract NftJoyueShopAgentV2 {
    using JoyueLib for JoyueLib.JVar;
    using JoyueLib for JoyueLib.Context;

    bytes32 private constant SUPPLY_KEY = keccak256(abi.encodePacked("nft.joyue.remainingSupply"));
    bytes32 private constant PRICE_KEY = keccak256(abi.encodePacked("nft.joyue.unitPrice"));

    address public immutable nftJoyueSalesMaster;
    address public immutable nftJoyueWalletMaster;
    uint32 public immutable shardSales;
    uint32 public immutable shardWallet;

    uint8 public constant REASON_CACHE_INSUFFICIENT = 6;

    event IntentSent(bytes32 indexed txId, address indexed buyer, uint256 quantity);
    event IntentRejected(bytes32 indexed txId, uint8 reason);

    constructor(address sales_, address wallet_, uint32 shardSales_, uint32 shardWallet_) {
        require(sales_ != address(0) && wallet_ != address(0), "NftJoyue: zero master");
        nftJoyueSalesMaster = sales_;
        nftJoyueWalletMaster = wallet_;
        shardSales = shardSales_;
        shardWallet = shardWallet_;
    }

    function _balanceKey(address user) private pure returns (bytes32) {
        return JoyueLib.keyOfAddr("nft.joyue.wallet.balance:", user);
    }

    function buyExplicit(address buyer_, uint256 quantity)
        external
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        require(buyer_ != address(0), "NftJoyue: zero buyer");
        return _buyInner(buyer_, quantity, new JoyueLib.StateOverride[](0));
    }

    function _buyInner(
        address buyer_,
        uint256 quantity,
        JoyueLib.StateOverride[] memory overrides
    )
        private
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        JoyueLib.Context memory ctx = JoyueLib.newTransactionContext(
            address(0),
            nftJoyueSalesMaster,
            NftJoyueSalesMasterV2.buyIntent.selector,
            abi.encode(buyer_, quantity)
        );

        if (!_loadAndValidate(ctx, buyer_, quantity, overrides)) {
            emit IntentRejected(ctx.txId, REASON_CACHE_INSUFFICIENT);
            return (false, ctx.txId, ctx.guards, ctx.deltas);
        }

        ctx.emitIntentViaPrecompile(shardSales, nftJoyueSalesMaster);
        JoyueLib.emitIntent(ctx);
        emit IntentSent(ctx.txId, buyer_, quantity);

        return (true, ctx.txId, ctx.guards, ctx.deltas);
    }

    function _loadAndValidate(
        JoyueLib.Context memory ctx,
        address buyer_,
        uint256 quantity,
        JoyueLib.StateOverride[] memory overrides
    ) private returns (bool) {
        if (quantity == 0) {
            return false;
        }

        JoyueLib.JVar memory supply =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, nftJoyueSalesMaster, shardSales, SUPPLY_KEY, overrides);
        JoyueLib.JVar memory priceVar =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, nftJoyueSalesMaster, shardSales, PRICE_KEY, overrides);
        uint256 unitPrice = JoyueLib.toUint(priceVar.value);
        if (unitPrice == 0) {
            return false;
        }
        if (quantity > type(uint256).max / unitPrice) {
            return false;
        }
        uint256 cost = unitPrice * quantity;

        JoyueLib.JVar memory bal =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, nftJoyueWalletMaster, shardWallet, _balanceKey(buyer_), overrides);

        if (!supply.checkGte(ctx, quantity)) {
            return false;
        }
        if (!bal.checkGte(ctx, cost)) {
            return false;
        }

        supply.sub(ctx, quantity);
        bal.sub(ctx, cost);
        return true;
    }

    function recomputeIntent(
        JoyueLib.RawRequest calldata req,
        address buyer_,
        JoyueLib.StateOverride[] calldata stateOverrides
    ) external returns (JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas, bool ok) {
        if (req.selector != NftJoyueSalesMasterV2.buyIntent.selector) {
            return (guards, deltas, false);
        }
        if (req.targetAddr != nftJoyueSalesMaster) {
            return (guards, deltas, false);
        }
        (address decodedBuyer, uint256 quantity) = abi.decode(req.args, (address, uint256));
        if (decodedBuyer != buyer_ || buyer_ == address(0)) {
            return (guards, deltas, false);
        }

        JoyueLib.StateOverride[] memory ov = new JoyueLib.StateOverride[](stateOverrides.length);
        for (uint256 i = 0; i < stateOverrides.length; i++) {
            ov[i] = stateOverrides[i];
        }

        JoyueLib.Context memory ctx =
            JoyueLib.newContext(address(0), nftJoyueSalesMaster, req.selector, req.args);
        ok = _loadAndValidate(ctx, buyer_, quantity, ov);
        return (ctx.guards, ctx.deltas, ok);
    }
}
