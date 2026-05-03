// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";

interface ISparrowTransferWallet {
    function prepareTransferWave(
        address[] calldata froms,
        address[] calldata tos,
        uint256[] calldata amounts
    ) external returns (bytes memory);

    function commitTransferWave(uint256 batchId) external returns (bytes memory);

    function abortTransferWave(uint256 batchId) external;
}

/**
 * @title SparrowTransferCoordinator
 * @dev 2PL：`SparrowWaveStarted` 后，子批仅通过跨分片 `emitCrossShardRequest` 调 `prepareTransferWave` / `commitTransferWave`（无同笔同步直连 Wallet）。
 *      prepare 回调失败 / 锁失败等 → `onWalletPrepareResponse` 整批 `SparrowWaveFinished(..., false)`；成功则再 emit commit。按行 `SparrowWaveFinished` 在 **`onWalletCommitResponse`** 根据返回值发出。
 *      prepare / commit 的 emit 若失败，映射可能残留直至回调或链外处理。`onWalletCommitResponse` 中 `!ok` 时仅再 **跨分片** `emitCrossShardRequest` 调 Wallet `abortTransferWave`（不直连 `wallet`）；本笔内整批 `SparrowWaveFinished(..., false)`。`onWalletAbortResponse` 仅占位满足 Executor 回调格式。
 *      Executor 传入的 `retData` 对 Wallet 的 `returns (bytes memory)` 含外层 ABI `bytes`，须 `_unwrapWalletBytesReturn` 再解内层。
 */
contract SparrowTransferCoordinator {
    uint8 private constant _WALLET_MODE_LINE = 0;
    uint8 private constant _WALLET_MODE_LOCK = 1;

    ISparrowTransferWallet public wallet;
    uint32 public walletShardId;

    uint256 private _waveNonce;
    uint256 private _reqId;

    mapping(uint256 => bytes32[]) private _prepareReqTxIds;
    mapping(uint256 => uint256) private _commitReqToBatchId;
    mapping(uint256 => bytes32[]) private _batchIdToTxIds;

    struct TransferItem {
        address from;
        address to;
        uint256 amount;
        bytes32 txId;
        bool intentPrecheckOk;
    }

    event SparrowWaveStarted(bytes32 indexed waveId, uint256 mainTxCount, bytes32[] txIds);
    event SparrowWaveFinished(bytes32 indexed txId, bool committed);

    constructor(address _wallet, uint32 _walletShardId) {
        require(_wallet != address(0), "SparrowCoord: zero wallet addr");
        wallet = ISparrowTransferWallet(_wallet);
        walletShardId = _walletShardId;
    }

    function transferWave(TransferItem[] calldata items) external {
        uint256 n = items.length;
        TransferItem[] memory m = new TransferItem[](n);
        for (uint256 i = 0; i < n; i++) {
            m[i] = items[i];
        }
        _transferWave(m);
    }

    function _transferWave(TransferItem[] memory items) internal {
        uint256 n = items.length;
        require(n > 0, "SparrowCoord: empty wave");

        uint256 waveKey = ++_waveNonce;
        bytes32 waveId = keccak256(abi.encodePacked(waveKey, block.timestamp));

        bytes32[] memory txIds = new bytes32[](n);
        for (uint256 i = 0; i < n; i++) {
            txIds[i] = items[i].txId;
        }

        emit SparrowWaveStarted(waveId, n, txIds);

        uint256 m = 0;
        for (uint256 i = 0; i < n; i++) {
            if (!items[i].intentPrecheckOk) {
                emit SparrowWaveFinished(items[i].txId, false);
            } else {
                m++;
            }
        }

        if (m == 0) {
            return;
        }

        address[] memory subFrom = new address[](m);
        address[] memory subTo = new address[](m);
        uint256[] memory subAmt = new uint256[](m);
        bytes32[] memory subTxIds = new bytes32[](m);
        uint256 idx = 0;
        for (uint256 i = 0; i < n; i++) {
            if (!items[i].intentPrecheckOk) {
                continue;
            }
            subFrom[idx] = items[i].from;
            subTo[idx] = items[i].to;
            subAmt[idx] = items[i].amount;
            subTxIds[idx] = items[i].txId;
            unchecked {
                idx++;
            }
        }

        _runPreparePhase(subFrom, subTo, subAmt, subTxIds);
    }

    function _runPreparePhase(
        address[] memory subFrom,
        address[] memory subTo,
        uint256[] memory subAmt,
        bytes32[] memory subTxIds
    ) internal {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();

        uint256 prepReqId = ++_reqId;
        _prepareReqTxIds[prepReqId] = subTxIds;

        bytes memory targetCd = abi.encodeWithSelector(
            ISparrowTransferWallet.prepareTransferWave.selector,
            subFrom,
            subTo,
            subAmt
        );

        bytes memory executorCalldata = TwoPhaseLib.buildExecutorCalldata(
            selfShard,
            address(this),
            this.onWalletPrepareResponse.selector,
            prepReqId,
            address(wallet),
            targetCd
        );

        TwoPhaseLib.emitCrossShardRequest(
            walletShardId,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            executorCalldata
        );
    }

    function onWalletPrepareResponse(uint256 prepReqId, bool ok, bytes calldata retData) external {
        bytes32[] memory txIds = _prepareReqTxIds[prepReqId];
        delete _prepareReqTxIds[prepReqId];
        if (txIds.length == 0) {
            return;
        }

        if (!ok) {
            _emitAllFinished(txIds, false);
            return;
        }

        bytes memory payload = _unwrapWalletBytesReturn(retData);
        if (payload.length < 96) {
            _emitAllFinished(txIds, false);
            return;
        }

        (uint8 mode, uint256 batchId, bool[] memory lineOk) = abi.decode(payload, (uint8, uint256, bool[]));

        if (mode == _WALLET_MODE_LOCK || batchId == 0 || lineOk.length != txIds.length) {
            _emitAllFinished(txIds, false);
            return;
        }

        if (mode != _WALLET_MODE_LINE) {
            _emitAllFinished(txIds, false);
            return;
        }

        _batchIdToTxIds[batchId] = txIds;

        uint256 commitReqId = ++_reqId;
        _commitReqToBatchId[commitReqId] = batchId;

        bytes memory commitCd = abi.encodeWithSelector(ISparrowTransferWallet.commitTransferWave.selector, batchId);

        bytes memory execCommit = TwoPhaseLib.buildExecutorCalldata(
            TwoPhaseLib.getCurrentShardID(),
            address(this),
            this.onWalletCommitResponse.selector,
            commitReqId,
            address(wallet),
            commitCd
        );

        TwoPhaseLib.emitCrossShardRequest(
            walletShardId,
            TwoPhaseLib.PRECOMPILE_EXECUTOR,
            execCommit
        );
    }

    function onWalletCommitResponse(uint256 commitReqId, bool ok, bytes calldata retData) external {
        uint256 batchId = _commitReqToBatchId[commitReqId];
        delete _commitReqToBatchId[commitReqId];

        bytes32[] memory txIds = _batchIdToTxIds[batchId];
        delete _batchIdToTxIds[batchId];

        if (txIds.length == 0) {
            return;
        }

        if (!ok) {
            uint256 abortReqId = ++_reqId;
            bytes memory abortCd = abi.encodeWithSelector(ISparrowTransferWallet.abortTransferWave.selector, batchId);
            bytes memory execAbort = TwoPhaseLib.buildExecutorCalldata(
                TwoPhaseLib.getCurrentShardID(),
                address(this),
                this.onWalletAbortResponse.selector,
                abortReqId,
                address(wallet),
                abortCd
            );
            TwoPhaseLib.emitCrossShardRequest(
                walletShardId,
                TwoPhaseLib.PRECOMPILE_EXECUTOR,
                execAbort
            );
            _emitAllFinished(txIds, false);
            return;
        }

        bytes memory retMem = retData;
        _finishCommitSubWave(txIds, true, retMem);
    }

    /// @dev Wallet 分片上执行 `abortTransferWave` 后的 Executor 回调；释锁在目标分片完成，此处无需再调 `wallet`。
    function onWalletAbortResponse(uint256, bool, bytes calldata) external {}

    /// @dev `returns (bytes memory)` 经 `call` 的原始返回：前 64 字节为 offset(32)+length(32)，其后为内层 `abi.encode(...)`。
    function _unwrapWalletBytesReturn(bytes calldata raw) private pure returns (bytes memory inner) {
        if (raw.length < 64) {
            return inner;
        }
        return abi.decode(raw, (bytes));
    }

    function _emitAllFinished(bytes32[] memory txIds, bool v) private {
        uint256 len = txIds.length;
        for (uint256 i = 0; i < len; i++) {
            emit SparrowWaveFinished(txIds[i], v);
        }
    }

    function _finishCommitSubWave(bytes32[] memory txIds, bool execOk, bytes memory ret) private {
        uint256 len = txIds.length;
        if (len == 0) {
            return;
        }
        if (!execOk) {
            _emitAllFinished(txIds, false);
            return;
        }
        if (ret.length < 64) {
            _emitAllFinished(txIds, false);
            return;
        }
        bytes memory payload = abi.decode(ret, (bytes));
        if (payload.length < 64) {
            _emitAllFinished(txIds, false);
            return;
        }

        (uint8 mode, bool[] memory lineOk) = abi.decode(payload, (uint8, bool[]));

        if (mode == _WALLET_MODE_LOCK) {
            _emitAllFinished(txIds, false);
            return;
        }

        if (mode != _WALLET_MODE_LINE || lineOk.length != len) {
            _emitAllFinished(txIds, false);
            return;
        }

        for (uint256 i = 0; i < len; i++) {
            emit SparrowWaveFinished(txIds[i], lineOk[i]);
        }
    }

    function transferWave1(address from, address to, uint256 amount) external {
        TransferItem[] memory arr = new TransferItem[](1);
        arr[0] = TransferItem({
            from: from,
            to: to,
            amount: amount,
            txId: bytes32(0),
            intentPrecheckOk: true
        });
        _transferWave(arr);
    }
}
