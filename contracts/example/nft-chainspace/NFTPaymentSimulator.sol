// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title NFTPaymentSimulator
 * @dev UTXO 支付侧：与 `ChainspaceWalletSimulator` 相同的 note 模型；`preparePay` 成功则上锁并进入 2PC，
 *      与 peer 交换 `peerPrepareOk` 后 `commit`：校验 `note.value == transferAmount + changeAmount`，销毁输入 note，创建找零 note（`transferAmount` 与 Store 侧 `quantity*price` 由 UserClient 对齐）。
 *      构造：当前仅 **两方**（`totalParticipants_` 须为 2），传入 **唯一** peer 的地址与分片；内部再组装为数组传入 `TwoPCPeerBase`（便于 `joyue-deploy-all-shards` 无数组部署）。
 *      初始 note 由链下 `bootstrap-wallet-balances-from-csv`（与 chainspace 同 ABI 的 `mintInitialBatch`）灌入。
 */
contract NFTPaymentSimulator is TwoPCPeerBase {
    enum Status {
        NONE,
        ACTIVE,
        INACTIVE
    }

    struct TokenObject {
        address owner;
        uint256 value;
        Status status;
    }

    mapping(bytes32 => TokenObject) public objects;
    mapping(address => bytes32[]) private objectIdsByOwner;
    mapping(bytes32 => address) private _inputLockedBy;

    struct PayPrepare {
        bool exists;
        address buyer;
        bytes32 inputId;
        uint256 transferAmount;
        uint256 changeAmount;
    }

    mapping(bytes32 => PayPrepare) private _prepares;

    uint256 private _idSalt;

    event PayPrepared(bytes32 indexed txId, address indexed buyer, bytes32 indexed inputId, uint256 transferAmount, uint256 changeAmount);
    event PayCommitted(bytes32 indexed txId, bytes32 changeOutId);
    event PayAborted(bytes32 indexed txId);

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
    {}

    /// @dev 与 `ChainspaceWalletSimulator.mintInitialBatch` 命名一致，便于 bootstrap 共用 ABI。
    function mintInitialBatch(address[] calldata users, uint256 amount) external {
        for (uint256 i; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "PAY: zero");
            bytes32 id = _nextInitId(u, amount);
            objects[id] = TokenObject(u, amount, Status.ACTIVE);
            objectIdsByOwner[u].push(id);
        }
    }

    function _requireTwoPartyTotal(uint8 totalParticipants_) private pure returns (uint8) {
        require(totalParticipants_ == 2, "PAY: two-party only");
        return totalParticipants_;
    }

    function _singlePeerList(address peer) private pure returns (address[] memory a) {
        require(peer != address(0), "PAY: peer");
        a = new address[](1);
        a[0] = peer;
    }

    function _singleShardList(uint32 shard) private pure returns (uint32[] memory s) {
        s = new uint32[](1);
        s[0] = shard;
    }

    function _nextInitId(address to, uint256 amount) private returns (bytes32) {
        _idSalt++;
        return keccak256(abi.encodePacked("PAY_INIT", to, amount, _idSalt, block.timestamp));
    }

    function _removeObjectIdFromOwner(address owner, bytes32 id) private {
        bytes32[] storage arr = objectIdsByOwner[owner];
        uint256 n = arr.length;
        for (uint256 i = 0; i < n; i++) {
            if (arr[i] == id) {
                arr[i] = arr[n - 1];
                arr.pop();
                return;
            }
        }
    }

    function _releaseLock(bytes32 inputId) private {
        delete _inputLockedBy[inputId];
    }

    /// @dev `transferAmount + changeAmount` 必须等于链上该 note 面值（与 `ChainspaceWalletSimulator` 守恒一致）；锁失败不 revert。
    function preparePay(bytes32 txId, address buyer, bytes32 inputId, uint256 transferAmount, uint256 changeAmount) external returns (bool) {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        require(buyer != address(0), "PAY: buyer");
        require(transferAmount > 0, "PAY: xfer");

        TokenObject storage inputObj = objects[inputId];
        if (inputObj.value != transferAmount + changeAmount) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }
        if (!_tryLockNote(inputId, buyer)) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.LOCK_CONFLICT);
            return false;
        }

        _prepares[txId] = PayPrepare({
            exists: true,
            buyer: buyer,
            inputId: inputId,
            transferAmount: transferAmount,
            changeAmount: changeAmount
        });
        emit PayPrepared(txId, buyer, inputId, transferAmount, changeAmount);
        _markPreparedAndSync(txId);
        return true;
    }

    function _tryLockNote(bytes32 inputId, address buyer) private returns (bool) {
        if (_inputLockedBy[inputId] != address(0)) return false;
        TokenObject storage o = objects[inputId];
        if (o.owner != buyer || o.status != Status.ACTIVE) return false;
        _inputLockedBy[inputId] = buyer;
        return true;
    }

    function _abortLocal(bytes32 txId) internal override {
        PayPrepare memory p = _prepares[txId];
        if (!p.exists) return;
        _releaseLock(p.inputId);
        delete _prepares[txId];
        emit PayAborted(txId);
    }

    function _commitLocal(bytes32 txId) internal override returns (bool) {
        PayPrepare memory p = _prepares[txId];
        if (!p.exists) return false;
        TokenObject storage inputObj = objects[p.inputId];
        if (inputObj.status != Status.ACTIVE || inputObj.value != p.transferAmount + p.changeAmount || inputObj.owner != p.buyer) {
            return false;
        }
        inputObj.status = Status.INACTIVE;
        _removeObjectIdFromOwner(p.buyer, p.inputId);
        _releaseLock(p.inputId);
        delete _prepares[txId];

        bytes32 changeId = keccak256(abi.encodePacked(p.inputId, "NFT_PAY_CHANGE", p.buyer, p.changeAmount, block.timestamp));
        objects[changeId] = TokenObject(p.buyer, p.changeAmount, Status.ACTIVE);
        objectIdsByOwner[p.buyer].push(changeId);
        emit PayCommitted(txId, changeId);
        return true;
    }

    function countObjectsForOwner(address user) external view returns (uint256) {
        return objectIdsByOwner[user].length;
    }

    function objectIdForOwnerAt(address user, uint256 index) external view returns (bytes32) {
        return objectIdsByOwner[user][index];
    }

    function lockedBy(bytes32 inputId) external view returns (address) {
        return _inputLockedBy[inputId];
    }
}
