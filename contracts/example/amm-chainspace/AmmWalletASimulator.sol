// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title AmmWalletASimulator
 * @dev 与 `ChainspaceWalletSimulator` 一致：`objects` + `TokenObject`。`prepareDebitA` 须显式传入 `objectIds`：
 *      所列 ACTIVE 且 `owner==user` 的 object **全额计入**输入和 `S`，要求 `S >= amountIn`；prepare 时全部销毁（INACTIVE），
 *      commit 时若有找零 `S - amountIn` 则 mint 新 object；abort 时按快照恢复各输入。
 *      初始 A note 由链下 `bootstrap-wallet-balances-from-csv -kind amm-chainspace-wallet-a` 调 `mintInitialBatch`。
 */
contract AmmWalletASimulator is TwoPCPeerBase {
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
    uint256 private _idSalt;

    struct SpentInput {
        bytes32 id;
        uint256 value;
    }

    struct DebitPrepare {
        SpentInput[] inputs;
        address user;
        uint256 amountIn;
        uint256 changeAmount;
        bool exists;
    }

    mapping(bytes32 => DebitPrepare) private _prepareLocks;

    uint256 public constant INITIAL_USER_A = 10_000_000;

    event DebitPrepared(bytes32 indexed txId, address indexed user, uint256 amountIn, uint256 changeAmount, uint256 inputCount);
    event DebitCommitted(bytes32 indexed txId);
    event DebitAborted(bytes32 indexed txId);

    constructor(
        uint8 myIndex_,
        uint8 totalParticipants_,
        address peerWalletB_,
        uint32 peerWalletBShard_,
        address peerPool_,
        uint32 peerPoolShard_
    )
        TwoPCPeerBase(
            myIndex_,
            _requireThreePartyTotal(totalParticipants_),
            _twoPeerList(peerWalletB_, peerPool_),
            _twoShardList(peerWalletBShard_, peerPoolShard_)
        )
    {}

    /// @dev 与 `ChainspaceWalletSimulator.mintInitialBatch` 命名一致，便于 bootstrap 共用 ABI。
    function mintInitialBatch(address[] calldata users, uint256 amount) external {
        for (uint256 i; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "WA: zero");
            bytes32 id = _nextInitId(u, amount);
            objects[id] = TokenObject(u, amount, Status.ACTIVE);
            objectIdsByOwner[u].push(id);
        }
    }

    function _requireThreePartyTotal(uint8 totalParticipants_) private pure returns (uint8) {
        require(totalParticipants_ == 3, "WA: N=3");
        return totalParticipants_;
    }

    function _twoPeerList(address p0, address p1) private pure returns (address[] memory a) {
        require(p0 != address(0) && p1 != address(0), "WA: peer");
        a = new address[](2);
        a[0] = p0;
        a[1] = p1;
    }

    function _twoShardList(uint32 s0, uint32 s1) private pure returns (uint32[] memory s) {
        s = new uint32[](2);
        s[0] = s0;
        s[1] = s1;
    }

    function _nextInitId(address to, uint256 amount) private returns (bytes32) {
        unchecked {
            _idSalt++;
        }
        return keccak256(abi.encodePacked("AMM_WA_INIT", to, amount, _idSalt, block.timestamp));
    }

    function _nextChangeObjectId(bytes32 txId, address user, uint256 changeAmt) private returns (bytes32) {
        unchecked {
            _idSalt++;
        }
        return keccak256(abi.encodePacked("AMM_WA_CHG", txId, user, changeAmt, _idSalt, block.timestamp));
    }

    function countObjectsForOwner(address user) external view returns (uint256) {
        return objectIdsByOwner[user].length;
    }

    function objectIdForOwnerAt(address user, uint256 index) external view returns (bytes32) {
        return objectIdsByOwner[user][index];
    }

    /// @dev 该用户名下所有 ACTIVE object 的 `value` 之和。
    function balanceA(address user) external view returns (uint256 total) {
        bytes32[] storage ids = objectIdsByOwner[user];
        for (uint256 i; i < ids.length; i++) {
            TokenObject storage o = objects[ids[i]];
            if (o.status == Status.ACTIVE) {
                total += o.value;
            }
        }
    }

    /// @dev 校验 `objectIds` 无重复、均为 `user` 的 ACTIVE object，并返回面值和 `S`；否则 `ok=false`。
    function sumDebitObjects(address user, bytes32[] calldata objectIds) external view returns (uint256 sum, bool ok) {
        uint256 len = objectIds.length;
        if (len == 0) return (0, false);
        for (uint256 i; i < len; i++) {
            for (uint256 j = i + 1; j < len; j++) {
                if (objectIds[i] == objectIds[j]) return (0, false);
            }
        }
        for (uint256 i; i < len; i++) {
            TokenObject storage o = objects[objectIds[i]];
            if (o.owner != user || o.status != Status.ACTIVE) return (0, false);
            sum += o.value;
        }
        return (sum, true);
    }

    function prepareDebitA(bytes32 txId, address user, uint256 amountIn, bytes32[] calldata objectIds) external returns (bool) {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        if (amountIn == 0) return false;
        require(user != address(0), "WA: user");

        (bool reserved, uint8 failReason) = _tryReserveDebit(txId, user, amountIn, objectIds);
        if (!reserved) {
            _failPrepareAndNotifyPeers(txId, failReason);
            return false;
        }

        DebitPrepare storage pl = _prepareLocks[txId];
        emit DebitPrepared(txId, user, amountIn, pl.changeAmount, pl.inputs.length);
        _markPreparedAndSync(txId);
        return true;
    }

    function _removeObjectIdFromOwner(address owner, bytes32 id) private {
        bytes32[] storage arr = objectIdsByOwner[owner];
        uint256 n = arr.length;
        for (uint256 i; i < n; i++) {
            if (arr[i] == id) {
                arr[i] = arr[n - 1];
                arr.pop();
                return;
            }
        }
    }

    function _tryReserveDebit(bytes32 txId, address user, uint256 amountIn, bytes32[] calldata objectIds)
        private
        returns (bool ok, uint8 failReason)
    {
        if (_prepareLocks[txId].exists) return (false, ChainspaceReason.LOCK_CONFLICT);
        uint256 len = objectIds.length;
        if (len == 0) return (false, ChainspaceReason.BUSINESS_RULE);
        for (uint256 i; i < len; i++) {
            for (uint256 j = i + 1; j < len; j++) {
                if (objectIds[i] == objectIds[j]) return (false, ChainspaceReason.BUSINESS_RULE);
            }
        }
        uint256 S;
        for (uint256 i; i < len; i++) {
            TokenObject storage o = objects[objectIds[i]];
            if (o.owner != user) return (false, ChainspaceReason.BUSINESS_RULE);
            if (o.status != Status.ACTIVE) return (false, ChainspaceReason.LOCK_CONFLICT);
            S += o.value;
        }
        if (S < amountIn) return (false, ChainspaceReason.BUSINESS_RULE);

        DebitPrepare storage pl = _prepareLocks[txId];
        pl.user = user;
        pl.amountIn = amountIn;
        pl.changeAmount = S - amountIn;
        pl.exists = true;

        for (uint256 k; k < len; k++) {
            bytes32 id = objectIds[k];
            TokenObject storage o = objects[id];
            uint256 v = o.value;
            pl.inputs.push(SpentInput({id: id, value: v}));
            o.value = 0;
            o.status = Status.INACTIVE;
            _removeObjectIdFromOwner(user, id);
        }
        return (true, 0);
    }

    function _abortLocal(bytes32 txId) internal override {
        DebitPrepare storage pls = _prepareLocks[txId];
        if (!pls.exists) return;
        uint256 n = pls.inputs.length;
        for (uint256 i; i < n; i++) {
            SpentInput storage si = pls.inputs[i];
            TokenObject storage o = objects[si.id];
            o.value = si.value;
            o.status = Status.ACTIVE;
            objectIdsByOwner[pls.user].push(si.id);
        }
        delete _prepareLocks[txId];
        emit DebitAborted(txId);
    }

    function _commitLocal(bytes32 txId) internal override returns (bool) {
        DebitPrepare storage pls = _prepareLocks[txId];
        if (!pls.exists) return false;
        address u = pls.user;
        uint256 ch = pls.changeAmount;
        delete _prepareLocks[txId];
        emit DebitCommitted(txId);
        if (ch > 0) {
            bytes32 newId = _nextChangeObjectId(txId, u, ch);
            objects[newId] = TokenObject(u, ch, Status.ACTIVE);
            objectIdsByOwner[u].push(newId);
        }
        return true;
    }
}
