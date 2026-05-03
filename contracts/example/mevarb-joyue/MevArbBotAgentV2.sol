// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../../baselib/JoyueLib.sol";
import "./MevArbBotMasterV2.sol";

/**
 * @title MevArbBotAgentV2
 * @dev 单 Agent 原子意图：简化闪电贷（**独立** `MevArbFlashLenderMasterV2`）→ 低价池 A→B → 高价池 B→A → 还贷；
 *      净利润（A）记入 **`MevArbProfitWalletMasterV2`**，不维护中间 WalletA/WalletB。
 *      RawRequest / `emitIntentViaPrecompile` 锚在 **`MevArbBotMasterV2`**。
 *      恒定乘积与 `AmmSwapAgentV2` 一致；`IntentSent` 签名与 AMM 一致。
 *      `arbRandom`：user 为 `msg.sender`；`arbExplicit` 须 `user != address(0)`。
 */
contract MevArbBotAgentV2 {
    using JoyueLib for JoyueLib.JVar;
    using JoyueLib for JoyueLib.Context;

    bytes32 private constant FLASH_AVAILABLE_KEY = keccak256(abi.encodePacked("mevarb.joyue.flash.available"));

    bytes32 private constant LOW_RA_KEY = keccak256(abi.encodePacked("mevarb.joyue.poolLow.reserveA"));
    bytes32 private constant LOW_RB_KEY = keccak256(abi.encodePacked("mevarb.joyue.poolLow.reserveB"));

    bytes32 private constant HIGH_RA_KEY = keccak256(abi.encodePacked("mevarb.joyue.poolHigh.reserveA"));
    bytes32 private constant HIGH_RB_KEY = keccak256(abi.encodePacked("mevarb.joyue.poolHigh.reserveB"));

    /// @dev 套利仿真中间态，避免单帧堆积多个 `JVar memory` 导致 stack too deep。
    struct ArbMevPack {
        JoyueLib.JVar flashAvail;
        JoyueLib.JVar lowRa;
        JoyueLib.JVar lowRb;
        JoyueLib.JVar highRa;
        JoyueLib.JVar highRb;
        JoyueLib.JVar profitBal;
        uint256 outLow;
        uint256 outHigh;
        uint256 netProfit;
        uint64 snapF;
        uint64 snapLa;
        uint64 snapLb;
        uint64 snapHa;
        uint64 snapHb;
        uint8 failReason;
    }

    /// @dev `_loadAndApplyArb` 单返回值，避免调用方 8 元组解构导致 stack too deep。
    struct ArbLoadResult {
        bool arbOk;
        uint256 outHigh;
        uint8 failReason;
        uint64 snapF;
        uint64 snapLa;
        uint64 snapLb;
        uint64 snapHa;
        uint64 snapHb;
    }

    address public immutable botMaster;
    address public immutable flashLenderMaster;
    address public immutable poolLowMaster;
    address public immutable poolHighMaster;
    address public immutable profitWalletMaster;
    uint32 public immutable shardBot;
    uint32 public immutable shardFlash;
    uint32 public immutable shardLow;
    uint32 public immutable shardHigh;
    uint32 public immutable shardProfit;

    uint8 public constant REASON_CACHE_INSUFFICIENT = 6;
    uint8 public constant REASON_NOT_SIMULATED_USER = 7;
    uint8 public constant REASON_RESERVE_VERSION_LOCKED = 8;
    uint8 public constant REASON_ARB_NOT_VIABLE = 9;

    uint256 public constant RESERVE_LOCK_TTL_BLOCKS = 256;

    bool private _arbLockActive;
    uint64 private _lockFlashVer;
    uint64 private _lockLowA;
    uint64 private _lockLowB;
    uint64 private _lockHighA;
    uint64 private _lockHighB;
    uint256 private _lockDeadlineBlock;

    event IntentSent(bytes32 indexed txId, address indexed user, uint256 amountIn, uint256 amountOut, uint256 minAmountOut);
    event IntentRejected(bytes32 indexed txId, uint8 reason);

    constructor(
        address botMaster_,
        address flashLender_,
        address poolLow_,
        address poolHigh_,
        address profitWallet_,
        uint32 shardBot_,
        uint32 shardFlash_,
        uint32 shardLow_,
        uint32 shardHigh_,
        uint32 shardProfit_
    ) {
        require(
            botMaster_ != address(0) &&
                flashLender_ != address(0) &&
                poolLow_ != address(0) &&
                poolHigh_ != address(0) &&
                profitWallet_ != address(0),
            "MevArb: zero"
        );
        require(botMaster_ != flashLender_, "MevArb: bot==lender");
        botMaster = botMaster_;
        flashLenderMaster = flashLender_;
        poolLowMaster = poolLow_;
        poolHighMaster = poolHigh_;
        profitWalletMaster = profitWallet_;
        shardBot = shardBot_;
        shardFlash = shardFlash_;
        shardLow = shardLow_;
        shardHigh = shardHigh_;
        shardProfit = shardProfit_;
    }

    function _profitKey(address user) private pure returns (bytes32) {
        return JoyueLib.keyOfAddr("mevarb.joyue.profit.balance:", user);
    }

    function arbRandom(uint256 borrowA, uint256 minNetProfitA)
        external
        returns (bool ok, bytes32 txId, address user, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        user = msg.sender;
        (ok, txId, guards, deltas) = _arbInner(user, borrowA, minNetProfitA, new JoyueLib.StateOverride[](0));
    }

    function arbExplicit(address user, uint256 borrowA, uint256 minNetProfitA)
        external
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        if (user == address(0)) {
            txId = bytes32(0);
            emit IntentRejected(txId, REASON_NOT_SIMULATED_USER);
            return (false, txId, guards, deltas);
        }
        return _arbInner(user, borrowA, minNetProfitA, new JoyueLib.StateOverride[](0));
    }

    function _peekArbVersions() private view returns (uint64 f, uint64 la, uint64 lb, uint64 ha, uint64 hb) {
        JoyueLib.Context memory peekCtx = JoyueLib.newContext(address(0), botMaster, bytes4(0), bytes(""));
        JoyueLib.JVar memory jf = JoyueLib.loadFromCacheByKey(peekCtx, flashLenderMaster, shardFlash, FLASH_AVAILABLE_KEY);
        JoyueLib.JVar memory jla = JoyueLib.loadFromCacheByKey(peekCtx, poolLowMaster, shardLow, LOW_RA_KEY);
        JoyueLib.JVar memory jlb = JoyueLib.loadFromCacheByKey(peekCtx, poolLowMaster, shardLow, LOW_RB_KEY);
        JoyueLib.JVar memory jha = JoyueLib.loadFromCacheByKey(peekCtx, poolHighMaster, shardHigh, HIGH_RA_KEY);
        JoyueLib.JVar memory jhb = JoyueLib.loadFromCacheByKey(peekCtx, poolHighMaster, shardHigh, HIGH_RB_KEY);
        f = jf.version;
        la = jla.version;
        lb = jlb.version;
        ha = jha.version;
        hb = jhb.version;
    }

    function _maybeReleaseArbLock(uint64 f, uint64 la, uint64 lb, uint64 ha, uint64 hb) private {
        if (!_arbLockActive) {
            return;
        }
        if (block.number > _lockDeadlineBlock) {
            _arbLockActive = false;
            return;
        }
        if (f != _lockFlashVer || la != _lockLowA || lb != _lockLowB || ha != _lockHighA || hb != _lockHighB) {
            _arbLockActive = false;
        }
    }

    function _acquireArbLock(uint64 f, uint64 la, uint64 lb, uint64 ha, uint64 hb) private {
        _arbLockActive = true;
        _lockFlashVer = f;
        _lockLowA = la;
        _lockLowB = lb;
        _lockHighA = ha;
        _lockHighB = hb;
        unchecked {
            _lockDeadlineBlock = block.number + RESERVE_LOCK_TTL_BLOCKS;
        }
    }

    /// @dev 将 `abi.encode` 与 `newTransactionContext` 移出 `_arbInner`，避免 stack too deep。
    function _newArbTransactionContext(address user, uint256 borrowA, uint256 minNetProfitA)
        private
        returns (JoyueLib.Context memory ctx)
    {
        ctx = JoyueLib.newTransactionContext(
            address(0),
            botMaster,
            MevArbBotMasterV2.executeArbIntent.selector,
            abi.encode(user, borrowA, minNetProfitA)
        );
    }

    /// @dev 与 `_arbInner` 拆栈：此处再调用 `_loadAndApplyArb` 并 emit，避免与版本锁 peek 同帧。
    function _arbInnerContinue(
        JoyueLib.Context memory ctx,
        address user,
        uint256 borrowA,
        uint256 minNetProfitA,
        JoyueLib.StateOverride[] memory overrides
    ) private returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas) {
        ArbLoadResult memory lr = _loadAndApplyArb(ctx, user, borrowA, minNetProfitA, overrides);
        if (!lr.arbOk) {
            emit IntentRejected(ctx.txId, lr.failReason);
            return (false, ctx.txId, ctx.guards, ctx.deltas);
        }

        _acquireArbLock(lr.snapF, lr.snapLa, lr.snapLb, lr.snapHa, lr.snapHb);

        JoyueLib.emitIntentViaPrecompile(ctx, shardBot, botMaster);
        JoyueLib.emitIntent(ctx);
        emit IntentSent(ctx.txId, user, borrowA, lr.outHigh, minNetProfitA);

        return (true, ctx.txId, ctx.guards, ctx.deltas);
    }

    function _arbInner(address user, uint256 borrowA, uint256 minNetProfitA, JoyueLib.StateOverride[] memory overrides)
        private
        returns (bool ok, bytes32 txId, JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas)
    {
        {
            (uint64 pf, uint64 pla, uint64 plb, uint64 pha, uint64 phb) = _peekArbVersions();
            _maybeReleaseArbLock(pf, pla, plb, pha, phb);
            if (_arbLockActive && pf == _lockFlashVer && pla == _lockLowA && plb == _lockLowB && pha == _lockHighA && phb == _lockHighB) {
                JoyueLib.Context memory rejCtx = _newArbTransactionContext(user, borrowA, minNetProfitA);
                emit IntentRejected(rejCtx.txId, REASON_RESERVE_VERSION_LOCKED);
                return (false, rejCtx.txId, rejCtx.guards, rejCtx.deltas);
            }
        }

        JoyueLib.Context memory ctx = _newArbTransactionContext(user, borrowA, minNetProfitA);
        return _arbInnerContinue(ctx, user, borrowA, minNetProfitA, overrides);
    }

    function _cpOutB(uint256 ra, uint256 rb, uint256 dx) private pure returns (uint256) {
        if (ra == 0 || dx == 0) {
            return 0;
        }
        return (rb * dx) / (ra + dx);
    }

    function _cpOutA(uint256 ra, uint256 rb, uint256 dy) private pure returns (uint256) {
        if (rb == 0 || dy == 0) {
            return 0;
        }
        return (ra * dy) / (rb + dy);
    }

    function _arbPackFlashPhase(
        JoyueLib.Context memory ctx,
        uint256 borrowA,
        JoyueLib.StateOverride[] memory overrides,
        ArbMevPack memory p
    ) private returns (bool) {
        p.flashAvail =
            JoyueLib.loadFromCacheByKeyWithOverrides(ctx, flashLenderMaster, shardFlash, FLASH_AVAILABLE_KEY, overrides);
        p.snapF = p.flashAvail.version;
        return p.flashAvail.checkGte(ctx, borrowA);
    }

    function _arbPackLowPhase(JoyueLib.Context memory ctx, JoyueLib.StateOverride[] memory overrides, ArbMevPack memory p)
        private
        returns (bool)
    {
        p.lowRa = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, poolLowMaster, shardLow, LOW_RA_KEY, overrides);
        p.lowRb = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, poolLowMaster, shardLow, LOW_RB_KEY, overrides);
        p.snapLa = p.lowRa.version;
        p.snapLb = p.lowRb.version;
        return p.lowRa.checkEq(ctx, JoyueLib.toUint(p.lowRa.value)) && p.lowRb.checkEq(ctx, JoyueLib.toUint(p.lowRb.value));
    }

    function _arbPackHighPhase(JoyueLib.Context memory ctx, JoyueLib.StateOverride[] memory overrides, ArbMevPack memory p)
        private
        returns (bool)
    {
        p.highRa = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, poolHighMaster, shardHigh, HIGH_RA_KEY, overrides);
        p.highRb = JoyueLib.loadFromCacheByKeyWithOverrides(ctx, poolHighMaster, shardHigh, HIGH_RB_KEY, overrides);
        p.snapHa = p.highRa.version;
        p.snapHb = p.highRb.version;
        return p.highRa.checkEq(ctx, JoyueLib.toUint(p.highRa.value))
            && p.highRb.checkEq(ctx, JoyueLib.toUint(p.highRb.value));
    }

    function _arbPackMathProfitPhase(
        JoyueLib.Context memory ctx,
        address user,
        uint256 borrowA,
        uint256 minNetProfitA,
        JoyueLib.StateOverride[] memory overrides,
        ArbMevPack memory p
    ) private returns (bool) {
        uint256 raL = JoyueLib.toUint(p.lowRa.value);
        uint256 rbL = JoyueLib.toUint(p.lowRb.value);
        uint256 raH = JoyueLib.toUint(p.highRa.value);
        uint256 rbH = JoyueLib.toUint(p.highRb.value);
        if (raL == 0 || rbL == 0 || raH == 0 || rbH == 0) {
            return false;
        }

        p.outLow = _cpOutB(raL, rbL, borrowA);
        if (p.outLow == 0 || p.outLow > rbL) {
            return false;
        }

        p.outHigh = _cpOutA(raH, rbH, p.outLow);
        if (p.outHigh == 0 || p.outHigh > raH) {
            return false;
        }
        if (p.outHigh < borrowA + minNetProfitA) {
            p.failReason = REASON_ARB_NOT_VIABLE;
            return false;
        }

        p.profitBal = JoyueLib.loadFromCacheByKeyWithOverrides(
            ctx, profitWalletMaster, shardProfit, _profitKey(user), overrides
        );
        unchecked {
            p.netProfit = p.outHigh - borrowA;
        }
        return true;
    }

    function _arbApplyPackDeltas(JoyueLib.Context memory ctx, ArbMevPack memory p, uint256 borrowA) private {
        p.flashAvail.sub(ctx, borrowA);
        p.lowRa.add(ctx, borrowA);
        p.lowRb.sub(ctx, p.outLow);
        p.highRb.add(ctx, p.outLow);
        p.highRa.sub(ctx, p.outHigh);
        p.flashAvail.add(ctx, borrowA);
        p.profitBal.add(ctx, p.netProfit);
    }

    function _loadAndApplyArb(
        JoyueLib.Context memory ctx,
        address user,
        uint256 borrowA,
        uint256 minNetProfitA,
        JoyueLib.StateOverride[] memory overrides
    ) private returns (ArbLoadResult memory r) {
        if (borrowA == 0) {
            r.arbOk = false;
            r.failReason = REASON_CACHE_INSUFFICIENT;
            return r;
        }

        ArbMevPack memory p;
        p.failReason = REASON_CACHE_INSUFFICIENT;

        if (!_arbPackFlashPhase(ctx, borrowA, overrides, p)) {
            r.arbOk = false;
            r.failReason = REASON_CACHE_INSUFFICIENT;
            r.snapF = p.snapF;
            r.snapLa = p.snapLa;
            r.snapLb = p.snapLb;
            r.snapHa = p.snapHa;
            r.snapHb = p.snapHb;
            return r;
        }
        if (!_arbPackLowPhase(ctx, overrides, p)) {
            r.arbOk = false;
            r.failReason = REASON_CACHE_INSUFFICIENT;
            r.snapF = p.snapF;
            r.snapLa = p.snapLa;
            r.snapLb = p.snapLb;
            r.snapHa = p.snapHa;
            r.snapHb = p.snapHb;
            return r;
        }
        if (!_arbPackHighPhase(ctx, overrides, p)) {
            r.arbOk = false;
            r.failReason = REASON_CACHE_INSUFFICIENT;
            r.snapF = p.snapF;
            r.snapLa = p.snapLa;
            r.snapLb = p.snapLb;
            r.snapHa = p.snapHa;
            r.snapHb = p.snapHb;
            return r;
        }
        if (!_arbPackMathProfitPhase(ctx, user, borrowA, minNetProfitA, overrides, p)) {
            r.arbOk = false;
            r.failReason = p.failReason;
            r.snapF = p.snapF;
            r.snapLa = p.snapLa;
            r.snapLb = p.snapLb;
            r.snapHa = p.snapHa;
            r.snapHb = p.snapHb;
            return r;
        }

        _arbApplyPackDeltas(ctx, p, borrowA);
        r.arbOk = true;
        r.outHigh = p.outHigh;
        r.failReason = 0;
        r.snapF = p.snapF;
        r.snapLa = p.snapLa;
        r.snapLb = p.snapLb;
        r.snapHa = p.snapHa;
        r.snapHb = p.snapHb;
        return r;
    }

    /// @dev 将 `newContext` + `_loadAndApplyArb` 单独成帧。
    function _recomputeArbApply(
        address user,
        uint256 borrowA,
        uint256 minNetProfitA,
        JoyueLib.StateOverride[] memory ov,
        bytes4 sel,
        bytes memory reqArgs
    ) private returns (JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas, bool ok) {
        JoyueLib.Context memory ctx = JoyueLib.newContext(address(0), botMaster, sel, reqArgs);
        ArbLoadResult memory lr = _loadAndApplyArb(ctx, user, borrowA, minNetProfitA, ov);
        ok = lr.arbOk;
        return (ctx.guards, ctx.deltas, ok);
    }

    /// @dev 在 selector/target 已校验后执行 decode、override 拷贝与仿真；避免 `recomputeIntent` 外层 stack too deep。
    function _recomputeAfterGate(JoyueLib.RawRequest calldata req, JoyueLib.StateOverride[] calldata stateOverrides)
        private
        returns (JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas, bool ok)
    {
        (address user, uint256 borrowA, uint256 minNetProfitA) = abi.decode(req.args, (address, uint256, uint256));
        if (user == address(0)) {
            return (guards, deltas, false);
        }

        JoyueLib.StateOverride[] memory ov = new JoyueLib.StateOverride[](stateOverrides.length);
        for (uint256 i = 0; i < stateOverrides.length; i++) {
            ov[i] = stateOverrides[i];
        }

        bytes4 sel = req.selector;
        bytes memory reqArgs = req.args;
        return _recomputeArbApply(user, borrowA, minNetProfitA, ov, sel, reqArgs);
    }

    function recomputeIntent(
        JoyueLib.RawRequest calldata req,
        address,
        JoyueLib.StateOverride[] calldata stateOverrides
    ) external returns (JoyueLib.Guard[] memory guards, JoyueLib.Delta[] memory deltas, bool ok) {
        if (req.selector != MevArbBotMasterV2.executeArbIntent.selector) {
            return (guards, deltas, false);
        }
        if (req.targetAddr != botMaster) {
            return (guards, deltas, false);
        }
        return _recomputeAfterGate(req, stateOverrides);
    }
}
