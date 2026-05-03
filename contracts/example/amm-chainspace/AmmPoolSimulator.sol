// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title AmmPoolSimulator
 * @dev 单池 CPMM：`amountOut = amountIn * resB / (resA + amountIn)`（A 进 B 出）；`preparePoolSwap` 与 NFTStore 一致
 *      先占 `poolPrepareTxId`、预改 `resA/resB`，abort 回滚；`TOTAL_PARTICIPANTS = 3` 时 `MY_INDEX` 须为 2。
 */
contract AmmPoolSimulator is TwoPCPeerBase {
    struct PoolObject {
        uint256 resA;
        uint256 resB;
    }

    PoolObject public pool;

    struct PoolSwapPrepare {
        uint256 amountIn;
        uint256 amountOut;
        bool exists;
    }

    mapping(bytes32 => PoolSwapPrepare) public prepareLocks;
    bytes32 public poolPrepareTxId;

    uint256 public constant INITIAL_RES = 10 ** 24;

    event PoolSwapPrepared(bytes32 indexed txId, uint256 amountIn, uint256 amountOut);
    event PoolSwapCommitted(bytes32 indexed txId);
    event PoolSwapAborted(bytes32 indexed txId);

    constructor(
        uint8 myIndex_,
        uint8 totalParticipants_,
        address peerWalletA_,
        uint32 peerWalletAShard_,
        address peerWalletB_,
        uint32 peerWalletBShard_
    )
        TwoPCPeerBase(
            myIndex_,
            _requireThreePartyTotal(totalParticipants_),
            _twoPeerList(peerWalletA_, peerWalletB_),
            _twoShardList(peerWalletAShard_, peerWalletBShard_)
        )
    {
        pool = PoolObject({resA: INITIAL_RES, resB: INITIAL_RES});
    }

    function _requireThreePartyTotal(uint8 totalParticipants_) private pure returns (uint8) {
        require(totalParticipants_ == 3, "POOL: N=3");
        return totalParticipants_;
    }

    function _twoPeerList(address p0, address p1) private pure returns (address[] memory a) {
        require(p0 != address(0) && p1 != address(0), "POOL: peer");
        a = new address[](2);
        a[0] = p0;
        a[1] = p1;
    }

    function _twoShardList(uint32 s0, uint32 s1) private pure returns (uint32[] memory s) {
        s = new uint32[](2);
        s[0] = s0;
        s[1] = s1;
    }

    /// @dev 当前池报价：用户用 `amountIn` 的 A 换出的 B 数量（整除向下取整）。
    function quoteSwapAForB(uint256 amountIn) external view returns (uint256 amountOut) {
        PoolObject memory p = pool;
        require(p.resA > 0 && p.resB > 0, "POOL: empty");
        require(amountIn > 0, "POOL: in");
        amountOut = (amountIn * p.resB) / (p.resA + amountIn);
    }

    /// @dev `minOut` 为对 `amountOut` 的下限；prepare 阶段即调整储备（与 NFTStore 预扣 count 一致）。
    function preparePoolSwap(bytes32 txId, uint256 amountIn, uint256 minOut) external returns (bool) {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        if (amountIn == 0) return false;

        uint256 amountOut = (amountIn * pool.resB) / (pool.resA + amountIn);
        if (amountOut < minOut) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }
        if (pool.resB < amountOut) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }

        if (!_tryReservePool(txId, amountIn, amountOut)) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.LOCK_CONFLICT);
            return false;
        }

        emit PoolSwapPrepared(txId, amountIn, amountOut);
        _markPreparedAndSync(txId);
        return true;
    }

    function _tryReservePool(bytes32 txId, uint256 amountIn, uint256 amountOut) private returns (bool) {
        bytes32 cur = poolPrepareTxId;
        if (cur != bytes32(0) && cur != txId) return false;
        if (prepareLocks[txId].exists) return false;

        if (cur == bytes32(0)) {
            poolPrepareTxId = txId;
        }
        pool.resA += amountIn;
        pool.resB -= amountOut;
        prepareLocks[txId] = PoolSwapPrepare({amountIn: amountIn, amountOut: amountOut, exists: true});
        return true;
    }

    function _abortLocal(bytes32 txId) internal override {
        PoolSwapPrepare memory pl = prepareLocks[txId];
        if (!pl.exists) return;
        pool.resA -= pl.amountIn;
        pool.resB += pl.amountOut;
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
