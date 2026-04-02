// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

/**
 * @title SparrowCoordinator
 * @dev 与原生 2PC / JOYUE 对照：一波内多笔 buy 按「同读写集」合并子交易。
 *      协调者向各 participant 传 **uint256[]**（每笔一行，顺序与 items 一致）；各合约 **逐行** 判定（余额/库存/积分溢出），
 *      批次结束再 emit 行级结果，并通过返回值 (allOk, lineOk[]) 经 Executor 的 returnData 回传。
 *      BuyItem.txId：Intent/Batcher 填真实 id；buyFruitWave1/2 使用 0（方案 A，不参与指标）。SparrowWaveStarted 带 txIds[]；SparrowWaveFinished(txId, committed) 每笔一条。
 *      Wallet / Points / Stock 的 prepare 在同一笔协调交易里**同时发出**；收齐回调后解码 lineOk；全 item 成功则 commit，否则 abort。
 *      使用与 TwoPhaseCoordinator 相同的 0x6D / 0x74 跨分片机制。
 */
contract SparrowCoordinator {
    address public fruitStore;
    address public wallet;
    address public points;
    uint32 public fruitStoreShardId;
    uint32 public walletShardId;
    uint32 public pointsShardId;

    uint256 private _waveNonce;
    /// @dev prepare 回调 requestId：单调递增，避免同块内 keccak+mod 碰撞导致 _reqToWave 被覆盖、pending 无法归零
    uint256 private _prepareReqId;
    /// @dev commit/abort 无回调路径仍向 Executor 传 requestId，单独递增避免与 prepare 混用
    uint256 private _execReqId;

    struct BuyItem {
        address buyer;
        bytes32 fruitType;
        uint256 quantity;
        /// @dev Intent 侧 intentId；测试入口 buyFruitWave1/2 使用 bytes32(0) 表示不参与指标
        bytes32 txId;
    }

    struct WBatch {
        bytes32 batchId;
        address buyer;
        uint256[] amounts;
    }

    struct PBatch {
        bytes32 batchId;
        address buyer;
        uint256[] ptsLines;
    }

    struct SBatch {
        bytes32 batchId;
        bytes32 fruitType;
        uint256[] qtys;
    }

    struct Wave {
        uint32 pending;
        bool anyFail;
        bool active;
    }

    struct ReqMeta {
        uint256 waveKey;
        uint8 kind; // 1=wallet 2=points 3=stock
        uint256 batchIdx;
    }

    mapping(uint256 => Wave) private _wave;
    mapping(uint256 => WBatch[]) private _waveW;
    mapping(uint256 => PBatch[]) private _waveP;
    mapping(uint256 => SBatch[]) private _waveS;
    mapping(uint256 => BuyItem[]) private _waveItems;
    mapping(uint256 => mapping(uint256 => bool[])) private _wLineOk;
    mapping(uint256 => mapping(uint256 => bool[])) private _pLineOk;
    mapping(uint256 => mapping(uint256 => bool[])) private _sLineOk;
    mapping(uint256 => ReqMeta) private _reqToWave;

    event SparrowWaveStarted(bytes32 indexed waveId, uint256 mainTxCount, bytes32[] txIds);
    /// @param txId 与 BuyItem.txId 一致（Intent intentId）；测试路径为 0
    /// @param committed 该主交易在 Wallet/Points/Stock 三端对应行 prepare 是否均成功
    event SparrowWaveFinished(bytes32 indexed txId, bool committed);

    constructor(
        address _fruitStore,
        address _wallet,
        address _points,
        uint32 _fruitStoreShardId,
        uint32 _walletShardId,
        uint32 _pointsShardId
    ) {
        fruitStore = _fruitStore;
        wallet = _wallet;
        points = _points;
        fruitStoreShardId = _fruitStoreShardId;
        walletShardId = _walletShardId;
        pointsShardId = _pointsShardId;
    }

    function buyFruitWave(BuyItem[] calldata items) external {
        BuyItem[] memory m = new BuyItem[](items.length);
        for (uint256 i = 0; i < items.length; i++) {
            m[i] = items[i];
        }
        _executeWave(m);
    }

    /// @dev 单笔主交易（便于 trigger 编码 calldata）
    function buyFruitWave1(address buyer, string calldata fruitName, uint256 quantity) external {
        BuyItem[] memory m = new BuyItem[](1);
        m[0] = BuyItem({ buyer: buyer, fruitType: keccak256(bytes(fruitName)), quantity: quantity, txId: bytes32(0) });
        _executeWave(m);
    }

    /// @dev 同一买家两笔主交易（合并同读写集子交易，典型 Sparrow 场景）
    function buyFruitWave2(
        address buyer,
        string calldata fruitName0,
        uint256 qty0,
        string calldata fruitName1,
        uint256 qty1
    ) external {
        BuyItem[] memory m = new BuyItem[](2);
        m[0] = BuyItem({ buyer: buyer, fruitType: keccak256(bytes(fruitName0)), quantity: qty0, txId: bytes32(0) });
        m[1] = BuyItem({ buyer: buyer, fruitType: keccak256(bytes(fruitName1)), quantity: qty1, txId: bytes32(0) });
        _executeWave(m);
    }

    function _executeWave(BuyItem[] memory items) internal {
        require(items.length > 0, "Sparrow: empty wave");
        uint256 waveKey = ++_waveNonce;
        _buildBatches(items, waveKey);

        uint256 nW = _waveW[waveKey].length;
        uint256 nP = _waveP[waveKey].length;
        uint256 nS = _waveS[waveKey].length;
        require(nW > 0, "Sparrow: no wallet batches");
        require(nP > 0, "Sparrow: no points batches");
        require(nS > 0, "Sparrow: no stock batches");
        uint256 total = nW + nP + nS;
        require(total <= type(uint32).max, "Sparrow: too many batches");

        Wave storage w = _wave[waveKey];
        w.active = true;
        w.anyFail = false;
        w.pending = uint32(total);

        for (uint256 ii = 0; ii < items.length; ii++) {
            _waveItems[waveKey].push(items[ii]);
        }

        bytes32[] memory txIds = new bytes32[](items.length);
        for (uint256 t = 0; t < items.length; t++) {
            txIds[t] = items[t].txId;
        }
        emit SparrowWaveStarted(bytes32(waveKey), items.length, txIds);
        _emitWalletPrepares(waveKey);
        _emitPointsPrepares(waveKey);
        _emitStockPrepares(waveKey);
    }

    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata returnData) external {
        ReqMeta memory meta = _reqToWave[requestId];
        delete _reqToWave[requestId];
        require(meta.waveKey != 0, "Sparrow: bad request");

        uint256 waveKey = meta.waveKey;
        Wave storage w = _wave[waveKey];
        require(w.active, "Sparrow: inactive wave");

        if (!ok) {
            w.anyFail = true;
        } else {
            (, bool[] memory lines) = _decodePrepareReturn(returnData);
            if (!_storePrepareLines(waveKey, meta.kind, meta.batchIdx, lines)) {
                w.anyFail = true;
            }
        }
        require(w.pending > 0, "Sparrow: pending underflow");
        w.pending--;

        if (w.pending != 0) {
            return;
        }

        _finalizeWave(waveKey);
    }

    function onCommitResponse(uint256, bool, bytes calldata) external {}

    function _decodePrepareReturn(bytes calldata data) private pure returns (bool allOk, bool[] memory lineOk) {
        return abi.decode(data, (bool, bool[]));
    }

    function _expectedLineCount(uint256 waveKey, uint8 kind, uint256 batchIdx) private view returns (uint256) {
        if (kind == 1) {
            return _waveW[waveKey][batchIdx].amounts.length;
        }
        if (kind == 2) {
            return _waveP[waveKey][batchIdx].ptsLines.length;
        }
        return _waveS[waveKey][batchIdx].qtys.length;
    }

    function _storePrepareLines(uint256 waveKey, uint8 kind, uint256 batchIdx, bool[] memory lines) private returns (bool) {
        uint256 exp = _expectedLineCount(waveKey, kind, batchIdx);
        if (lines.length != exp) {
            return false;
        }
        if (kind == 1) {
            delete _wLineOk[waveKey][batchIdx];
            for (uint256 i = 0; i < lines.length; i++) {
                _wLineOk[waveKey][batchIdx].push(lines[i]);
            }
        } else if (kind == 2) {
            delete _pLineOk[waveKey][batchIdx];
            for (uint256 i = 0; i < lines.length; i++) {
                _pLineOk[waveKey][batchIdx].push(lines[i]);
            }
        } else {
            delete _sLineOk[waveKey][batchIdx];
            for (uint256 i = 0; i < lines.length; i++) {
                _sLineOk[waveKey][batchIdx].push(lines[i]);
            }
        }
        return true;
    }

    function _finalizeWave(uint256 waveKey) private {
        uint256 n = _waveItems[waveKey].length;
        Wave storage w = _wave[waveKey];

        if (w.anyFail) {
            _emitAbortWave(waveKey);
            for (uint256 k = 0; k < n; k++) {
                emit SparrowWaveFinished(_waveItems[waveKey][k].txId, false);
            }
            _cleanupWave(waveKey);
            return;
        }

        bool allItemsOk = true;
        for (uint256 k = 0; k < n; k++) {
            bool itemOk = _itemPrepareSuccess(waveKey, k);
            emit SparrowWaveFinished(_waveItems[waveKey][k].txId, itemOk);
            if (!itemOk) {
                allItemsOk = false;
            }
        }

        if (!allItemsOk) {
            _emitAbortWave(waveKey);
        } else {
            _emitCommitWave(waveKey);
        }
        _cleanupWave(waveKey);
    }

    function _findWalletBatchIndex(uint256 waveKey, address buyer) private view returns (uint256) {
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            if (wb[i].buyer == buyer) {
                return i;
            }
        }
        revert("Sparrow: w batch");
    }

    function _findStockBatchIndex(uint256 waveKey, bytes32 fruitType) private view returns (uint256) {
        SBatch[] storage sb = _waveS[waveKey];
        for (uint256 i = 0; i < sb.length; i++) {
            if (sb[i].fruitType == fruitType) {
                return i;
            }
        }
        revert("Sparrow: s batch");
    }

    function _countSameBuyerBefore(uint256 waveKey, uint256 itemIdx, address buyer) private view returns (uint256 c) {
        BuyItem[] storage items = _waveItems[waveKey];
        for (uint256 j = 0; j < itemIdx; j++) {
            if (items[j].buyer == buyer) {
                c++;
            }
        }
    }

    function _countSameFruitBefore(uint256 waveKey, uint256 itemIdx, bytes32 fruitType) private view returns (uint256 c) {
        BuyItem[] storage items = _waveItems[waveKey];
        for (uint256 j = 0; j < itemIdx; j++) {
            if (items[j].fruitType == fruitType) {
                c++;
            }
        }
    }

    function _itemPrepareSuccess(uint256 waveKey, uint256 itemIdx) private view returns (bool) {
        BuyItem storage it = _waveItems[waveKey][itemIdx];
        uint256 wi = _findWalletBatchIndex(waveKey, it.buyer);
        uint256 wLine = _countSameBuyerBefore(waveKey, itemIdx, it.buyer);
        if (_wLineOk[waveKey][wi].length <= wLine || !_wLineOk[waveKey][wi][wLine]) {
            return false;
        }
        if (_pLineOk[waveKey][wi].length <= wLine || !_pLineOk[waveKey][wi][wLine]) {
            return false;
        }
        uint256 si = _findStockBatchIndex(waveKey, it.fruitType);
        uint256 sLine = _countSameFruitBefore(waveKey, itemIdx, it.fruitType);
        if (_sLineOk[waveKey][si].length <= sLine || !_sLineOk[waveKey][si][sLine]) {
            return false;
        }
        return true;
    }

    function _cleanupWave(uint256 waveKey) internal {
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            delete _wLineOk[waveKey][i];
        }
        PBatch[] storage pb = _waveP[waveKey];
        for (uint256 i = 0; i < pb.length; i++) {
            delete _pLineOk[waveKey][i];
        }
        SBatch[] storage sb = _waveS[waveKey];
        for (uint256 i = 0; i < sb.length; i++) {
            delete _sLineOk[waveKey][i];
        }
        delete _wave[waveKey];
        delete _waveItems[waveKey];
        delete _waveW[waveKey];
        delete _waveP[waveKey];
        delete _waveS[waveKey];
    }

    function _buildBatches(BuyItem[] memory items, uint256 waveKey) internal {
        _buildWalletAndPointsBatches(items, waveKey);
        _buildStockBatches(items, waveKey);
    }

    function _buildWalletAndPointsBatches(BuyItem[] memory items, uint256 waveKey) internal {
        uint256 n = items.length;
        address[] memory ub = new address[](n);
        uint256 nub = 0;
        for (uint256 i = 0; i < n; i++) {
            address b = items[i].buyer;
            bool seen = false;
            for (uint256 j = 0; j < nub; j++) {
                if (ub[j] == b) {
                    seen = true;
                    break;
                }
            }
            if (!seen) {
                ub[nub++] = b;
            }
        }
        for (uint256 i = 0; i < nub; i++) {
            _pushOneBuyerWalletPoints(items, n, waveKey, ub[i]);
        }
    }

    function _pushOneBuyerWalletPoints(BuyItem[] memory items, uint256 n, uint256 waveKey, address buyer) internal {
        uint256 cnt = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].buyer == buyer) {
                cnt++;
            }
        }
        uint256[] memory lineAmt = new uint256[](cnt);
        uint256[] memory linePts = new uint256[](cnt);
        uint256 ix = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].buyer == buyer) {
                lineAmt[ix] = _fruitPrice(items[k].fruitType) * items[k].quantity;
                linePts[ix] = _fruitPointsPer(items[k].fruitType) * items[k].quantity;
                unchecked {
                    ix++;
                }
            }
        }
        bytes32 wid = keccak256(abi.encodePacked(waveKey, uint8(1), buyer));
        bytes32 pid = keccak256(abi.encodePacked(waveKey, uint8(2), buyer));
        uint256 wi = _waveW[waveKey].length;
        _waveW[waveKey].push();
        _waveW[waveKey][wi].batchId = wid;
        _waveW[waveKey][wi].buyer = buyer;
        for (uint256 j = 0; j < lineAmt.length; j++) {
            _waveW[waveKey][wi].amounts.push(lineAmt[j]);
        }
        uint256 pi = _waveP[waveKey].length;
        _waveP[waveKey].push();
        _waveP[waveKey][pi].batchId = pid;
        _waveP[waveKey][pi].buyer = buyer;
        for (uint256 j = 0; j < linePts.length; j++) {
            _waveP[waveKey][pi].ptsLines.push(linePts[j]);
        }
    }

    function _buildStockBatches(BuyItem[] memory items, uint256 waveKey) internal {
        uint256 n = items.length;
        bytes32[] memory uf = new bytes32[](n);
        uint256 nuf = 0;
        for (uint256 i = 0; i < n; i++) {
            bytes32 f = items[i].fruitType;
            bool seen = false;
            for (uint256 j = 0; j < nuf; j++) {
                if (uf[j] == f) {
                    seen = true;
                    break;
                }
            }
            if (!seen) {
                uf[nuf++] = f;
            }
        }
        for (uint256 i = 0; i < nuf; i++) {
            _pushOneFruitStock(items, n, waveKey, uf[i]);
        }
    }

    function _pushOneFruitStock(BuyItem[] memory items, uint256 n, uint256 waveKey, bytes32 ft) internal {
        uint256 cnt = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].fruitType == ft) {
                cnt++;
            }
        }
        uint256[] memory lineQty = new uint256[](cnt);
        uint256 ix = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].fruitType == ft) {
                lineQty[ix] = items[k].quantity;
                unchecked {
                    ix++;
                }
            }
        }
        bytes32 sid = keccak256(abi.encodePacked(waveKey, uint8(3), ft));
        uint256 si = _waveS[waveKey].length;
        _waveS[waveKey].push();
        _waveS[waveKey][si].batchId = sid;
        _waveS[waveKey][si].fruitType = ft;
        for (uint256 j = 0; j < lineQty.length; j++) {
            _waveS[waveKey][si].qtys.push(lineQty[j]);
        }
    }

    function _fruitPrice(bytes32 fruitType) internal view returns (uint256) {
        (bool ok, bytes memory ret) = fruitStore.staticcall(abi.encodeWithSignature("getPrice(bytes32)", fruitType));
        require(ok && ret.length >= 32, "Sparrow: getPrice");
        return abi.decode(ret, (uint256));
    }

    function _fruitPointsPer(bytes32 fruitType) internal view returns (uint256) {
        (bool ok, bytes memory ret) = fruitStore.staticcall(abi.encodeWithSignature("getPointsPer(bytes32)", fruitType));
        require(ok && ret.length >= 32, "Sparrow: getPointsPer");
        return abi.decode(ret, (uint256));
    }

    function _emitWalletPrepares(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            bytes memory cd = abi.encodeWithSignature("prepareBatch(bytes32,address,uint256[])", wb[i].batchId, wb[i].buyer, wb[i].amounts);
            _emitPrepare(waveKey, walletShardId, wallet, selfShard, cd, uint8(1), i);
        }
    }

    function _emitPointsPrepares(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        PBatch[] storage pb = _waveP[waveKey];
        for (uint256 i = 0; i < pb.length; i++) {
            bytes memory cd = abi.encodeWithSignature("prepareBatch(bytes32,address,uint256[])", pb[i].batchId, pb[i].buyer, pb[i].ptsLines);
            _emitPrepare(waveKey, pointsShardId, points, selfShard, cd, uint8(2), i);
        }
    }

    function _emitStockPrepares(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        SBatch[] storage sb = _waveS[waveKey];
        for (uint256 i = 0; i < sb.length; i++) {
            bytes memory cd =
                abi.encodeWithSignature("prepareBatch(bytes32,bytes32,uint256[])", sb[i].batchId, sb[i].fruitType, sb[i].qtys);
            _emitPrepare(waveKey, fruitStoreShardId, fruitStore, selfShard, cd, uint8(3), i);
        }
    }

    function _emitPrepare(
        uint256 waveKey,
        uint32 targetShardId,
        address targetAddr,
        uint32 sourceShardId,
        bytes memory targetCalldata,
        uint8 kind,
        uint256 batchIdx
    ) internal {
        uint256 requestId = ++_prepareReqId;
        _reqToWave[requestId] = ReqMeta({ waveKey: waveKey, kind: kind, batchIdx: batchIdx });

        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onPrepareResponse.selector,
            requestId,
            targetAddr,
            targetCalldata
        );

        require(TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata), "Sparrow: prepare emit");
    }

    function _emitAbortWave(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes4 selAbort = bytes4(keccak256("abortBatch(bytes32)"));
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            _emitExecNoCallback(waveKey, walletShardId, wallet, selfShard, abi.encodeWithSelector(selAbort, wb[i].batchId));
        }
        PBatch[] storage pb = _waveP[waveKey];
        for (uint256 i = 0; i < pb.length; i++) {
            _emitExecNoCallback(waveKey, pointsShardId, points, selfShard, abi.encodeWithSelector(selAbort, pb[i].batchId));
        }
        SBatch[] storage sb = _waveS[waveKey];
        for (uint256 i = 0; i < sb.length; i++) {
            _emitExecNoCallback(waveKey, fruitStoreShardId, fruitStore, selfShard, abi.encodeWithSelector(selAbort, sb[i].batchId));
        }
    }

    function _emitCommitWave(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes4 selCommit = bytes4(keccak256("commitBatch(bytes32)"));
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            _emitExecNoCallback(waveKey, walletShardId, wallet, selfShard, abi.encodeWithSelector(selCommit, wb[i].batchId));
        }
        PBatch[] storage pb = _waveP[waveKey];
        for (uint256 i = 0; i < pb.length; i++) {
            _emitExecNoCallback(waveKey, pointsShardId, points, selfShard, abi.encodeWithSelector(selCommit, pb[i].batchId));
        }
        SBatch[] storage sb = _waveS[waveKey];
        for (uint256 i = 0; i < sb.length; i++) {
            _emitExecNoCallback(waveKey, fruitStoreShardId, fruitStore, selfShard, abi.encodeWithSelector(selCommit, sb[i].batchId));
        }
    }

    function _emitExecNoCallback(
        uint256 waveKey,
        uint32 targetShardId,
        address targetAddr,
        uint32 sourceShardId,
        bytes memory targetCalldata
    ) internal {
        uint256 requestId = ++_execReqId;
        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            requestId,
            targetAddr,
            targetCalldata
        );
        require(
            TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata),
            "Sparrow: exec emit"
        );
    }
}
