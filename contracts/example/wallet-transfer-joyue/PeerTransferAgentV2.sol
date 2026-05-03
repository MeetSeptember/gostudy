// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "./PeerWalletMasterV2.sol";

/**
 * @title PeerTransferAgentV2
 * @dev 单分片 JOYUE 转账：仅一份 `PeerWalletMasterV2`，from/to 余额均在同一 Master、同一分片缓存上校验与记账。
 *
 *      构造：`walletMaster` = 本链上的 `PeerWalletMasterV2` 地址，`shardId` = 当前分片 ID。
 *      `transfer(uint256)` 已弃用；请用 `transferExplicit(from,to,amount)` 或链下选地址对。
 */
contract PeerTransferAgentV2 {
    using JoyueLib for JoyueLib.JVar;
    using JoyueLib for JoyueLib.Context;

    address public immutable walletMaster;
    uint32 public immutable shardId;

    uint8 public constant REASON_CACHE_INSUFFICIENT = 6;
    uint8 public constant REASON_NOT_SIMULATED_USER = 7;

    event IntentSent(bytes32 indexed txId, address indexed from, address indexed to, uint256 amount);
    event IntentRejected(bytes32 indexed txId, uint8 reason);

    constructor(address walletMaster_, uint32 shardId_) {
        require(walletMaster_ != address(0), "JOYUE: zero master");
        walletMaster = walletMaster_;
        shardId = shardId_;
    }

    function simulatedUserCount() external pure returns (uint256) {
        return 0;
    }

    function simulatedUser(uint256) external pure returns (address) {
        return address(0);
    }

    function isSimulatedUser(address) external pure returns (bool) {
        return false;
    }

    function _balanceKey(address user) internal pure returns (bytes32) {
        return JoyueLib.keyOfAddr("wallet.balance:", user);
    }

    function transfer(uint256) external pure {
        revert("JOYUE: use transferExplicit");
    }

    /**
     * @notice 显式 from / to（须已在 Master 上 bootstrap 余额）。
     */
    function transferExplicit(address from, address to, uint256 amount)
        external
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        if (from == address(0) || to == address(0) || from == to) {
            txId = bytes32(0);
            emit IntentRejected(txId, REASON_NOT_SIMULATED_USER);
            return (false, txId, guards, deltas);
        }
        return _transferInner(from, to, amount);
    }

    function _transferInner(address from, address to, uint256 amount)
        private
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        JoyueLib.Context memory ctx = JoyueLib.newTransactionContext(
            address(0),
            walletMaster,
            PeerWalletMasterV2.transferIntent.selector,
            abi.encode(from, to, amount)
        );

        if (!_loadAndValidate(ctx, from, to, amount, new JoyueLib.StateOverride[](0))) {
            emit IntentRejected(ctx.txId, REASON_CACHE_INSUFFICIENT);
            return (false, ctx.txId, ctx.guards, ctx.deltas);
        }

        ctx.emitIntentViaPrecompile(shardId, walletMaster);
        JoyueLib.emitIntent(ctx);
        emit IntentSent(ctx.txId, from, to, amount);

        return (true, ctx.txId, ctx.guards, ctx.deltas);
    }

    function _loadAndValidate(
        JoyueLib.Context memory ctx,
        address from,
        address to,
        uint256 amount,
        JoyueLib.StateOverride[] memory overrides
    ) internal returns (bool) {
        if (amount == 0) {
            return false;
        }

        JoyueLib.JVar memory fromBal =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, walletMaster, shardId, _balanceKey(from), overrides);
        JoyueLib.JVar memory toBal =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, walletMaster, shardId, _balanceKey(to), overrides);

        if (!fromBal.checkGte(ctx, amount)) {
            return false;
        }

        uint256 toCur = JoyueLib.toUint(toBal.value);
        if (toCur > type(uint256).max - amount) {
            return false;
        }

        fromBal.sub(ctx, amount);
        toBal.add(ctx, amount);
        return true;
    }

    function recomputeIntent(
        JoyueLib.RawRequest calldata req,
        address,
        JoyueLib.StateOverride[] calldata stateOverrides
    ) external returns (JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas, bool ok) {
        if (req.selector != PeerWalletMasterV2.transferIntent.selector) {
            return (guards, deltas, false);
        }
        if (req.targetAddr != walletMaster) {
            return (guards, deltas, false);
        }
        (address from, address to, uint256 amount) = abi.decode(req.args, (address, address, uint256));
        if (from == address(0) || to == address(0) || from == to) {
            return (guards, deltas, false);
        }

        JoyueLib.Context memory ctx = JoyueLib.newContext(
            address(0),
            walletMaster,
            req.selector,
            req.args
        );
        ok = _loadAndValidate(ctx, from, to, amount, stateOverrides);
        return (ctx.guards, ctx.deltas, ok);
    }
}
