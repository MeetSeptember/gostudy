// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowNftIntentShop
 * @dev 与 `SparrowAmmIntentShop` 对称：显式买家 `buyNftIntentExplicit`；发 `SparrowNftIntent`，**sparrow-nft-batcher** 聚批调 `SparrowNftCoordinator.buyNftWave`。
 *      `intentId` 即 metrics / Coordinator `NftItem.txId`；买家与余额由链下 CSV + `bootstrap-wallet-balances-from-csv -kind nft-sparrow-wallet` 对齐。
 */
contract SparrowNftIntentShop {
    mapping(address => uint256) public intentSeq;

    event SparrowNftIntent(address indexed buyer, uint256 quantity, bytes32 intentId);

    function buyNftIntentExplicit(address buyer, uint256 quantity) external {
        require(buyer != address(0), "SNftIntent: buyer");
        require(quantity > 0, "SNftIntent: qty");
        uint256 seq = ++intentSeq[buyer];
        bytes32 iid = keccak256(abi.encodePacked(buyer, quantity, block.number, seq));
        emit SparrowNftIntent(buyer, quantity, iid);
    }
}
