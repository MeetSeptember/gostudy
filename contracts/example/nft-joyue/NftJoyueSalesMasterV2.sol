// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueCoordinatorV2.sol";

/**
 * @title NftJoyueSalesMasterV2
 * @dev JOYUE 简化 NFT（方案 B）：仅全局剩余数量 + 单价，无 tokenId、无按用户持有计数。
 *      状态 key 前缀统一 `nft.joyue.*`（与 Agent 内常量一致）。
 */
contract NftJoyueSalesMasterV2 is JoyueCoordinatorV2 {
    bytes32 public constant NFT_JOYUE_SUPPLY_KEY = keccak256(abi.encodePacked("nft.joyue.remainingSupply"));
    bytes32 public constant NFT_JOYUE_PRICE_KEY = keccak256(abi.encodePacked("nft.joyue.unitPrice"));

    uint256 public constant INITIAL_REMAINING_SUPPLY = 500;
    uint256 public constant INITIAL_UNIT_PRICE = 100;

    constructor() {
        _setUint(NFT_JOYUE_SUPPLY_KEY, INITIAL_REMAINING_SUPPLY);
        _broadcastState(NFT_JOYUE_SUPPLY_KEY);
        _setUint(NFT_JOYUE_PRICE_KEY, INITIAL_UNIT_PRICE);
        _broadcastState(NFT_JOYUE_PRICE_KEY);
    }

    /**
     * @dev RawRequest 占位：由 NftJoyueShopAgentV2 生成 Guard/Delta。
     */
    function buyIntent(address buyer, uint256 quantity) external pure returns (address, uint256) {
        return (buyer, quantity);
    }

    function setRemainingSupply(uint256 v) external {
        _setUint(NFT_JOYUE_SUPPLY_KEY, v);
        _broadcastState(NFT_JOYUE_SUPPLY_KEY);
    }

    function setUnitPrice(uint256 v) external {
        _setUint(NFT_JOYUE_PRICE_KEY, v);
        _broadcastState(NFT_JOYUE_PRICE_KEY);
    }
}
