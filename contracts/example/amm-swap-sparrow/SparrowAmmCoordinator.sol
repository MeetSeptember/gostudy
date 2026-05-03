// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";
import "./SparrowAmmPool.sol";

/**
 * @title SparrowAmmCoordinator
 * @dev 与 `wallet-transfer-sparrow` 一致：Pool / WalletA / WalletB 的 prepare **并行 emit**（跨分片预编译），
 *      `pending` 归零后 `onPrepareResponse` 聚合；全成则 commit，否则 abort。
 *      Pool 包仅含 `batchId + newReserveA/B[] + clientVersion[]`（顺序即 index）；协调者用 **view** `quoteSwap` 组包，**不**在协调者判版本。
 *      Wallet 按波内 **user** 组批（`batchId` 含 waveKey+user），**所有 item** 均进入对应批；finalize 时 **每条 item** 须 pool+debit+credit 行全 true，否则整波 abort。
 */
contract SparrowAmmCoordinator {
    address public pool;
    uint32 public poolShardId;
    address public walletA;
    address public walletB;
    uint32 public walletAShardId;
    uint32 public walletBShardId;

    uint256 private _waveNonce;
    uint256 private _prepareReqId;
    uint256 private _execReqId;

    struct SwapItem {
        address user;
        uint256 amountIn;
        uint256 minOut;
        uint256 clientVersion;
        bytes32 txId;
    }

    struct DBatch {
        bytes32 batchId;
        address fromUser;
        uint256[] amounts;
    }

    struct CBatch {
        bytes32 batchId;
        address toUser;
        uint256[] amounts;
    }

    struct Wave {
        uint32 pending;
        bool anyFail;
        bool active;
    }

    /// @dev kind: 1=debit(walletA) 2=credit(walletB) 3=pool
    struct ReqMeta {
        uint256 waveKey;
        uint8 kind;
        uint256 batchIdx;
    }

    mapping(uint256 => Wave) private _wave;
    mapping(uint256 => DBatch[]) private _waveD;
    mapping(uint256 => CBatch[]) private _waveC;
    mapping(uint256 => SwapItem[]) private _waveItems;
    mapping(uint256 => mapping(uint256 => bool[])) private _dLineOk;
    mapping(uint256 => mapping(uint256 => bool[])) private _cLineOk;
    mapping(uint256 => ReqMeta) private _reqToWave;
    mapping(uint256 => bytes32) private _wavePoolBatchId;
    /// @dev Pool 侧已成功 prepare（锁池）时置 true，abort/commit 需带 pool
    mapping(uint256 => bool) private _poolBatchActive;
    mapping(uint256 => bool[]) private _poolLineOk;

    event SparrowWaveStarted(bytes32 indexed waveId, uint256 mainTxCount, bytes32[] txIds);
    event SparrowWaveFinished(bytes32 indexed txId, bool committed);

    constructor(
        address pool_,
        address walletA_,
        address walletB_,
        uint32 poolShardId_,
        uint32 walletAShardId_,
        uint32 walletBShardId_
    ) {
        require(pool_ != address(0), "SAmm: pool");
        require(walletA_ != address(0) && walletB_ != address(0), "SAmm: addr");
        require(walletA_ != walletB_, "SAmm: same wallet addr");
        pool = pool_;
        walletA = walletA_;
        walletB = walletB_;
        poolShardId = poolShardId_;
        walletAShardId = walletAShardId_;
        walletBShardId = walletBShardId_;
    }

    function swapWave(SwapItem[] calldata items) external {
        SwapItem[] memory m = new SwapItem[](items.length);
        for (uint256 i = 0; i < items.length; i++) {
            m[i] = items[i];
        }
        _executeWave(m);
    }

    function swapWave1(address user, uint256 amountIn, uint256 minOut, uint256 clientVersion) external {
        SwapItem[] memory m = new SwapItem[](1);
        m[0] = SwapItem({user: user, amountIn: amountIn, minOut: minOut, clientVersion: clientVersion, txId: bytes32(0)});
        _executeWave(m);
    }

    /// @dev 仅 view 组 Pool 的 `newReserve*` 与 Wallet 的 `outs`；不在此判版本。
    function _quoteWaveForPoolPack(SparrowAmmPool P, SwapItem[] memory items)
        private
        view
        returns (
            bool allOk,
            uint256[] memory newRAs,
            uint256[] memory newRBs,
            uint256[] memory clientVs,
            uint256[] memory outs
        )
    {
        uint256 n = items.length;
        newRAs = new uint256[](n);
        newRBs = new uint256[](n);
        clientVs = new uint256[](n);
        outs = new uint256[](n);
        for (uint256 qi = 0; qi < n; qi++) {
            SwapItem memory it = items[qi];
            (bool qOk, uint256 outAmt, uint256 nra, uint256 nrb, ) = P.quoteSwap(it.amountIn, it.minOut, it.user);
            if (!qOk) {
                return (false, newRAs, newRBs, clientVs, outs);
            }
            newRAs[qi] = nra;
            newRBs[qi] = nrb;
            clientVs[qi] = it.clientVersion;
            outs[qi] = outAmt;
        }
        allOk = true;
    }

    function _executeWave(SwapItem[] memory items) internal {
        require(items.length > 0, "SAmm: empty wave");
        for (uint256 i = 0; i < items.length; i++) {
            require(items[i].amountIn > 0, "SAmm: amountIn");
            require(items[i].user != address(0), "SAmm: user");
        }

        uint256 waveKey = ++_waveNonce;
        uint256 n = items.length;
        for (uint256 ii = 0; ii < n; ii++) {
            _waveItems[waveKey].push(items[ii]);
        }

        Wave storage w = _wave[waveKey];
        w.active = true;
        w.anyFail = false;

        bytes32[] memory txIds = new bytes32[](n);
        for (uint256 t = 0; t < n; t++) {
            txIds[t] = items[t].txId;
        }
        emit SparrowWaveStarted(bytes32(waveKey), n, txIds);

        (bool quotesOk, uint256[] memory newRAs, uint256[] memory newRBs, uint256[] memory clientVs, uint256[] memory outs) =
            _quoteWaveForPoolPack(SparrowAmmPool(pool), items);
        if (!quotesOk) {
            w.active = false;
            for (uint256 k = 0; k < n; k++) {
                emit SparrowWaveFinished(_waveItems[waveKey][k].txId, false);
            }
            _cleanupWave(waveKey);
            return;
        }

        bytes32 poolBatchId = keccak256(abi.encodePacked(waveKey, uint8(11)));
        _wavePoolBatchId[waveKey] = poolBatchId;

        _buildDebitBatchesByUser(waveKey);
        _buildCreditBatchesByUser(waveKey, outs);

        uint256 nD = _waveD[waveKey].length;
        uint256 nC = _waveC[waveKey].length;
        require(nD > 0 && nC > 0, "SAmm: batches");
        uint256 total = uint256(1) + nD + nC;
        require(total <= type(uint32).max, "SAmm: too many batches");

        w.pending = uint32(total);

        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        _emitPoolPrepare(waveKey, poolBatchId, newRAs, newRBs, clientVs, selfShard);
        _emitDebitPrepares(waveKey);
        _emitCreditPrepares(waveKey);
    }

    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata returnData) external {
        ReqMeta memory meta = _reqToWave[requestId];
        delete _reqToWave[requestId];
        require(meta.waveKey != 0, "SAmm: bad request");

        uint256 waveKey = meta.waveKey;
        Wave storage w = _wave[waveKey];
        require(w.active, "SAmm: inactive wave");

        if (!ok) {
            w.anyFail = true;
        } else if (meta.kind == 3) {
            (bool batchPrepared, bool[] memory lines) = abi.decode(returnData, (bool, bool[]));
            uint256 exp = _waveItems[waveKey].length;
            if (lines.length != exp) {
                w.anyFail = true;
                if (batchPrepared) {
                    _poolBatchActive[waveKey] = true;
                }
            } else {
                delete _poolLineOk[waveKey];
                for (uint256 i = 0; i < lines.length; i++) {
                    _poolLineOk[waveKey].push(lines[i]);
                }
                if (!batchPrepared) {
                    w.anyFail = true;
                } else {
                    _poolBatchActive[waveKey] = true;
                }
            }
        } else {
            (, bool[] memory lines) = _decodePrepareReturn(returnData);
            if (!_storePrepareLines(waveKey, meta.kind, meta.batchIdx, lines)) {
                w.anyFail = true;
            }
        }
        require(w.pending > 0, "SAmm: pending underflow");
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
            return _waveD[waveKey][batchIdx].amounts.length;
        }
        return _waveC[waveKey][batchIdx].amounts.length;
    }

    function _storePrepareLines(uint256 waveKey, uint8 kind, uint256 batchIdx, bool[] memory lines) private returns (bool) {
        uint256 exp = _expectedLineCount(waveKey, kind, batchIdx);
        if (lines.length != exp) {
            return false;
        }
        if (kind == 1) {
            delete _dLineOk[waveKey][batchIdx];
            for (uint256 i = 0; i < lines.length; i++) {
                _dLineOk[waveKey][batchIdx].push(lines[i]);
            }
        } else {
            delete _cLineOk[waveKey][batchIdx];
            for (uint256 i = 0; i < lines.length; i++) {
                _cLineOk[waveKey][batchIdx].push(lines[i]);
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

        bool anyIncludedFail = false;
        for (uint256 k = 0; k < n; k++) {
            bool itemOk = _itemPrepareSuccess(waveKey, k);
            emit SparrowWaveFinished(_waveItems[waveKey][k].txId, itemOk);
            if (!itemOk) {
                anyIncludedFail = true;
            }
        }

        if (anyIncludedFail) {
            _emitAbortWave(waveKey);
        } else {
            _emitCommitWave(waveKey);
        }
        _cleanupWave(waveKey);
    }

    function _findDebitBatchIndex(uint256 waveKey, address fromUser) private view returns (uint256) {
        DBatch[] storage db = _waveD[waveKey];
        for (uint256 i = 0; i < db.length; i++) {
            if (db[i].fromUser == fromUser) {
                return i;
            }
        }
        revert("SAmm: d batch");
    }

    function _findCreditBatchIndex(uint256 waveKey, address toUser) private view returns (uint256) {
        CBatch[] storage cb = _waveC[waveKey];
        for (uint256 i = 0; i < cb.length; i++) {
            if (cb[i].toUser == toUser) {
                return i;
            }
        }
        revert("SAmm: c batch");
    }

    function _countSameUserBefore(uint256 waveKey, uint256 itemIdx, address user) private view returns (uint256 c) {
        SwapItem[] storage items = _waveItems[waveKey];
        for (uint256 j = 0; j < itemIdx; j++) {
            if (items[j].user == user) {
                c++;
            }
        }
    }

    function _itemPrepareSuccess(uint256 waveKey, uint256 itemIdx) private view returns (bool) {
        if (_poolLineOk[waveKey].length <= itemIdx || !_poolLineOk[waveKey][itemIdx]) {
            return false;
        }
        SwapItem storage it = _waveItems[waveKey][itemIdx];
        uint256 di = _findDebitBatchIndex(waveKey, it.user);
        uint256 dLine = _countSameUserBefore(waveKey, itemIdx, it.user);
        if (_dLineOk[waveKey][di].length <= dLine || !_dLineOk[waveKey][di][dLine]) {
            return false;
        }
        uint256 ci = _findCreditBatchIndex(waveKey, it.user);
        uint256 cLine = _countSameUserBefore(waveKey, itemIdx, it.user);
        if (_cLineOk[waveKey][ci].length <= cLine || !_cLineOk[waveKey][ci][cLine]) {
            return false;
        }
        return true;
    }

    function _cleanupWave(uint256 waveKey) private {
        DBatch[] storage db = _waveD[waveKey];
        for (uint256 i = 0; i < db.length; i++) {
            delete _dLineOk[waveKey][i];
        }
        CBatch[] storage cb = _waveC[waveKey];
        for (uint256 i = 0; i < cb.length; i++) {
            delete _cLineOk[waveKey][i];
        }
        delete _wave[waveKey];
        delete _waveItems[waveKey];
        delete _waveD[waveKey];
        delete _waveC[waveKey];
        delete _wavePoolBatchId[waveKey];
        delete _poolBatchActive[waveKey];
        delete _poolLineOk[waveKey];
    }

    function _buildDebitBatchesByUser(uint256 waveKey) internal {
        SwapItem[] storage items = _waveItems[waveKey];
        uint256 n = items.length;
        address[] memory ufs = new address[](n);
        uint256 nu = 0;
        for (uint256 i = 0; i < n; i++) {
            address u = items[i].user;
            bool seen = false;
            for (uint256 j = 0; j < nu; j++) {
                if (ufs[j] == u) {
                    seen = true;
                    break;
                }
            }
            if (!seen) {
                ufs[nu++] = u;
            }
        }
        for (uint256 i = 0; i < nu; i++) {
            _pushOneDebitBatchForUser(waveKey, ufs[i]);
        }
    }

    function _buildCreditBatchesByUser(uint256 waveKey, uint256[] memory poolOuts) internal {
        SwapItem[] storage items = _waveItems[waveKey];
        uint256 n = items.length;
        address[] memory uts = new address[](n);
        uint256 nv = 0;
        for (uint256 i = 0; i < n; i++) {
            address u = items[i].user;
            bool seen = false;
            for (uint256 j = 0; j < nv; j++) {
                if (uts[j] == u) {
                    seen = true;
                    break;
                }
            }
            if (!seen) {
                uts[nv++] = u;
            }
        }
        for (uint256 i = 0; i < nv; i++) {
            _pushOneCreditBatchForUser(waveKey, n, uts[i], poolOuts);
        }
    }

    function _pushOneDebitBatchForUser(uint256 waveKey, address fromUser) internal {
        SwapItem[] storage items = _waveItems[waveKey];
        uint256 n = items.length;
        uint256 cnt = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].user == fromUser) {
                cnt++;
            }
        }
        uint256[] memory amts = new uint256[](cnt);
        uint256 ix = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].user == fromUser) {
                amts[ix] = items[k].amountIn;
                unchecked {
                    ix++;
                }
            }
        }
        bytes32 bid = keccak256(abi.encodePacked(waveKey, uint8(1), fromUser));
        uint256 di = _waveD[waveKey].length;
        _waveD[waveKey].push();
        _waveD[waveKey][di].batchId = bid;
        _waveD[waveKey][di].fromUser = fromUser;
        for (uint256 j = 0; j < amts.length; j++) {
            _waveD[waveKey][di].amounts.push(amts[j]);
        }
    }

    function _pushOneCreditBatchForUser(uint256 waveKey, uint256 n, address toUser, uint256[] memory poolOuts) internal {
        SwapItem[] storage items = _waveItems[waveKey];
        uint256 cnt = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].user == toUser) {
                cnt++;
            }
        }
        uint256[] memory amts = new uint256[](cnt);
        uint256 ix = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].user == toUser) {
                amts[ix] = poolOuts[k];
                unchecked {
                    ix++;
                }
            }
        }
        bytes32 bid = keccak256(abi.encodePacked(waveKey, uint8(2), toUser));
        uint256 ci = _waveC[waveKey].length;
        _waveC[waveKey].push();
        _waveC[waveKey][ci].batchId = bid;
        _waveC[waveKey][ci].toUser = toUser;
        for (uint256 j = 0; j < amts.length; j++) {
            _waveC[waveKey][ci].amounts.push(amts[j]);
        }
    }

    function _emitPoolPrepare(
        uint256 waveKey,
        bytes32 poolBatchId,
        uint256[] memory newRAs,
        uint256[] memory newRBs,
        uint256[] memory clientVs,
        uint32 selfShard
    ) internal {
        bytes memory cd = abi.encodeWithSignature(
            "prepareReadWriteBatch(bytes32,uint256[],uint256[],uint256[])",
            poolBatchId,
            newRAs,
            newRBs,
            clientVs
        );
        _emitPrepare(waveKey, poolShardId, pool, selfShard, cd, uint8(3), 0);
    }

    function _emitDebitPrepares(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        DBatch[] storage db = _waveD[waveKey];
        for (uint256 i = 0; i < db.length; i++) {
            bytes memory cd = abi.encodeWithSignature(
                "prepareBatch(bytes32,address,uint256[])",
                db[i].batchId,
                db[i].fromUser,
                db[i].amounts
            );
            _emitPrepare(waveKey, walletAShardId, walletA, selfShard, cd, uint8(1), i);
        }
    }

    function _emitCreditPrepares(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        CBatch[] storage cb = _waveC[waveKey];
        for (uint256 i = 0; i < cb.length; i++) {
            bytes memory cd = abi.encodeWithSignature(
                "prepareCreditBatch(bytes32,address,uint256[])",
                cb[i].batchId,
                cb[i].toUser,
                cb[i].amounts
            );
            _emitPrepare(waveKey, walletBShardId, walletB, selfShard, cd, uint8(2), i);
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
        _reqToWave[requestId] = ReqMeta({waveKey: waveKey, kind: kind, batchIdx: batchIdx});

        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onPrepareResponse.selector,
            requestId,
            targetAddr,
            targetCalldata
        );

        require(TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata), "SAmm: prepare emit");
    }

    function _emitAbortWave(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes4 selAbort = bytes4(keccak256("abortBatch(bytes32)"));

        if (_poolBatchActive[waveKey]) {
            bytes32 pid = _wavePoolBatchId[waveKey];
            _emitExecNoCallback(poolShardId, pool, selfShard, abi.encodeWithSelector(selAbort, pid));
        }

        DBatch[] storage db = _waveD[waveKey];
        for (uint256 i = 0; i < db.length; i++) {
            _emitExecNoCallback(walletAShardId, walletA, selfShard, abi.encodeWithSelector(selAbort, db[i].batchId));
        }
        CBatch[] storage cb = _waveC[waveKey];
        for (uint256 i = 0; i < cb.length; i++) {
            _emitExecNoCallback(walletBShardId, walletB, selfShard, abi.encodeWithSelector(selAbort, cb[i].batchId));
        }
    }

    function _emitCommitWave(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes4 selCommit = bytes4(keccak256("commitBatch(bytes32)"));

        if (_poolBatchActive[waveKey]) {
            bytes32 pid = _wavePoolBatchId[waveKey];
            _emitExecNoCallback(poolShardId, pool, selfShard, abi.encodeWithSelector(selCommit, pid));
        }

        DBatch[] storage db = _waveD[waveKey];
        for (uint256 i = 0; i < db.length; i++) {
            _emitExecNoCallback(walletAShardId, walletA, selfShard, abi.encodeWithSelector(selCommit, db[i].batchId));
        }
        CBatch[] storage cb = _waveC[waveKey];
        for (uint256 i = 0; i < cb.length; i++) {
            _emitExecNoCallback(walletBShardId, walletB, selfShard, abi.encodeWithSelector(selCommit, cb[i].batchId));
        }
    }

    function _emitExecNoCallback(uint32 targetShardId, address targetAddr, uint32 sourceShardId, bytes memory targetCalldata) internal {
        uint256 requestId = ++_execReqId;
        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            sourceShardId,
            address(this),
            this.onCommitResponse.selector,
            requestId,
            targetAddr,
            targetCalldata
        );
        require(TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata), "SAmm: exec emit");
    }
}
