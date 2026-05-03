// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title MevPoolSimulator
 * @dev 四参与方 2PC 单池 CPMM：`A->B` 与 `B->A` 两向 swap；`MY_INDEX` 为 2 或 3；与 `AmmPoolSimulator` 同形锁 `poolPrepareTxId`。
 */
contract MevPoolSimulator is TwoPCPeerBase {
    struct PoolObject {
        uint256 resA;
        uint256 resB;
    }

    PoolObject public pool;

    struct PoolSwapPrepare {
        bool isAForB;
        uint256 amountIn;
        uint256 amountOut;
        bool exists;
    }

    mapping(bytes32 => PoolSwapPrepare) public prepareLocks;
    bytes32 public poolPrepareTxId;

    event PoolSwapPrepared(bytes32 indexed txId, bool aForB, uint256 amountIn, uint256 amountOut);
    event PoolSwapCommitted(bytes32 indexed txId);
    event PoolSwapAborted(bytes32 indexed txId);

    constructor(
        uint8 myIndex_,
        uint8 totalParticipants_,
        address peerFlash_,
        uint32 peerFlashShard_,
        address peerWallet_,
        uint32 peerWalletShard_,
        address peerOtherPool_,
        uint32 peerOtherPoolShard_,
        uint256 initResA_,
        uint256 initResB_
    )
        TwoPCPeerBase(
            myIndex_,
            _requireFour(totalParticipants_),
            _threePeers(peerFlash_, peerWallet_, peerOtherPool_),
            _threeShards(peerFlashShard_, peerWalletShard_, peerOtherPoolShard_)
        )
    {
        require(initResA_ > 0 && initResB_ > 0, "MP: res");
        pool = PoolObject({resA: initResA_, resB: initResB_});
    }

    function _requireFour(uint8 n) private pure returns (uint8) {
        require(n == 4, "MP: N=4");
        return n;
    }

    function _threePeers(address a, address b, address c) private pure returns (address[] memory o) {
        require(a != address(0) && b != address(0) && c != address(0), "MP: peer");
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

    function quoteSwapAForB(uint256 amountIn) external view returns (uint256 amountOut) {
        PoolObject memory p = pool;
        require(p.resA > 0 && p.resB > 0, "MP: empty");
        require(amountIn > 0, "MP: in");
        amountOut = (amountIn * p.resB) / (p.resA + amountIn);
    }

    function quoteSwapBForA(uint256 amountInB) external view returns (uint256 amountOutA) {
        PoolObject memory p = pool;
        require(p.resA > 0 && p.resB > 0, "MP: empty");
        require(amountInB > 0, "MP: in");
        amountOutA = (amountInB * p.resA) / (p.resB + amountInB);
    }

    function preparePoolSwapAForB(bytes32 txId, uint256 amountIn, uint256 minOut) external returns (bool) {
        return _prepareSwap(txId, true, amountIn, minOut);
    }

    function preparePoolSwapBForA(bytes32 txId, uint256 amountInB, uint256 minOutA) external returns (bool) {
        return _prepareSwap(txId, false, amountInB, minOutA);
    }

    function _prepareSwap(bytes32 txId, bool aForB, uint256 amountIn, uint256 minOut) private returns (bool) {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        if (amountIn == 0) return false;

        uint256 amountOut;
        if (aForB) {
            amountOut = (amountIn * pool.resB) / (pool.resA + amountIn);
        } else {
            amountOut = (amountIn * pool.resA) / (pool.resB + amountIn);
        }
        if (amountOut < minOut) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }
        if (aForB) {
            if (pool.resB < amountOut) {
                _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
                return false;
            }
        } else {
            if (pool.resA < amountOut) {
                _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
                return false;
            }
        }

        if (!_tryReserve(txId, aForB, amountIn, amountOut)) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.LOCK_CONFLICT);
            return false;
        }
        emit PoolSwapPrepared(txId, aForB, amountIn, amountOut);
        _markPreparedAndSync(txId);
        return true;
    }

    function _tryReserve(bytes32 txId, bool aForB, uint256 amountIn, uint256 amountOut) private returns (bool) {
        bytes32 cur = poolPrepareTxId;
        if (cur != bytes32(0) && cur != txId) return false;
        if (prepareLocks[txId].exists) return false;
        if (cur == bytes32(0)) {
            poolPrepareTxId = txId;
        }
        if (aForB) {
            pool.resA += amountIn;
            pool.resB -= amountOut;
        } else {
            pool.resB += amountIn;
            pool.resA -= amountOut;
        }
        prepareLocks[txId] = PoolSwapPrepare({isAForB: aForB, amountIn: amountIn, amountOut: amountOut, exists: true});
        return true;
    }

    function _abortLocal(bytes32 txId) internal override {
        PoolSwapPrepare memory pl = prepareLocks[txId];
        if (!pl.exists) return;
        if (pl.isAForB) {
            pool.resA -= pl.amountIn;
            pool.resB += pl.amountOut;
        } else {
            pool.resB -= pl.amountIn;
            pool.resA += pl.amountOut;
        }
        delete prepareLocks[txId];
        poolPrepareTxId = bytes32(0);
        emit PoolSwapAborted(txId);
    }

    function _commitLocal(bytes32 txId) internal override returns (bool) {
        PoolSwapPrepare memory pl = prepareLocks[txId];
        if (!pl.exists) return false;
        delete prepareLocks[txId];
        poolPrepareTxId = bytes32(0);
        emit PoolSwapCommitted(txId);
        return true;
    }
}
