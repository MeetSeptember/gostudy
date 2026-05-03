// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title NFTStoreSimulator
 * @dev 多 listing：构造时初始化 `NUM_LISTINGS` 个 `NFTObject`（各 `INITIAL_LISTING_COUNT` 件、单价 `INITIAL_PRICE`），
 *      `listingIdAt(i)` 派生 id；`prepareSupply(txId, listingId, ...)` 仅操作指定 listing，按 listing 粒度占锁（`listingPrepareTxId`）。
 *      构造：当前仅 **两方**（`totalParticipants_` 须为 2），传入 **唯一** peer 的地址与分片。
 */
contract NFTStoreSimulator is TwoPCPeerBase {
    enum Status {
        NONE,
        ACTIVE,
        INACTIVE
    }

    struct NFTObject {
        uint256 count;
        uint256 price;
        Status status;
    }

    uint256 public constant NUM_LISTINGS = 25;
    uint256 public constant INITIAL_LISTING_COUNT = 20;
    uint256 public constant INITIAL_PRICE = 100;

    mapping(bytes32 => NFTObject) public objects;

    struct SupplyPrepare {
        bytes32 listingId;
        uint256 quantity;
        uint256 expectedPaymentTotal;
        bool exists;
    }

    mapping(bytes32 => SupplyPrepare) public prepareLocks;
    /// @dev 每个 listing 上当前 prepare 占用的 `txId`；允许不同 listing 并行 prepare。
    mapping(bytes32 => bytes32) public listingPrepareTxId;

    event SupplyPrepared(bytes32 indexed txId, bytes32 indexed listingId, uint256 quantity);
    event SupplyCommitted(bytes32 indexed txId, bytes32 indexed listingId);
    event SupplyAborted(bytes32 indexed txId, bytes32 indexed listingId);

    constructor(
        uint8 myIndex_,
        uint8 totalParticipants_,
        address peerAddr_,
        uint32 peerShard_
    )
        TwoPCPeerBase(
            myIndex_,
            _requireTwoPartyTotal(totalParticipants_),
            _singlePeerList(peerAddr_),
            _singleShardList(peerShard_)
        )
    {
        for (uint256 i; i < NUM_LISTINGS; i++) {
            bytes32 lid = listingIdAt(i);
            objects[lid] = NFTObject({count: INITIAL_LISTING_COUNT, price: INITIAL_PRICE, status: Status.ACTIVE});
        }
    }

    function _requireTwoPartyTotal(uint8 totalParticipants_) private pure returns (uint8) {
        require(totalParticipants_ == 2, "STORE: two-party only");
        return totalParticipants_;
    }

    function _singlePeerList(address peer) private pure returns (address[] memory a) {
        require(peer != address(0), "STORE: peer");
        a = new address[](1);
        a[0] = peer;
    }

    function _singleShardList(uint32 shard) private pure returns (uint32[] memory s) {
        s = new uint32[](1);
        s[0] = shard;
    }

    /// @dev 第 `index` 个 listing 的 id（`index < NUM_LISTINGS`）。
    function listingIdAt(uint256 index) public pure returns (bytes32) {
        require(index < NUM_LISTINGS, "STORE: idx");
        return keccak256(abi.encodePacked("CHAINSPACE_NFT_LISTING", index));
    }

    /// @dev 指定 listing 的单价（须已初始化）。
    function listingUnitPrice(bytes32 listingId) external view returns (uint256) {
        return objects[listingId].price;
    }

    /// @dev `expectedPaymentTotal` 须等于链上 `quantity * listing.price`，与 Payment 侧 `transferAmount` 对账。
    function prepareSupply(bytes32 txId, bytes32 listingId, uint256 quantity, uint256 expectedPaymentTotal)
        external
        returns (bool)
    {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        if (quantity == 0) return false;

        NFTObject storage listing = objects[listingId];
        if (listing.status != Status.ACTIVE) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }
        uint256 line = quantity * listing.price;
        if (line != expectedPaymentTotal || line / quantity != listing.price) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }

        (bool reserved, uint8 failReason) = _tryReserveSupply(txId, listingId, quantity, expectedPaymentTotal);
        if (!reserved) {
            _failPrepareAndNotifyPeers(txId, failReason);
            return false;
        }

        emit SupplyPrepared(txId, listingId, quantity);
        _markPreparedAndSync(txId);
        return true;
    }

    function _tryReserveSupply(bytes32 txId, bytes32 listingId, uint256 quantity, uint256 expectedPaymentTotal)
        private
        returns (bool ok, uint8 failReason)
    {
        bytes32 cur = listingPrepareTxId[listingId];
        if (cur != bytes32(0) && cur != txId) return (false, ChainspaceReason.LOCK_CONFLICT);
        NFTObject storage listing = objects[listingId];
        if (listing.count < quantity) return (false, ChainspaceReason.BUSINESS_RULE);
        if (prepareLocks[txId].exists) return (false, ChainspaceReason.LOCK_CONFLICT);

        if (cur == bytes32(0)) {
            listingPrepareTxId[listingId] = txId;
        }
        listing.count -= quantity;
        prepareLocks[txId] =
            SupplyPrepare({listingId: listingId, quantity: quantity, expectedPaymentTotal: expectedPaymentTotal, exists: true});
        return (true, 0);
    }

    function _abortLocal(bytes32 txId) internal override {
        SupplyPrepare memory pl = prepareLocks[txId];
        if (!pl.exists) return;
        NFTObject storage listing = objects[pl.listingId];
        listing.count += pl.quantity;
        delete prepareLocks[txId];
        listingPrepareTxId[pl.listingId] = bytes32(0);
        emit SupplyAborted(txId, pl.listingId);
    }

    function _commitLocal(bytes32 txId) internal override returns (bool) {
        SupplyPrepare memory pl = prepareLocks[txId];
        if (!pl.exists) return false;
        delete prepareLocks[txId];
        listingPrepareTxId[pl.listingId] = bytes32(0);
        emit SupplyCommitted(txId, pl.listingId);
        return true;
    }
}
