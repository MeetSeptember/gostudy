// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

interface ISparrowTransferWalletPrecheck {
    function precheckLine(address from, address to, uint256 amount) external view returns (bool);
}

/**
 * @title SparrowTransferIntentShop
 * @dev 发事件前调 Wallet `precheckLine`（返回 bool，不检查锁）；`precheckOk` 一并写入事件供 Coordinator / batcher 使用。
 *      随机 `transferIntentRandom` 已弃用；请用 `transferIntentExplicit` 或链下选地址对。
 */
contract SparrowTransferIntentShop {
    ISparrowTransferWalletPrecheck public immutable wallet;

    uint256 private _nonce;

    event SparrowTransferIntent(
        address indexed from,
        address indexed to,
        uint256 amount,
        bytes32 intentId,
        bool precheckOk
    );

    constructor(address wallet_) {
        require(wallet_ != address(0), "STIntent: zero wallet");
        wallet = ISparrowTransferWalletPrecheck(wallet_);
    }

    function transferIntentRandom(uint256) external pure {
        revert("STIntent: use transferIntentExplicit");
    }

    function transferIntentExplicit(address from, address to, uint256 amount) external {
        require(from != address(0) && to != address(0), "STIntent: zero");
        require(from != to, "STIntent: self");
        require(amount > 0, "STIntent: amount");
        _emitIntent(from, to, amount);
    }

    function _emitIntent(address from, address to, uint256 amount) internal {
        bool ok = wallet.precheckLine(from, to, amount);
        unchecked {
            _nonce++;
        }
        bytes32 iid = keccak256(abi.encodePacked(from, to, amount, block.number, _nonce));
        emit SparrowTransferIntent(from, to, amount, iid, ok);
    }
}
