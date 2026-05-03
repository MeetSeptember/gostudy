// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

interface IFlashMevSparrow {
    function quoteLend(bytes32 txId, address user, uint256 borrowA) external view returns (bool);
}

interface ILowMevSparrow {
    function quoteSwapAToB(uint256 amountInA, uint256 minOutB, address user)
        external
        view
        returns (bool ok, uint256 amountOutB, uint256 newReserveA, uint256 newReserveB, uint256 versionRead);
}

interface IHighMevSparrow {
    function quoteSwapBToA(uint256 amountInB, uint256 minOutA, address user)
        external
        view
        returns (bool ok, uint256 amountOutA, uint256 newReserveA, uint256 newReserveB, uint256 versionRead);
}

/**
 * @title SparrowMevArbCoordinator
 * @dev 与 `SparrowAmmCoordinator` 同形：对两池用 **入口快照** 并行算每行 `newReserve*`（与 AMM 组包一致）；池子 `prepareReadWriteBatch` 仅 **首行** 可能成功并 `poolVersion++`，
 *      其余行恒为 false（与 `SparrowAmmPool` 一致）。故 **n>1 时整波通常在 finalize 失败**；通路径 **n=1**。Flash/Profit 仍为全行 `(allOk, lineOk)`。
 *      `SparrowWaveFinished` 在 prepare 收尾时按条发出，bool 为 **该条 `_itemPrepareSuccess`**（与 `SparrowAmmCoordinator._finalizeWave` 一致）；Profit 在首轮 `prepareLockBatch` 即带 `amounts`，全条通过后直接 `commitWave`，无单独 seal 轮。
 */
contract SparrowMevArbCoordinator {
    address public immutable flash;
    address public immutable poolLow;
    address public immutable poolHigh;
    address public immutable profit;
    uint32 public immutable shardFlash;
    uint32 public immutable shardLow;
    uint32 public immutable shardHigh;
    uint32 public immutable shardProfit;

    uint256 private constant _KIND_FLASH = 1;
    uint256 private constant _KIND_LOW = 2;
    uint256 private constant _KIND_HIGH = 3;
    uint256 private constant _KIND_PROFIT = 4;
    uint256 private constant _PREP_CNT = 4;

    uint256 private _waveNonce;
    uint256 private _prepareReqId;
    uint256 private _execReqId;

    struct ArbItem {
        address user;
        uint256 borrowA;
        uint256 minNetProfitA;
        bytes32 txId;
    }

    struct Wave {
        uint32 pending;
        bool anyFail;
        bool active;
    }

    struct ReqMeta {
        uint256 waveKey;
        uint8 kind;
        uint256 batchIdx;
    }

    mapping(uint256 => Wave) private _wave;
    mapping(uint256 => ArbItem[]) private _waveItems;
    mapping(uint256 => ReqMeta) private _reqToWave;

    mapping(uint256 => bytes32) private _bidFlash;
    mapping(uint256 => bytes32) private _bidLow;
    mapping(uint256 => bytes32) private _bidHigh;
    mapping(uint256 => bytes32) private _bidProfit;

    mapping(uint256 => mapping(uint256 => bool[])) private _lineFlash;
    mapping(uint256 => mapping(uint256 => bool[])) private _lineLow;
    mapping(uint256 => mapping(uint256 => bool[])) private _lineHigh;
    mapping(uint256 => mapping(uint256 => bool[])) private _lineProfit;

    mapping(uint256 => bool) private _poolLowActive;
    mapping(uint256 => bool) private _poolHighActive;
    mapping(uint256 => bool) private _flashActive;
    mapping(uint256 => bool) private _profitLockActive;

    mapping(uint256 => uint256[]) private _waveNetProfit;

    /// @dev `_quoteMev` 输出：低价池/高价池提案储备（每行基于同一快照并行计算）
    struct MevQuotePack {
        bool ok;
        uint256[] outLows;
        uint256[] outHighs;
        uint256[] newRAsLow;
        uint256[] newRBsLow;
        uint256[] newRAsHigh;
        uint256[] newRBsHigh;
    }

    /// @dev 单行报价结果；将 `_quoteMev` 体内逻辑外提，避免 Stack too deep。
    struct MevQuoteLine {
        bool ok;
        uint256 outL;
        uint256 outH;
        uint256 nraL;
        uint256 nrbL;
        uint256 nraH;
        uint256 nrbH;
    }

    function _quoteMevLine(ILowMevSparrow low, IHighMevSparrow high, ArbItem memory item) private view returns (MevQuoteLine memory ln) {
        if (!IFlashMevSparrow(flash).quoteLend(item.txId, item.user, item.borrowA)) {
            return ln;
        }
        uint256 b = item.borrowA;
        (bool okL, uint256 outL, uint256 nraL, uint256 nrbL, ) = low.quoteSwapAToB(b, 1, item.user);
        if (!okL) {
            return ln;
        }
        uint256 minA;
        unchecked {
            minA = b + item.minNetProfitA;
        }
        (bool okH, uint256 outH, uint256 nraH, uint256 nrbH, ) = high.quoteSwapBToA(outL, minA, item.user);
        if (!okH || outH <= b) {
            return ln;
        }
        ln.ok = true;
        ln.outL = outL;
        ln.outH = outH;
        ln.nraL = nraL;
        ln.nrbL = nrbL;
        ln.nraH = nraH;
        ln.nrbH = nrbH;
    }

    /// @dev 四路 prepare 所需引用打包，避免 `_emitWavePrepares` / `_emitPreparesFromCtx` 栈过深。
    struct PrepareCtx {
        uint256 waveKey;
        bytes32 bf;
        bytes32 bl;
        bytes32 bh;
        bytes32 bp;
        bytes32[] txIds;
        address[] users;
        uint256[] borrows;
        uint256[] clientVs;
        uint256[] nets;
        uint256[] newRAsLow;
        uint256[] newRBsLow;
        uint256[] newRAsHigh;
        uint256[] newRBsHigh;
    }

    function _emitWavePrepares(uint256 waveKey, MevQuotePack memory pk, bytes32[] memory txIds, ArbItem[] memory items) private {
        uint256 n = items.length;
        PrepareCtx memory c;
        c.waveKey = waveKey;
        c.bf = keccak256(abi.encodePacked(waveKey, uint8(41)));
        c.bl = keccak256(abi.encodePacked(waveKey, uint8(42)));
        c.bh = keccak256(abi.encodePacked(waveKey, uint8(43)));
        c.bp = keccak256(abi.encodePacked(waveKey, uint8(44)));
        _bidFlash[waveKey] = c.bf;
        _bidLow[waveKey] = c.bl;
        _bidHigh[waveKey] = c.bh;
        _bidProfit[waveKey] = c.bp;

        c.txIds = txIds;
        c.users = new address[](n);
        c.borrows = new uint256[](n);
        c.clientVs = new uint256[](n);
        c.nets = new uint256[](n);
        for (uint256 j = 0; j < n; j++) {
            c.users[j] = items[j].user;
            c.borrows[j] = items[j].borrowA;
            c.nets[j] = _waveNetProfit[waveKey][j];
        }
        c.newRAsLow = pk.newRAsLow;
        c.newRBsLow = pk.newRBsLow;
        c.newRAsHigh = pk.newRAsHigh;
        c.newRBsHigh = pk.newRBsHigh;

        _emitPreparesFromCtx(c);
    }

    function _emitPreparesFromCtx(PrepareCtx memory c) private {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes memory cdFlash = _cdFlash(c.bf, c.txIds, c.users, c.borrows);
        _emitPrepare(c.waveKey, shardFlash, flash, selfShard, cdFlash, uint8(_KIND_FLASH), 0);

        bytes memory cdLow = _cdPoolPrepare(c.bl, c.newRAsLow, c.newRBsLow, c.clientVs);
        _emitPrepare(c.waveKey, shardLow, poolLow, selfShard, cdLow, uint8(_KIND_LOW), 0);

        bytes memory cdHigh = _cdPoolPrepare(c.bh, c.newRAsHigh, c.newRBsHigh, c.clientVs);
        _emitPrepare(c.waveKey, shardHigh, poolHigh, selfShard, cdHigh, uint8(_KIND_HIGH), 0);

        bytes memory cdProfit = _cdProfitPrepare(c.bp, c.txIds, c.users, c.nets);
        _emitPrepare(c.waveKey, shardProfit, profit, selfShard, cdProfit, uint8(_KIND_PROFIT), 0);
    }

    event SparrowWaveStarted(bytes32 indexed waveId, uint256 mainTxCount, bytes32[] txIds);
    event SparrowWaveFinished(bytes32 indexed txId, bool committed);

    constructor(
        address flash_,
        address poolLow_,
        address poolHigh_,
        address profit_,
        uint32 shardFlash_,
        uint32 shardLow_,
        uint32 shardHigh_,
        uint32 shardProfit_
    ) {
        require(flash_ != address(0) && poolLow_ != address(0) && poolHigh_ != address(0) && profit_ != address(0), "SMev: addr");
        flash = flash_;
        poolLow = poolLow_;
        poolHigh = poolHigh_;
        profit = profit_;
        shardFlash = shardFlash_;
        shardLow = shardLow_;
        shardHigh = shardHigh_;
        shardProfit = shardProfit_;
    }

    function arbWave(ArbItem[] calldata items) external {
        ArbItem[] memory m = new ArbItem[](items.length);
        for (uint256 i = 0; i < items.length; i++) {
            m[i] = items[i];
        }
        _executeWave(m);
    }

    function arbWave1(address user, uint256 borrowA, uint256 minNetProfitA) external {
        ArbItem[] memory m = new ArbItem[](1);
        m[0] = ArbItem({user: user, borrowA: borrowA, minNetProfitA: minNetProfitA, txId: bytes32(0)});
        _executeWave(m);
    }

    function arbWave1Random(uint256 borrowA, uint256 minNetProfitA) external {
        ArbItem[] memory m = new ArbItem[](1);
        m[0] = ArbItem({user: msg.sender, borrowA: borrowA, minNetProfitA: minNetProfitA, txId: bytes32(0)});
        _executeWave(m);
    }

    function _executeWave(ArbItem[] memory items) private {
        uint256 n = items.length;
        require(n > 0, "SMev: empty");
        require(n <= type(uint32).max, "SMev: n");

        for (uint256 i = 0; i < n; i++) {
            require(items[i].borrowA > 0, "SMev: borrow");
            require(items[i].user != address(0), "SMev: user");
        }
        for (uint256 a = 0; a < n; a++) {
            for (uint256 b = a + 1; b < n; b++) {
                require(items[a].user != items[b].user, "SMev: dup user");
                if (items[a].txId != bytes32(0) && items[b].txId != bytes32(0)) {
                    require(items[a].txId != items[b].txId, "SMev: dup tx");
                }
            }
        }

        for (uint256 t = 0; t < n; t++) {
            if (items[t].txId == bytes32(0)) {
                items[t].txId = keccak256(abi.encodePacked(msg.sender, block.number, t, _waveNonce, gasleft()));
            }
        }

        MevQuotePack memory pk = _quoteMev(items);
        if (!pk.ok) {
            for (uint256 k = 0; k < n; k++) {
                emit SparrowWaveFinished(items[k].txId, false);
            }
            return;
        }

        uint256 waveKey = ++_waveNonce;
        delete _waveNetProfit[waveKey];
        for (uint256 i = 0; i < n; i++) {
            _waveItems[waveKey].push(items[i]);
            unchecked {
                _waveNetProfit[waveKey].push(pk.outHighs[i] - items[i].borrowA);
            }
        }

        Wave storage w = _wave[waveKey];
        w.active = true;
        w.anyFail = false;
        w.pending = uint32(_PREP_CNT);

        bytes32[] memory txIds = new bytes32[](n);
        for (uint256 t = 0; t < n; t++) {
            txIds[t] = items[t].txId;
        }
        emit SparrowWaveStarted(bytes32(waveKey), n, txIds);

        _emitWavePrepares(waveKey, pk, txIds, items);
    }

    function _cdFlash(bytes32 batchId, bytes32[] memory txIds, address[] memory users, uint256[] memory borrows)
        private
        pure
        returns (bytes memory)
    {
        return abi.encodeWithSignature(
            "prepareLendBatch(bytes32,bytes32[],address[],uint256[])", batchId, txIds, users, borrows
        );
    }

    /// @dev `clientVs` 全 0 时池子首行用入口 `poolVersion` 快照（与 SparrowAmm `clientVersion==0` 一致）
    /// @dev 与 `SparrowAmmCoordinator` 调 Pool 的 ABI 一致：`prepareReadWriteBatch(bytes32,uint256[],uint256[],uint256[])`。
    function _cdPoolPrepare(bytes32 batchId, uint256[] memory newRAs, uint256[] memory newRBs, uint256[] memory clientVs)
        private
        pure
        returns (bytes memory)
    {
        return abi.encodeWithSignature(
            "prepareReadWriteBatch(bytes32,uint256[],uint256[],uint256[])",
            batchId,
            newRAs,
            newRBs,
            clientVs
        );
    }

    function _cdProfitPrepare(bytes32 batchId, bytes32[] memory txIds, address[] memory users, uint256[] memory amounts)
        private
        pure
        returns (bytes memory)
    {
        return abi.encodeWithSignature(
            "prepareLockBatch(bytes32,bytes32[],address[],uint256[])", batchId, txIds, users, amounts
        );
    }

    /// @notice 与 AMM `_quoteWaveForPoolPack` 一致：组包时 **只调池子 view**（`quoteSwap*`），核心定价在 Pool；各行读取的链上储备相同，等价于同一入口快照下并行报价。
    function _quoteMev(ArbItem[] memory items) private view returns (MevQuotePack memory q) {
        uint256 n = items.length;
        q.outLows = new uint256[](n);
        q.outHighs = new uint256[](n);
        q.newRAsLow = new uint256[](n);
        q.newRBsLow = new uint256[](n);
        q.newRAsHigh = new uint256[](n);
        q.newRBsHigh = new uint256[](n);

        ILowMevSparrow low = ILowMevSparrow(poolLow);
        IHighMevSparrow high = IHighMevSparrow(poolHigh);

        for (uint256 i = 0; i < n; i++) {
            MevQuoteLine memory ln = _quoteMevLine(low, high, items[i]);
            if (!ln.ok) {
                return q;
            }
            q.outLows[i] = ln.outL;
            q.outHighs[i] = ln.outH;
            q.newRAsLow[i] = ln.nraL;
            q.newRBsLow[i] = ln.nrbL;
            q.newRAsHigh[i] = ln.nraH;
            q.newRBsHigh[i] = ln.nrbH;
        }
        q.ok = true;
        return q;
    }

    function _emitPrepare(
        uint256 waveKey,
        uint32 targetShardId,
        address targetAddr,
        uint32 sourceShardId,
        bytes memory targetCalldata,
        uint8 kind,
        uint256 batchIdx
    ) private {
        unchecked {
            _prepareReqId++;
        }
        uint256 requestId = _prepareReqId;
        _reqToWave[requestId] = ReqMeta({waveKey: waveKey, kind: kind, batchIdx: batchIdx});
        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onPrepareResponse.selector,
            requestId,
            targetAddr,
            targetCalldata
        );
        require(TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata), "SMev: emit");
    }

    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata returnData) external {
        ReqMeta memory meta = _reqToWave[requestId];
        delete _reqToWave[requestId];
        require(meta.waveKey != 0, "SMev: bad req");

        uint256 waveKey = meta.waveKey;
        Wave storage w = _wave[waveKey];
        require(w.active, "SMev: inactive");

        uint256 n = _waveItems[waveKey].length;

        if (!ok) {
            w.anyFail = true;
        } else if (meta.kind == uint8(_KIND_LOW)) {
            (bool batchPrepared, bool[] memory lines) = abi.decode(returnData, (bool, bool[]));
            if (lines.length != n) {
                w.anyFail = true;
                if (batchPrepared) {
                    _poolLowActive[waveKey] = true;
                }
            } else {
                _storeLines(_lineLow, waveKey, meta.batchIdx, lines);
                if (!batchPrepared) {
                    w.anyFail = true;
                } else {
                    _poolLowActive[waveKey] = true;
                }
            }
        } else if (meta.kind == uint8(_KIND_HIGH)) {
            (bool batchPrepared, bool[] memory lines) = abi.decode(returnData, (bool, bool[]));
            if (lines.length != n) {
                w.anyFail = true;
                if (batchPrepared) {
                    _poolHighActive[waveKey] = true;
                }
            } else {
                _storeLines(_lineHigh, waveKey, meta.batchIdx, lines);
                if (!batchPrepared) {
                    w.anyFail = true;
                } else {
                    _poolHighActive[waveKey] = true;
                }
            }
        } else {
            (bool ao, bool[] memory lines) = abi.decode(returnData, (bool, bool[]));
            if (lines.length != n || !ao) {
                w.anyFail = true;
            } else if (meta.kind == uint8(_KIND_FLASH)) {
                _storeLines(_lineFlash, waveKey, meta.batchIdx, lines);
                _flashActive[waveKey] = true;
            } else if (meta.kind == uint8(_KIND_PROFIT)) {
                _storeLines(_lineProfit, waveKey, meta.batchIdx, lines);
                _profitLockActive[waveKey] = true;
            } else {
                w.anyFail = true;
            }
        }

        require(w.pending > 0, "SMev: pending");
        unchecked {
            w.pending--;
        }
        if (w.pending != 0) {
            return;
        }
        _finalizePrepareWave(waveKey);
    }

    function _storeLines(
        mapping(uint256 => mapping(uint256 => bool[])) storage store,
        uint256 waveKey,
        uint256 batchIdx,
        bool[] memory lines
    ) private {
        delete store[waveKey][batchIdx];
        for (uint256 i = 0; i < lines.length; i++) {
            store[waveKey][batchIdx].push(lines[i]);
        }
    }

    /// @dev 与 `SparrowAmmCoordinator._itemPrepareSuccess` 同义：该 item 在 Flash/Low/High/Profit 四路 prepare 行上是否均为 true。
    function _itemPrepareSuccess(uint256 waveKey, uint256 itemIdx) private view returns (bool) {
        if (_lineFlash[waveKey][0].length <= itemIdx || !_lineFlash[waveKey][0][itemIdx]) {
            return false;
        }
        if (_lineLow[waveKey][0].length <= itemIdx || !_lineLow[waveKey][0][itemIdx]) {
            return false;
        }
        if (_lineHigh[waveKey][0].length <= itemIdx || !_lineHigh[waveKey][0][itemIdx]) {
            return false;
        }
        if (_lineProfit[waveKey][0].length <= itemIdx || !_lineProfit[waveKey][0][itemIdx]) {
            return false;
        }
        return true;
    }

    /// @dev 与 `SparrowAmmCoordinator._finalizeWave` 对齐：`SparrowWaveFinished` 的 bool 表示 **该条 item 的 prepare 聚合是否成功**；全条通过后直接 `commitWave`（无二次 emit）。
    function _finalizePrepareWave(uint256 waveKey) private {
        uint256 n = _waveItems[waveKey].length;
        Wave storage w = _wave[waveKey];

        if (w.anyFail) {
            _abortWave(waveKey);
            for (uint256 k = 0; k < n; k++) {
                emit SparrowWaveFinished(_waveItems[waveKey][k].txId, false);
            }
            _cleanupWave(waveKey);
            return;
        }

        bool anyIncludedFail = false;
        for (uint256 k = 0; k < n; k++) {
            bool itemOk = _itemPrepareSuccess(waveKey, k);
            emit SparrowWaveFinished(_waveItems[waveKey][k].txId, itemOk);
            if (!itemOk) {
                anyIncludedFail = true;
            }
        }

        if (anyIncludedFail) {
            _abortWave(waveKey);
            _cleanupWave(waveKey);
            return;
        }

        _commitWave(waveKey);
        _cleanupWave(waveKey);
    }

    function _abortWave(uint256 waveKey) private {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        if (_poolLowActive[waveKey]) {
            _emitExecNoCb(shardLow, poolLow, selfShard, abi.encodeWithSignature("abortBatch(bytes32)", _bidLow[waveKey]));
        }
        if (_poolHighActive[waveKey]) {
            _emitExecNoCb(shardHigh, poolHigh, selfShard, abi.encodeWithSignature("abortBatch(bytes32)", _bidHigh[waveKey]));
        }
        if (_flashActive[waveKey]) {
            _emitExecNoCb(shardFlash, flash, selfShard, abi.encodeWithSignature("abortBatch(bytes32)", _bidFlash[waveKey]));
        }
        if (_profitLockActive[waveKey]) {
            _emitExecNoCb(shardProfit, profit, selfShard, abi.encodeWithSignature("abortBatch(bytes32)", _bidProfit[waveKey]));
        }
    }

    function _commitWave(uint256 waveKey) private {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        if (_flashActive[waveKey]) {
            _emitExecNoCb(shardFlash, flash, selfShard, abi.encodeWithSignature("commitBatch(bytes32)", _bidFlash[waveKey]));
        }
        if (_poolLowActive[waveKey]) {
            _emitExecNoCb(shardLow, poolLow, selfShard, abi.encodeWithSignature("commitBatch(bytes32)", _bidLow[waveKey]));
        }
        if (_poolHighActive[waveKey]) {
            _emitExecNoCb(shardHigh, poolHigh, selfShard, abi.encodeWithSignature("commitBatch(bytes32)", _bidHigh[waveKey]));
        }
        if (_profitLockActive[waveKey]) {
            _emitExecNoCb(shardProfit, profit, selfShard, abi.encodeWithSignature("commitBatch(bytes32)", _bidProfit[waveKey]));
        }
    }

    function _emitExecNoCb(uint32 targetShard, address target, uint32 sourceShard, bytes memory cd) private {
        unchecked {
            _execReqId++;
        }
        uint256 rid = _execReqId;
        bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
            sourceShard, address(this), this.onCommitResponse.selector, rid, target, cd
        );
        TwoPhaseLib.emitCrossShardRequest(targetShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, exec);
    }

    function _cleanupWave(uint256 waveKey) private {
        delete _wave[waveKey];
        delete _waveItems[waveKey];
        delete _bidFlash[waveKey];
        delete _bidLow[waveKey];
        delete _bidHigh[waveKey];
        delete _bidProfit[waveKey];
        delete _waveNetProfit[waveKey];
        delete _poolLowActive[waveKey];
        delete _poolHighActive[waveKey];
        delete _flashActive[waveKey];
        delete _profitLockActive[waveKey];
        delete _lineFlash[waveKey][0];
        delete _lineLow[waveKey][0];
        delete _lineHigh[waveKey][0];
        delete _lineProfit[waveKey][0];
    }

    function onCommitResponse(uint256, bool, bytes calldata) external {}
}
