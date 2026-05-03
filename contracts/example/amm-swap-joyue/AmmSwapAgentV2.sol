// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "./AmmPoolMasterV2.sol";

/**
 * @title AmmSwapAgentV2
 * @dev 简化 AMM（JOYUE）：用户用 WalletA 的代币 A 换池子中的 B，到账 WalletB。
 *      恒定乘积 amountOut = reserveB * amountIn / (reserveA + amountIn) 在 Agent 中计算；
 *      池储备 Guard 使用 checkEq(当前值) 生成 STRICT 版本语义；Master 仅持状态与协调。
 *
 *      部署：poolMaster / walletA / walletB 可为不同分片；构造参数传入对应 shardId。
 *
 *      池储备版本锁（仅 STRICT 储备）：仅用户首进 swapExplicit。发 Intent 前若当前缓存
 *      reserveA/reserveB 版本对与锁内一致则拒绝；释放仅在**下一次用户首进**时通过「版本已变」或
 *      超过 TTL 判定。recomputeIntent 与锁无关（不占锁、不解锁、不检查锁）。
 */
contract AmmSwapAgentV2 {
    using JoyueLib for JoyueLib.JVar;
    using JoyueLib for JoyueLib.Context;

    /// @dev 必须与 AmmPoolMasterV2 中储备 key 定义一致
    bytes32 private constant RESERVE_A_KEY = keccak256(abi.encodePacked("amm.joyue.pool.reserveA"));
    bytes32 private constant RESERVE_B_KEY = keccak256(abi.encodePacked("amm.joyue.pool.reserveB"));

    address public immutable poolMaster;
    address public immutable walletMasterA;
    address public immutable walletMasterB;
    uint32 public immutable shardPool;
    uint32 public immutable shardA;
    uint32 public immutable shardB;

    uint8 public constant REASON_CACHE_INSUFFICIENT = 6;
    /// @dev 池储备版本仍被上一笔用户首进占用；解锁仅待版本更新或 TTL（见 _swapInner 开头）
    uint8 public constant REASON_RESERVE_VERSION_LOCKED = 8;

    /// @dev Intent 未落地导致版本长期不变时，超过该区块数后强制释放储备锁（与「版本更新自动释放」互补）
    uint256 public constant RESERVE_LOCK_TTL_BLOCKS = 256;

    bool private _reserveLockActive;
    uint64 private _lockedVerA;
    uint64 private _lockedVerB;
    uint256 private _lockDeadlineBlock;
    event IntentSent(bytes32 indexed txId, address indexed user, uint256 amountIn, uint256 amountOut, uint256 minAmountOut);
    event IntentRejected(bytes32 indexed txId, uint8 reason);

    constructor(address pool_, address wA_, address wB_, uint32 shardPool_, uint32 shardA_, uint32 shardB_) {
        require(pool_ != address(0) && wA_ != address(0) && wB_ != address(0), "AMM: zero master");
        poolMaster = pool_;
        walletMasterA = wA_;
        walletMasterB = wB_;
        shardPool = shardPool_;
        shardA = shardA_;
        shardB = shardB_;
    }

    function _balanceKeyA(address user) private pure returns (bytes32) {
        return JoyueLib.keyOfAddr("amm.joyue.walletA.balance:", user);
    }

    function _balanceKeyB(address user) private pure returns (bytes32) {
        return JoyueLib.keyOfAddr("amm.joyue.walletB.balance:", user);
    }

    function swapExplicit(address user, uint256 amountIn, uint256 minAmountOut)
        external
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        require(user != address(0), "AMM: user");
        return _swapInner(user, amountIn, minAmountOut, new JoyueLib.StateOverride[](0));
    }

    /// @dev 仅读池储备缓存版本（用于锁的占用/释放判断，与用户余额无关）
    function _peekReserveVersions() private view returns (uint64 verA, uint64 verB) {
        JoyueLib.Context memory peekCtx = JoyueLib.newContext(address(0), poolMaster, bytes4(0), bytes(""));
        JoyueLib.JVar memory rA = JoyueLib.loadFromCacheByKey(peekCtx, poolMaster, shardPool, RESERVE_A_KEY);
        JoyueLib.JVar memory rB = JoyueLib.loadFromCacheByKey(peekCtx, poolMaster, shardPool, RESERVE_B_KEY);
        return (rA.version, rB.version);
    }

    /// @dev 仅在用户首进 `_swapInner` 开头调用：版本对与锁内不同 → 释放；或超过 TTL 释放
    function _maybeReleaseReserveLock(uint64 curA, uint64 curB) private {
        if (!_reserveLockActive) {
            return;
        }
        if (block.number > _lockDeadlineBlock) {
            _reserveLockActive = false;
            return;
        }
        if (curA != _lockedVerA || curB != _lockedVerB) {
            _reserveLockActive = false;
        }
    }

    function _acquireReserveLock(uint64 verA, uint64 verB) private {
        _reserveLockActive = true;
        _lockedVerA = verA;
        _lockedVerB = verB;
        unchecked {
            _lockDeadlineBlock = block.number + RESERVE_LOCK_TTL_BLOCKS;
        }
    }

    function _swapInner(
        address user,
        uint256 amountIn,
        uint256 minAmountOut,
        JoyueLib.StateOverride[] memory overrides
    )
        private
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        {
            (uint64 peekA, uint64 peekB) = _peekReserveVersions();
            _maybeReleaseReserveLock(peekA, peekB);
            if (_reserveLockActive && peekA == _lockedVerA && peekB == _lockedVerB) {
                JoyueLib.Context memory rejCtx = JoyueLib.newTransactionContext(
                    address(0),
                    poolMaster,
                    AmmPoolMasterV2.swapIntent.selector,
                    abi.encode(user, amountIn, minAmountOut)
                );
                emit IntentRejected(rejCtx.txId, REASON_RESERVE_VERSION_LOCKED);
                return (false, rejCtx.txId, rejCtx.guards, rejCtx.deltas);
            }
        }

        JoyueLib.Context memory ctx = JoyueLib.newTransactionContext(
            address(0),
            poolMaster,
            AmmPoolMasterV2.swapIntent.selector,
            abi.encode(user, amountIn, minAmountOut)
        );
        return _finalizeSwapAfterLockOk(ctx, user, amountIn, minAmountOut, overrides);
    }

    /// @dev 独立栈帧，避免 _swapInner 与 _loadAndValidateSwap 叠加导致 Stack too deep
    function _finalizeSwapAfterLockOk(
        JoyueLib.Context memory ctx,
        address user,
        uint256 amountIn,
        uint256 minAmountOut,
        JoyueLib.StateOverride[] memory overrides
    ) private returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas) {
        (bool swapOk, uint256 amountOut, uint64 snapA, uint64 snapB) =
            _loadAndValidateSwap(ctx, user, amountIn, minAmountOut, overrides);
        if (!swapOk) {
            emit IntentRejected(ctx.txId, REASON_CACHE_INSUFFICIENT);
            return (false, ctx.txId, ctx.guards, ctx.deltas);
        }

        _acquireReserveLock(snapA, snapB);

        ctx.emitIntentViaPrecompile(shardPool, poolMaster);
        JoyueLib.emitIntent(ctx);
        emit IntentSent(ctx.txId, user, amountIn, amountOut, minAmountOut);

        return (true, ctx.txId, ctx.guards, ctx.deltas);
    }

    function _loadAndValidateSwap(
        JoyueLib.Context memory ctx,
        address user,
        uint256 amountIn,
        uint256 minAmountOut,
        JoyueLib.StateOverride[] memory overrides
    ) internal returns (bool, uint256 amountOut, uint64 verAtSnapshot, uint64 verBtSnapshot) {
        if (amountIn == 0 || minAmountOut == 0) {
            return (false, 0, 0, 0);
        }

        JoyueLib.JVar memory rA =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, poolMaster, shardPool, RESERVE_A_KEY, overrides);
        JoyueLib.JVar memory rB =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, poolMaster, shardPool, RESERVE_B_KEY, overrides);

        if (!rA.checkEq(ctx, JoyueLib.toUint(rA.value))) {
            return (false, 0, 0, 0);
        }
        if (!rB.checkEq(ctx, JoyueLib.toUint(rB.value))) {
            return (false, 0, 0, 0);
        }

        verAtSnapshot = rA.version;
        verBtSnapshot = rB.version;

        uint256 ra = JoyueLib.toUint(rA.value);
        uint256 rb = JoyueLib.toUint(rB.value);
        if (ra == 0 || rb == 0) {
            return (false, 0, 0, 0);
        }

        amountOut = (rb * amountIn) / (ra + amountIn);
        if (amountOut < minAmountOut || amountOut == 0) {
            return (false, 0, 0, 0);
        }
        if (amountOut > rb) {
            return (false, 0, 0, 0);
        }

        if (!_applyUserAndPoolDeltas(ctx, user, amountIn, amountOut, rA, rB, overrides)) {
            return (false, 0, 0, 0);
        }
        return (true, amountOut, verAtSnapshot, verBtSnapshot);
    }

    /// @dev 拆出以降低 _loadAndValidateSwap 栈深（避免 Stack too deep）
    function _applyUserAndPoolDeltas(
        JoyueLib.Context memory ctx,
        address user,
        uint256 amountIn,
        uint256 outAmt,
        JoyueLib.JVar memory rA,
        JoyueLib.JVar memory rB,
        JoyueLib.StateOverride[] memory overrides
    ) private returns (bool) {
        bytes32 keyA = _balanceKeyA(user);
        JoyueLib.JVar memory balA =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, walletMasterA, shardA, keyA, overrides);
        if (!balA.checkGte(ctx, amountIn)) {
            return false;
        }

        bytes32 keyB = _balanceKeyB(user);
        JoyueLib.JVar memory balB =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, walletMasterB, shardB, keyB, overrides);
        uint256 bCur = JoyueLib.toUint(balB.value);
        if (bCur > type(uint256).max - outAmt) {
            return false;
        }

        balA.sub(ctx, amountIn);
        balB.add(ctx, outAmt);
        rA.add(ctx, amountIn);
        rB.sub(ctx, outAmt);
        return true;
    }

    function recomputeIntent(
        JoyueLib.RawRequest calldata req,
        address,
        JoyueLib.StateOverride[] calldata stateOverrides
    ) external returns (JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas, bool ok) {
        if (req.selector != AmmPoolMasterV2.swapIntent.selector) {
            return (guards, deltas, false);
        }
        if (req.targetAddr != poolMaster) {
            return (guards, deltas, false);
        }
        (address user, uint256 amountIn, uint256 minAmountOut) = abi.decode(req.args, (address, uint256, uint256));
        if (user == address(0)) {
            return (guards, deltas, false);
        }

        JoyueLib.StateOverride[] memory ov = new JoyueLib.StateOverride[](stateOverrides.length);
        for (uint256 i = 0; i < stateOverrides.length; i++) {
            ov[i] = stateOverrides[i];
        }

        JoyueLib.Context memory ctx =
            JoyueLib.newContext(address(0), poolMaster, req.selector, req.args);
        (ok, , , ) = _loadAndValidateSwap(ctx, user, amountIn, minAmountOut, ov);
        return (ctx.guards, ctx.deltas, ok);
    }
}
