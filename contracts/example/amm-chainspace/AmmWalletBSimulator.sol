// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title AmmWalletBSimulator
 * @dev 用户 token B 与 `ChainspaceWalletSimulator` 一致：`objects` + `TokenObject`；初始无用户 B object，仅金库 `vaultB`。
 *      `prepareCreditB` 从金库预留 `amountOut`，commit 时为用户 **mint** 新的 ACTIVE object；`MY_INDEX` 须为 1。
 */
contract AmmWalletBSimulator is TwoPCPeerBase {
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

    uint256 public vaultB;

    struct CreditPrepare {
        address user;
        uint256 amountOut;
        bool exists;
    }

    mapping(bytes32 => CreditPrepare) public prepareLocks;

    uint256 public constant INITIAL_VAULT_B = 1_000_000_000;

    event CreditPrepared(bytes32 indexed txId, address indexed user, uint256 amountOut);
    event CreditCommitted(bytes32 indexed txId, bytes32 indexed newObjectId);
    event CreditAborted(bytes32 indexed txId);

    constructor(
        uint8 myIndex_,
        uint8 totalParticipants_,
        address peerWalletA_,
        uint32 peerWalletAShard_,
        address peerPool_,
        uint32 peerPoolShard_
    )
        TwoPCPeerBase(
            myIndex_,
            _requireThreePartyTotal(totalParticipants_),
            _twoPeerList(peerWalletA_, peerPool_),
            _twoShardList(peerWalletAShard_, peerPoolShard_)
        )
    {
        vaultB = INITIAL_VAULT_B;
    }

    function _requireThreePartyTotal(uint8 totalParticipants_) private pure returns (uint8) {
        require(totalParticipants_ == 3, "WB: N=3");
        return totalParticipants_;
    }

    function _twoPeerList(address p0, address p1) private pure returns (address[] memory a) {
        require(p0 != address(0) && p1 != address(0), "WB: peer");
        a = new address[](2);
        a[0] = p0;
        a[1] = p1;
    }

    function _twoShardList(uint32 s0, uint32 s1) private pure returns (uint32[] memory s) {
        s = new uint32[](2);
        s[0] = s0;
        s[1] = s1;
    }

    function countObjectsForOwner(address user) external view returns (uint256) {
        return objectIdsByOwner[user].length;
    }

    function objectIdForOwnerAt(address user, uint256 index) external view returns (bytes32) {
        return objectIdsByOwner[user][index];
    }

    /// @dev 该用户名下所有 ACTIVE object 的 `value` 之和。
    function balanceB(address user) external view returns (uint256 total) {
        bytes32[] storage ids = objectIdsByOwner[user];
        for (uint256 i; i < ids.length; i++) {
            TokenObject storage o = objects[ids[i]];
            if (o.status == Status.ACTIVE) {
                total += o.value;
            }
        }
    }

    function prepareCreditB(bytes32 txId, address user, uint256 amountOut) external returns (bool) {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        if (amountOut == 0) return false;
        require(user != address(0), "WB: user");

        (bool reserved, uint8 failReason) = _tryReserveCredit(txId, user, amountOut);
        if (!reserved) {
            _failPrepareAndNotifyPeers(txId, failReason);
            return false;
        }

        emit CreditPrepared(txId, user, amountOut);
        _markPreparedAndSync(txId);
        return true;
    }

    function _tryReserveCredit(bytes32 txId, address user, uint256 amountOut) private returns (bool ok, uint8 failReason) {
        if (prepareLocks[txId].exists) return (false, ChainspaceReason.LOCK_CONFLICT);
        if (vaultB < amountOut) return (false, ChainspaceReason.BUSINESS_RULE);
        vaultB -= amountOut;
        prepareLocks[txId] = CreditPrepare({user: user, amountOut: amountOut, exists: true});
        return (true, 0);
    }

    function _nextCreditObjectId(bytes32 txId, address user, uint256 amountOut) private returns (bytes32) {
        unchecked {
            _idSalt++;
        }
        return keccak256(abi.encodePacked("AMM_WB_CREDIT", txId, user, amountOut, _idSalt, block.timestamp));
    }

    function _abortLocal(bytes32 txId) internal override {
        CreditPrepare memory pl = prepareLocks[txId];
        if (!pl.exists) return;
        vaultB += pl.amountOut;
        delete prepareLocks[txId];
        emit CreditAborted(txId);
    }

    function _commitLocal(bytes32 txId) internal override returns (bool) {
        CreditPrepare memory pl = prepareLocks[txId];
        if (!pl.exists) return false;
        bytes32 newId = _nextCreditObjectId(txId, pl.user, pl.amountOut);
        objects[newId] = TokenObject(pl.user, pl.amountOut, Status.ACTIVE);
        objectIdsByOwner[pl.user].push(newId);
        delete prepareLocks[txId];
        emit CreditCommitted(txId, newId);
        return true;
    }
}
