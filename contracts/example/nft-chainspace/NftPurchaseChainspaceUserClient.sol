// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../twophase/TwoPhaseLib.sol";
import "../chainspace-utils/ChainspaceReason.sol";
import "./NFTPaymentSimulator.sol";
import "./NFTStoreSimulator.sol";

/**
 * @title NftPurchaseChainspaceUserClient
 * @dev 编排跨分片 NFT 购买：并行发出各 Simulator 的 `prepare`；各 Simulator 在 peer 2PC 完成后
 *      通过 `onParticipantFinished(txId, index, success, reason)` 回调；**全部** `success==true` 时认为整笔成功。
 *      - `_parts[0]` **须为** `NFTPaymentSimulator`（view 枚举 note）；`_parts[1]` **须为** `NFTStoreSimulator`。
 *      - 在 User 侧一次性算出 `paymentTotal = quantity*listingPrice`、`noteValue`、`changeAmount=noteValue-paymentTotal`，
 *        Payment `preparePay` 校验 `noteValue==paymentTotal+changeAmount`，Store `prepareSupply` 校验 `quantity*price==paymentTotal`；commit 再执行销毁/创建。
 *      - `startPurchase(buyer, quantity)`：在 Store 的 `NUM_LISTINGS` 中随机选一 listing；在 Payment 侧为该 buyer 选一张面值 `>= paymentTotal` 的 ACTIVE note（与旧 `startPurchaseRandom` 选 note 逻辑一致，但 buyer 由调用方指定）。
 *      - 在发起跨分片 prepare **之前**，若 listing 无法计价、无足够 note、note 面值不足等**预期内业务失败**，
 *        不 `revert`：分配 `txId` 并依次 `NftChainspaceIntentStarted`、`NftChainspaceIntentFinalized(txId,false,BUSINESS_RULE)`，供 metrics / 对账记录。
 *      - `startIntent`：传入与固定两腿（Payment、Store）一致的 `calls[0]`、`calls[1]`。
 *      构造：固定 **两方**（Payment + Store），标量地址与分片，便于 `joyue-deploy-all-shards` 逗号分隔部署。
 *      部署后请在各 Simulator 上调用 `setUserClient(address(this), selfShardId)` 各一次。
 */
contract NftPurchaseChainspaceUserClient {
    address[] private _parts;
    uint32[] private _pShards;
    uint256 private _nonce;
    uint256 private _xcReq;

    uint256 private _listingEntropyNonce;

    struct TxWait {
        uint8 total;
        uint8 received;
        uint256 mask;
        bool anyFail;
        bool exists;
        uint8 aggFailReason;
    }

    mapping(bytes32 => TxWait) private _wait;

    /// @dev 打包两腿 prepare 所需字段，降低主流程栈深（`payNoteId`：选中的 note `inputId`）。
    struct TwoLegIntent {
        bytes32 txId;
        address buyer;
        uint256 quantity;
        bytes32 payNoteId;
        uint256 paymentTotal;
        uint256 noteValue;
        uint256 changeAmount;
        bytes32 storeListingId;
    }

    event NftChainspaceIntentStarted(
        bytes32 indexed txId,
        uint8 participantCount,
        address buyer,
        uint256 quantity,
        bytes32 paymentInputId,
        uint256 paymentTotal,
        uint256 noteValue,
        uint256 changeAmount,
        bytes32 storeListingId
    );
    event NftChainspaceIntentFinalized(bytes32 indexed txId, bool success, uint8 finalReason);
    event PrepareLegSent(bytes32 indexed txId, uint8 indexed legIndex);

    constructor(
        address paymentSimulator_,
        uint32 paymentShard_,
        address storeSimulator_,
        uint32 storeShard_
    ) {
        require(paymentSimulator_ != address(0) && storeSimulator_ != address(0), "NPC: addr");
        _parts.push(paymentSimulator_);
        _pShards.push(paymentShard_);
        _parts.push(storeSimulator_);
        _pShards.push(storeShard_);
    }

    function participantCount() external view returns (uint256) {
        return _parts.length;
    }

    function participantAt(uint256 i) external view returns (address, uint32) {
        return (_parts[i], _pShards[i]);
    }

    function _payment() private view returns (NFTPaymentSimulator) {
        require(_parts.length >= 2, "NPC: parts");
        return NFTPaymentSimulator(_parts[0]);
    }

    function _store() private view returns (NFTStoreSimulator) {
        require(_parts.length >= 2, "NPC: parts");
        return NFTStoreSimulator(_parts[1]);
    }

    /// @dev `totalCost = quantity * listing.price`；失败时返回 `(false,0)`（listing 非 ACTIVE、库存不足、无单价或溢出）。
    function _tryListingTotalCost(bytes32 listingId, uint256 quantity) private view returns (bool ok, uint256 totalCost) {
        (uint256 cnt, uint256 unitPrice, NFTStoreSimulator.Status st) = _store().objects(listingId);
        if (st != NFTStoreSimulator.Status.ACTIVE || unitPrice == 0) return (false, 0);
        if (cnt < quantity) return (false, 0);
        totalCost = quantity * unitPrice;
        if (totalCost / quantity != unitPrice) return (false, 0);
        return (true, totalCost);
    }

    /// @dev 在 `NUM_LISTINGS` 中均匀随机一个 listing id（与 `NFTStoreSimulator.listingIdAt` 一致）。
    function _randomListingId(uint256 quantity) private returns (bytes32 listingId) {
        unchecked {
            _listingEntropyNonce++;
        }
        NFTStoreSimulator st = _store();
        uint256 n = st.NUM_LISTINGS();
        uint256 idx = uint256(
            keccak256(
                abi.encodePacked(
                    block.timestamp,
                    block.number,
                    block.prevrandao,
                    gasleft(),
                    msg.sender,
                    tx.origin,
                    quantity,
                    _listingEntropyNonce,
                    address(this)
                )
            )
        ) % n;
        listingId = st.listingIdAt(idx);
    }

    /// @dev 在已知 `paymentTotal` 下校验 note 与找零；用于「正常失败」分支与成功路径。
    function _tryPaymentSplitFromTotal(address buyer, bytes32 inputId, uint256 paymentTotal)
        private
        view
        returns (bool ok, uint256 noteValue, uint256 changeAmount)
    {
        (address o, uint256 val, NFTPaymentSimulator.Status st) = _payment().objects(inputId);
        if (st != NFTPaymentSimulator.Status.ACTIVE || o != buyer) return (false, 0, 0);
        if (val < paymentTotal) return (false, 0, 0);
        changeAmount = val - paymentTotal;
        if (val != paymentTotal + changeAmount) return (false, 0, 0);
        return (true, val, changeAmount);
    }

    /// @dev 预期内失败：带 Started（与成功路径同形，便于 joyue-trigger 取 tx_id）再 Finalized(false)；不占 `_wait`。
    function _emitIntentNormalFailure(
        bytes32 txId_,
        address buyer_,
        uint256 quantity_,
        bytes32 paymentInputId_,
        uint256 paymentTotal_,
        uint256 noteValue_,
        uint256 changeAmount_,
        bytes32 storeListingId_
    ) private {
        emit NftChainspaceIntentStarted(
            txId_, 2, buyer_, quantity_, paymentInputId_, paymentTotal_, noteValue_, changeAmount_, storeListingId_
        );
        emit NftChainspaceIntentFinalized(txId_, false, ChainspaceReason.BUSINESS_RULE);
    }

    /// @dev 子函数降低 `startPurchase` 栈深。
    function _finalizeEarlyFailure(
        address buyer_,
        uint256 quantity_,
        bytes32 paymentInputId_,
        uint256 paymentTotal_,
        bytes32 storeListingId_
    ) private returns (bytes32 tid_) {
        tid_ = _nextTxId();
        _emitIntentNormalFailure(tid_, buyer_, quantity_, paymentInputId_, paymentTotal_, 0, 0, storeListingId_);
    }

    function _openIntentTwoLegs(TwoLegIntent memory c) private {
        _wait[c.txId] = TxWait({total: 2, received: 0, mask: 0, anyFail: false, exists: true, aggFailReason: 0});
        emit NftChainspaceIntentStarted(
            c.txId, 2, c.buyer, c.quantity, c.payNoteId, c.paymentTotal, c.noteValue, c.changeAmount, c.storeListingId
        );
        _emitPrepare(
            _pShards[0],
            _parts[0],
            abi.encodeWithSelector(
                NFTPaymentSimulator.preparePay.selector, c.txId, c.buyer, c.payNoteId, c.paymentTotal, c.changeAmount
            )
        );
        emit PrepareLegSent(c.txId, 0);
        _emitPrepare(
            _pShards[1],
            _parts[1],
            abi.encodeWithSelector(
                NFTStoreSimulator.prepareSupply.selector, c.txId, c.storeListingId, c.quantity, c.paymentTotal
            )
        );
        emit PrepareLegSent(c.txId, 1);
    }

    function _notePickSalt(address buyer, uint256 quantity) private view returns (uint256) {
        return uint256(keccak256(abi.encodePacked(buyer, quantity, block.number, msg.sender, address(this))));
    }

    function _pickActiveNoteAtLeast(address buyer, uint256 minValue, uint256 salt)
        private
        view
        returns (bytes32 inputId, uint256 value, bool ok)
    {
        NFTPaymentSimulator pay = _payment();
        uint256 n = pay.countObjectsForOwner(buyer);
        if (n == 0 || minValue == 0) return (bytes32(0), 0, false);
        uint256 start = uint256(keccak256(abi.encodePacked(salt, buyer, block.prevrandao, block.number, address(this)))) % n;
        for (uint256 t = 0; t < n; t++) {
            bytes32 cand = pay.objectIdForOwnerAt(buyer, (start + t) % n);
            (address oowner, uint256 val, NFTPaymentSimulator.Status st) = pay.objects(cand);
            if (st == NFTPaymentSimulator.Status.ACTIVE && oowner == buyer && val >= minValue) {
                return (cand, val, true);
            }
        }
        return (bytes32(0), 0, false);
    }

    function _nextTxId() private returns (bytes32) {
        unchecked {
            _nonce++;
        }
        return keccak256(abi.encodePacked("NFT_CS", address(this), _nonce, block.number, msg.sender));
    }

    function onPrepareLaunched(uint256, bool, bytes calldata) external {}

    function _emitPrepare(uint32 targetShard, address target, bytes memory targetCalldata) private {
        uint32 selfShard = TwoPhaseLib.getCurrentShardID();
        unchecked {
            _xcReq++;
        }
        uint256 requestId = uint256(keccak256(abi.encodePacked("PRE", _xcReq, targetShard, target))) % (2 ** 128);
        bytes memory exec = TwoPhaseLib.buildExecutorCalldata(
            selfShard,
            address(this),
            this.onPrepareLaunched.selector,
            requestId,
            target,
            targetCalldata
        );
        require(TwoPhaseLib.emitCrossShardRequest(targetShard, TwoPhaseLib.PRECOMPILE_EXECUTOR, exec), "NPC: xc");
    }

    /// @dev `_parts[0]` = Payment，`_parts[1]` = Store。内部随机 listing、并为 buyer 自动挑选一张足额 ACTIVE note。
    function startPurchase(address buyer, uint256 quantity) external returns (bytes32 txId) {
        require(_parts.length == 2, "NPC: use startIntent");
        require(quantity > 0, "NPC: qty");
        require(buyer != address(0), "NPC: buyer");

        bytes32 storeListingId = _randomListingId(quantity);
        (bool costOk, uint256 paymentTotal) = _tryListingTotalCost(storeListingId, quantity);
        if (!costOk) {
            return _finalizeEarlyFailure(buyer, quantity, bytes32(0), 0, storeListingId);
        }

        (bytes32 inputId, , bool hit) = _pickActiveNoteAtLeast(buyer, paymentTotal, _notePickSalt(buyer, quantity));
        if (!hit) {
            return _finalizeEarlyFailure(buyer, quantity, bytes32(0), paymentTotal, storeListingId);
        }
        (bool splitOk, uint256 noteValue, uint256 changeAmount) = _tryPaymentSplitFromTotal(buyer, inputId, paymentTotal);
        if (!splitOk) {
            return _finalizeEarlyFailure(buyer, quantity, inputId, paymentTotal, storeListingId);
        }

        txId = _nextTxId();
        TwoLegIntent memory c;
        c.txId = txId;
        c.buyer = buyer;
        c.quantity = quantity;
        c.payNoteId = inputId;
        c.paymentTotal = paymentTotal;
        c.noteValue = noteValue;
        c.changeAmount = changeAmount;
        c.storeListingId = storeListingId;
        _openIntentTwoLegs(c);
    }

    /// @dev 两腿通用入口：`calls[i]` 发往 `_parts[i]`（须含统一 `txId` 作为首参或与各 Simulator 约定一致）。
    function startIntent(bytes[] calldata calls) external returns (bytes32 txId) {
        require(calls.length == _parts.length && calls.length >= 2, "NPC: calls");
        txId = _nextTxId();
        uint8 n = uint8(_parts.length);
        _wait[txId] = TxWait({total: n, received: 0, mask: 0, anyFail: false, exists: true, aggFailReason: 0});
        emit NftChainspaceIntentStarted(txId, n, msg.sender, 0, bytes32(0), 0, 0, 0, bytes32(0));

        for (uint256 i = 0; i < _parts.length; i++) {
            _emitPrepare(_pShards[i], _parts[i], calls[i]);
            emit PrepareLegSent(txId, uint8(i));
        }
    }

    function onParticipantFinished(bytes32 txId, uint8 participantIndex, bool success, uint8 reason) external {
        TxWait storage w = _wait[txId];
        require(w.exists, "NPC: tx");
        require(participantIndex < w.total, "NPC: idx");
        uint256 bit = uint256(1) << uint256(participantIndex);
        require((w.mask & bit) == 0, "NPC: dup");
        w.mask |= bit;
        w.received++;
        if (!success) {
            w.anyFail = true;
            w.aggFailReason = _mergeAggFailReason(w.aggFailReason, reason);
        }
        if (w.received == w.total) {
            bool ok = !w.anyFail;
            uint8 fr = ok
                ? ChainspaceReason.OK
                : (w.aggFailReason == 0 ? ChainspaceReason.OTHER : w.aggFailReason);
            emit NftChainspaceIntentFinalized(txId, ok, fr);
            delete _wait[txId];
        }
    }

    function _mergeAggFailReason(uint8 cur, uint8 inc) private pure returns (uint8) {
        return _legReasonPriority(inc) > _legReasonPriority(cur) ? inc : cur;
    }

    function _legReasonPriority(uint8 r) private pure returns (uint256) {
        if (r == ChainspaceReason.LOCK_CONFLICT) return 100;
        if (r == ChainspaceReason.BUSINESS_RULE) return 80;
        if (r == ChainspaceReason.FOLLOWER_ABORT) return 50;
        if (r == ChainspaceReason.OTHER) return 20;
        return 0;
    }
}
