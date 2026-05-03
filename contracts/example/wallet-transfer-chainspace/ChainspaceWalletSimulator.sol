// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/ChainspaceReason.sol";
import "../chainspace-utils/ChainspaceRevert.sol";

/**
 * @title ChainspaceWalletSimulator
 * @dev 测试用 UTXO 账本：不校验调用方身份。消费前对 `inputId` 上锁，记录 **该 object 的经济 owner**（与 `objects[inputId].owner` 一致），便于观测；
 *      锁冲突时 `revert ChainspaceRevert(LOCK_CONFLICT)`，便于 Executor 将 `returnData` 传回 `onWalletVerifyCallback` 与 metrics 对齐。
 *
 *      初始 **无** note；部署后由 `cmd/bootstrap-wallet-balances-from-csv -kind chainspace` 调 `mintInitialBatch`（或单笔 `mintInitial`）灌入，与 2PC `setBalances` 流程对称。
 */
contract ChainspaceWalletSimulator {
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

    /// @dev `inputId` 被占用时记录的经济 owner（非 `msg.sender`）；`address(0)` 表示未锁
    mapping(bytes32 => address) private _inputLockedBy;

    uint256 private _idSalt;

    event TransactionCommitted(bytes32 indexed inputId, bytes32 output1, bytes32 output2);
    event TransactionAborted(bytes32 indexed inputId, string reason);

    constructor() {}

    function countObjectsForOwner(address user) external view returns (uint256) {
        return objectIdsByOwner[user].length;
    }

    function objectIdForOwnerAt(address user, uint256 index) external view returns (bytes32) {
        return objectIdsByOwner[user][index];
    }

    function lockedBy(bytes32 inputId) external view returns (address) {
        return _inputLockedBy[inputId];
    }

    function _nextInitId(address to, uint256 amount) private returns (bytes32) {
        _idSalt++;
        return keccak256(abi.encodePacked("INIT", to, amount, _idSalt, block.timestamp));
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

    /// @dev 仅当当前无锁时可加锁；`locker` 须为链上该 object 的 owner（由调用方在校验后传入）
    function _acquireLock(bytes32 inputId, address locker) private {
        if (_inputLockedBy[inputId] != address(0)) {
            revert ChainspaceRevert(ChainspaceReason.LOCK_CONFLICT);
        }
        _inputLockedBy[inputId] = locker;
    }

    function _releaseLock(bytes32 inputId) private {
        delete _inputLockedBy[inputId];
    }

    /// @dev 显式 `from` 须与链上 `objects[inputId].owner` 一致；不依赖 `msg.sender` 作为付款人。锁记录为 object owner，而非 Executor。
    function verifyAndCommit(address from, bytes32 inputId, address to, uint256 transferAmount, uint256 changeAmount) external {
        TokenObject storage inputObj = objects[inputId];
        if (inputObj.owner != from) {
            revert ChainspaceRevert(ChainspaceReason.BUSINESS_RULE);
        }
        _acquireLock(inputId, inputObj.owner);
        _verifyAndCommitAfterOwnerCheck(inputId, to, transferAmount, changeAmount, from);
        _releaseLock(inputId);
    }

    function verifyAndCommitFor(address owner, bytes32 inputId, address to, uint256 transferAmount, uint256 changeAmount) external {
        TokenObject storage inputObj = objects[inputId];
        if (inputObj.owner != owner) {
            revert ChainspaceRevert(ChainspaceReason.BUSINESS_RULE);
        }
        _acquireLock(inputId, inputObj.owner);
        _verifyAndCommitAfterOwnerCheck(inputId, to, transferAmount, changeAmount, owner);
        _releaseLock(inputId);
    }

    function _verifyAndCommitAfterOwnerCheck(
        bytes32 inputId,
        address to,
        uint256 transferAmount,
        uint256 changeAmount,
        address ownerClaim
    ) private {
        TokenObject storage inputObj = objects[inputId];

        if (inputObj.status != Status.ACTIVE) {
            revert ChainspaceRevert(ChainspaceReason.BUSINESS_RULE);
        }

        if (inputObj.value != (transferAmount + changeAmount)) {
            revert ChainspaceRevert(ChainspaceReason.BUSINESS_RULE);
        }

        inputObj.status = Status.INACTIVE;
        _removeObjectIdFromOwner(ownerClaim, inputId);

        bytes32 outId1 = keccak256(abi.encodePacked(inputId, "TRANSFER", to, transferAmount, block.timestamp));
        objects[outId1] = TokenObject(to, transferAmount, Status.ACTIVE);
        objectIdsByOwner[to].push(outId1);

        bytes32 outId2 = keccak256(abi.encodePacked(inputId, "CHANGE", ownerClaim, changeAmount, block.timestamp));
        objects[outId2] = TokenObject(ownerClaim, changeAmount, Status.ACTIVE);
        objectIdsByOwner[ownerClaim].push(outId2);

        emit TransactionCommitted(inputId, outId1, outId2);
    }

    function mintInitial(address to, uint256 amount) external returns (bytes32) {
        require(to != address(0), "CWS: zero");
        require(amount > 0, "CWS: amount");
        bytes32 id = _nextInitId(to, amount);
        objects[id] = TokenObject(to, amount, Status.ACTIVE);
        objectIdsByOwner[to].push(id);
        return id;
    }

    /// @dev 与 `PeerTransferWallet2PC.setBalances` 对称：一批地址各 mint 一张面值为 `amount` 的 ACTIVE note。
    function mintInitialBatch(address[] calldata users, uint256 amount) external {
        require(amount > 0, "CWS: amount");
        for (uint256 i = 0; i < users.length; i++) {
            address to = users[i];
            require(to != address(0), "CWS: zero");
            bytes32 id = _nextInitId(to, amount);
            objects[id] = TokenObject(to, amount, Status.ACTIVE);
            objectIdsByOwner[to].push(id);
        }
    }
}
