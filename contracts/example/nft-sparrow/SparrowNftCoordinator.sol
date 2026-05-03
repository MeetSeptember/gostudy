// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

/**
 * @title SparrowNftCoordinator
 * @dev 与 `SparrowCoordinator` 同编排，仅 **Store + Wallet** 两 Participant（无 Points）。
 *      事件 `SparrowWaveStarted` / `SparrowWaveFinished` 签名与水果/Amm Sparrow 一致，链下 `sparrow-metrics` / joyue-trigger 无需改 topic。
 *      波内 `buyer` 由链下意图与 `SparrowNftWallet` bootstrap 地址对齐。
 */
contract SparrowNftCoordinator {
    address public nftStore;
    address public wallet;
    uint32 public nftStoreShardId;
    uint32 public walletShardId;

    uint256 private _waveNonce;
    uint256 private _prepareReqId;
    uint256 private _execReqId;

    struct NftItem {
        address buyer;
        uint256 quantity;
        bytes32 txId;
    }

    struct WBatch {
        bytes32 batchId;
        address buyer;
        uint256[] amounts;
    }

    struct SBatch {
        bytes32 batchId;
        uint256[] qtys;
    }

    struct Wave {
        uint32 pending;
        bool anyFail;
        bool active;
    }

    /// @dev kind: 1=wallet 2=store
    struct ReqMeta {
        uint256 waveKey;
        uint8 kind;
        uint256 batchIdx;
    }

    mapping(uint256 => Wave) private _wave;
    mapping(uint256 => WBatch[]) private _waveW;
    mapping(uint256 => SBatch[]) private _waveS;
    mapping(uint256 => NftItem[]) private _waveItems;
    mapping(uint256 => mapping(uint256 => bool[])) private _wLineOk;
    mapping(uint256 => mapping(uint256 => bool[])) private _sLineOk;
    mapping(uint256 => ReqMeta) private _reqToWave;

    event SparrowWaveStarted(bytes32 indexed waveId, uint256 mainTxCount, bytes32[] txIds);
    event SparrowWaveFinished(bytes32 indexed txId, bool committed);

    constructor(address _nftStore, address _wallet, uint32 _nftStoreShardId, uint32 _walletShardId) {
        require(_nftStore != address(0) && _wallet != address(0), "SNftC: addr");
        nftStore = _nftStore;
        wallet = _wallet;
        nftStoreShardId = _nftStoreShardId;
        walletShardId = _walletShardId;
    }

    function buyNftWave(NftItem[] calldata items) external {
        NftItem[] memory m = new NftItem[](items.length);
        for (uint256 i = 0; i < items.length; i++) {
            m[i] = items[i];
        }
        _executeWave(m);
    }

    /// @dev 单笔主交易（txId=0，不参与指标）
    function buyNftWave1(address buyer, uint256 quantity) external {
        NftItem[] memory m = new NftItem[](1);
        m[0] = NftItem({buyer: buyer, quantity: quantity, txId: bytes32(0)});
        _executeWave(m);
    }

    /// @dev 同一买家两笔主交易（典型 Sparrow 合并）
    function buyNftWave2(address buyer, uint256 qty0, uint256 qty1) external {
        NftItem[] memory m = new NftItem[](2);
        m[0] = NftItem({buyer: buyer, quantity: qty0, txId: bytes32(0)});
        m[1] = NftItem({buyer: buyer, quantity: qty1, txId: bytes32(0)});
        _executeWave(m);
    }

    function _executeWave(NftItem[] memory items) internal {
        require(items.length > 0, "SNftC: empty wave");
        for (uint256 i = 0; i < items.length; i++) {
            require(items[i].buyer != address(0), "SNftC: zero buyer");
        }
        uint256 waveKey = ++_waveNonce;
        _buildBatches(items, waveKey);

        uint256 nW = _waveW[waveKey].length;
        uint256 nS = _waveS[waveKey].length;
        require(nW > 0, "SNftC: no wallet batches");
        require(nS == 1, "SNftC: store batch");
        uint256 total = nW + nS;
        require(total <= type(uint32).max, "SNftC: too many batches");

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
        _emitStorePrepares(waveKey);
    }

    function onPrepareResponse(uint256 requestId, bool ok, bytes calldata returnData) external {
        ReqMeta memory meta = _reqToWave[requestId];
        delete _reqToWave[requestId];
        require(meta.waveKey != 0, "SNftC: bad request");

        uint256 waveKey = meta.waveKey;
        Wave storage w = _wave[waveKey];
        require(w.active, "SNftC: inactive wave");

        if (!ok) {
            w.anyFail = true;
        } else {
            (, bool[] memory lines) = _decodePrepareReturn(returnData);
            if (!_storePrepareLines(waveKey, meta.kind, meta.batchIdx, lines)) {
                w.anyFail = true;
            }
        }
        require(w.pending > 0, "SNftC: pending underflow");
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
        revert("SNftC: w batch");
    }

    function _countSameBuyerBefore(uint256 waveKey, uint256 itemIdx, address buyer) private view returns (uint256 c) {
        NftItem[] storage items = _waveItems[waveKey];
        for (uint256 j = 0; j < itemIdx; j++) {
            if (items[j].buyer == buyer) {
                c++;
            }
        }
    }

    function _itemPrepareSuccess(uint256 waveKey, uint256 itemIdx) private view returns (bool) {
        NftItem storage it = _waveItems[waveKey][itemIdx];
        uint256 wi = _findWalletBatchIndex(waveKey, it.buyer);
        uint256 wLine = _countSameBuyerBefore(waveKey, itemIdx, it.buyer);
        if (_wLineOk[waveKey][wi].length <= wLine || !_wLineOk[waveKey][wi][wLine]) {
            return false;
        }
        if (_sLineOk[waveKey][0].length <= itemIdx || !_sLineOk[waveKey][0][itemIdx]) {
            return false;
        }
        return true;
    }

    function _cleanupWave(uint256 waveKey) internal {
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            delete _wLineOk[waveKey][i];
        }
        SBatch[] storage sb = _waveS[waveKey];
        for (uint256 i = 0; i < sb.length; i++) {
            delete _sLineOk[waveKey][i];
        }
        delete _wave[waveKey];
        delete _waveItems[waveKey];
        delete _waveW[waveKey];
        delete _waveS[waveKey];
    }

    function _buildBatches(NftItem[] memory items, uint256 waveKey) internal {
        _buildWalletBatches(items, waveKey);
        _buildStoreBatch(items, waveKey);
    }

    function _nftUnitPrice() internal view returns (uint256) {
        (bool ok, bytes memory ret) = nftStore.staticcall(abi.encodeWithSignature("unitPrice()"));
        require(ok && ret.length >= 32, "SNftC: unitPrice");
        return abi.decode(ret, (uint256));
    }

    function _buildWalletBatches(NftItem[] memory items, uint256 waveKey) internal {
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
        uint256 up = _nftUnitPrice();
        for (uint256 i = 0; i < nub; i++) {
            _pushOneBuyerWallet(items, n, waveKey, ub[i], up);
        }
    }

    function _pushOneBuyerWallet(NftItem[] memory items, uint256 n, uint256 waveKey, address buyer, uint256 up) internal {
        uint256 cnt = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].buyer == buyer) {
                cnt++;
            }
        }
        uint256[] memory lineAmt = new uint256[](cnt);
        uint256 ix = 0;
        for (uint256 k = 0; k < n; k++) {
            if (items[k].buyer == buyer) {
                lineAmt[ix] = up * items[k].quantity;
                unchecked {
                    ix++;
                }
            }
        }
        bytes32 wid = keccak256(abi.encodePacked(waveKey, uint8(1), buyer));
        uint256 wi = _waveW[waveKey].length;
        _waveW[waveKey].push();
        _waveW[waveKey][wi].batchId = wid;
        _waveW[waveKey][wi].buyer = buyer;
        for (uint256 j = 0; j < lineAmt.length; j++) {
            _waveW[waveKey][wi].amounts.push(lineAmt[j]);
        }
    }

    function _buildStoreBatch(NftItem[] memory items, uint256 waveKey) internal {
        uint256 n = items.length;
        uint256[] memory lineQty = new uint256[](n);
        for (uint256 k = 0; k < n; k++) {
            lineQty[k] = items[k].quantity;
        }
        bytes32 sid = keccak256(abi.encodePacked(waveKey, uint8(2)));
        _waveS[waveKey].push();
        _waveS[waveKey][0].batchId = sid;
        for (uint256 j = 0; j < n; j++) {
            _waveS[waveKey][0].qtys.push(lineQty[j]);
        }
    }

    function _emitWalletPrepares(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            bytes memory cd = abi.encodeWithSignature("prepareBatch(bytes32,address,uint256[])", wb[i].batchId, wb[i].buyer, wb[i].amounts);
            _emitPrepare(waveKey, walletShardId, wallet, selfShard, cd, uint8(1), i);
        }
    }

    function _emitStorePrepares(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        SBatch storage sb = _waveS[waveKey][0];
        bytes memory cd = abi.encodeWithSignature("prepareBatch(bytes32,uint256[])", sb.batchId, sb.qtys);
        _emitPrepare(waveKey, nftStoreShardId, nftStore, selfShard, cd, uint8(2), 0);
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

        require(TwoPhaseLib.emitCrossShardRequest(targetShardId, TwoPhaseLib.PRECOMPILE_EXECUTOR, executorCalldata), "SNftC: prepare emit");
    }

    function _emitAbortWave(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes4 selAbort = bytes4(keccak256("abortBatch(bytes32)"));
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            _emitExecNoCallback(waveKey, walletShardId, wallet, selfShard, abi.encodeWithSelector(selAbort, wb[i].batchId));
        }
        _emitExecNoCallback(
            waveKey,
            nftStoreShardId,
            nftStore,
            selfShard,
            abi.encodeWithSelector(selAbort, _waveS[waveKey][0].batchId)
        );
    }

    function _emitCommitWave(uint256 waveKey) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        bytes4 selCommit = bytes4(keccak256("commitBatch(bytes32)"));
        WBatch[] storage wb = _waveW[waveKey];
        for (uint256 i = 0; i < wb.length; i++) {
            _emitExecNoCallback(waveKey, walletShardId, wallet, selfShard, abi.encodeWithSelector(selCommit, wb[i].batchId));
        }
        _emitExecNoCallback(
            waveKey,
            nftStoreShardId,
            nftStore,
            selfShard,
            abi.encodeWithSelector(selCommit, _waveS[waveKey][0].batchId)
        );
    }

    function _emitExecNoCallback(
        uint256,
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
            "SNftC: exec emit"
        );
    }
}
