// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowAmmIntentShop
 * @dev 用户侧单笔 swap 意图：仅事件；链下 batcher 聚批后调 SparrowAmmCoordinator.swapWave。
 *      `swapIntentExplicit` 可传 `clientVersion` 钉版本；`0` 表示执行时由协调者以 Pool `quoteSwap` 返回的权威版本代填。
 */
contract SparrowAmmIntentShop {
    uint256 private _nonce;

    /// @param clientVersion 0 = 执行时采用池子 quote 返回的权威版本；非 0 须与当时 ammVersion 一致
    event SparrowAmmSwapIntent(
        address indexed user,
        uint256 amountIn,
        uint256 minOut,
        uint256 clientVersion,
        bytes32 intentId
    );

    function swapIntentExplicit(address user, uint256 amountIn, uint256 minOut, uint256 clientVersion) external {
        require(user != address(0), "SAmmIntent: user");
        require(amountIn > 0, "SAmmIntent: amountIn");
        _emitIntent(user, amountIn, minOut, clientVersion);
    }

    function _emitIntent(address user, uint256 amountIn, uint256 minOut, uint256 clientVersion) internal {
        unchecked {
            _nonce++;
        }
        bytes32 iid = keccak256(abi.encodePacked(user, amountIn, minOut, clientVersion, block.number, _nonce));
        emit SparrowAmmSwapIntent(user, amountIn, minOut, clientVersion, iid);
    }
}
