// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title MevFlashLenderSimulator
 * @dev 四参与方 2PC 之一（`MY_INDEX = 0`）：金库 `vaultA`；`prepareLend` 预扣 `borrowAmount`，commit 收回 `repayDue`（含费），abort 退回预扣。
 */
contract MevFlashLenderSimulator is TwoPCPeerBase {
    uint256 public vaultA;
    uint256 public immutable FEE_BPS;

    struct LendPrepare {
        uint256 borrowAmount;
        uint256 repayDue;
        bool exists;
    }

    mapping(bytes32 => LendPrepare) private _prepareLocks;

    uint256 public constant INITIAL_VAULT_A = 10_000_000_000;

    event FlashLendPrepared(bytes32 indexed txId, uint256 borrowAmount, uint256 repayDue);
    event FlashLendCommitted(bytes32 indexed txId);
    event FlashLendAborted(bytes32 indexed txId);

    constructor(
        uint8 myIndex_,
        uint8 totalParticipants_,
        address peerWallet_,
        uint32 peerWalletShard_,
        address peerPool1_,
        uint32 peerPool1Shard_,
        address peerPool2_,
        uint32 peerPool2Shard_,
        uint256 feeBps_
    )
        TwoPCPeerBase(
            myIndex_,
            _requireFour(totalParticipants_),
            _threePeers(peerWallet_, peerPool1_, peerPool2_),
            _threeShards(peerWalletShard_, peerPool1Shard_, peerPool2Shard_)
        )
    {
        require(feeBps_ <= 5000, "MFL: fee");
        FEE_BPS = feeBps_;
        vaultA = INITIAL_VAULT_A;
    }

    function _requireFour(uint8 n) private pure returns (uint8) {
        require(n == 4, "MFL: N=4");
        return n;
    }

    function _threePeers(address a, address b, address c) private pure returns (address[] memory o) {
        require(a != address(0) && b != address(0) && c != address(0), "MFL: peer");
        o = new address[](3);
        o[0] = a;
        o[1] = b;
        o[2] = c;
    }

    function _threeShards(uint32 s0, uint32 s1, uint32 s2) private pure returns (uint32[] memory s) {
        s = new uint32[](3);
        s[0] = s0;
        s[1] = s1;
        s[2] = s2;
    }

    function owedOnBorrow(uint256 borrowAmount) public view returns (uint256) {
        if (borrowAmount == 0) return 0;
        return borrowAmount + (borrowAmount * FEE_BPS) / 10_000;
    }

    function prepareLend(bytes32 txId, uint256 borrowAmount, uint256 repayDue) external returns (bool) {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        if (borrowAmount == 0 || repayDue < borrowAmount) return false;
        uint256 expect = owedOnBorrow(borrowAmount);
        if (repayDue != expect) return false;
        if (vaultA < borrowAmount) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }
        if (_prepareLocks[txId].exists) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.LOCK_CONFLICT);
            return false;
        }

        vaultA -= borrowAmount;
        _prepareLocks[txId] = LendPrepare({borrowAmount: borrowAmount, repayDue: repayDue, exists: true});
        emit FlashLendPrepared(txId, borrowAmount, repayDue);
        _markPreparedAndSync(txId);
        return true;
    }

    function _abortLocal(bytes32 txId) internal override {
        LendPrepare memory pl = _prepareLocks[txId];
        if (!pl.exists) return;
        vaultA += pl.borrowAmount;
        delete _prepareLocks[txId];
        emit FlashLendAborted(txId);
    }

    function _commitLocal(bytes32 txId) internal override returns (bool) {
        LendPrepare memory pl = _prepareLocks[txId];
        if (!pl.exists) return false;
        vaultA += pl.repayDue;
        delete _prepareLocks[txId];
        emit FlashLendCommitted(txId);
        return true;
    }
}
