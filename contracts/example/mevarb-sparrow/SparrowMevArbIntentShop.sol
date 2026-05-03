// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowMevArbIntentShop
 * @dev 单笔 MEV 套利意图（仅事件）；链下 `sparrow-mev-batcher` 聚批后调 `SparrowMevArbCoordinator.arbWave`。
 *      `intentId` 与 `ArbItem.txId` 对齐，供 joyue-trigger / sparrow-metrics 关联。
 *      `mevIntentRandom`：user 固定为 `msg.sender`（便于压测无白名单）；显式路径用 `mevIntentExplicit`。
 */
contract SparrowMevArbIntentShop {
    uint256 private _nonce;

    event SparrowMevArbIntent(address indexed user, uint256 borrowA, uint256 minNetProfitA, bytes32 intentId);

    function mevIntentRandom(uint256 borrowA, uint256 minNetProfitA) external {
        require(msg.sender != address(0), "SMevIntent: zero");
        require(borrowA > 0, "SMevIntent: borrow");
        _emitIntent(msg.sender, borrowA, minNetProfitA);
    }

    function mevIntentExplicit(address user, uint256 borrowA, uint256 minNetProfitA) external {
        require(user != address(0), "SMevIntent: user");
        require(borrowA > 0, "SMevIntent: borrow");
        _emitIntent(user, borrowA, minNetProfitA);
    }

    function _emitIntent(address user, uint256 borrowA, uint256 minNetProfitA) internal {
        unchecked {
            _nonce++;
        }
        bytes32 iid = keccak256(abi.encodePacked(user, borrowA, minNetProfitA, block.number, _nonce, msg.sender));
        emit SparrowMevArbIntent(user, borrowA, minNetProfitA, iid);
    }
}
