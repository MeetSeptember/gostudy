// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "../chainspace-utils/TwoPCPeerBase.sol";
import "../chainspace-utils/ChainspaceReason.sol";

/**
 * @title MevBotWalletSimulator
 * @dev 四参与方 2PC 之一（`MY_INDEX = 1`）：每笔 `prepareMevBot` 显式传入 `user`（套利账户），A/B 均为 UTXO 式 `Note`，同一合约可按 owner 分账模拟多用户。
 *      `prepareMevBot` 在一次 prepare 内完成：铸借币 A → 扣 Pool1 所需 A → 铸 Pool1 产出 B → 扣 Pool2 所需 B → 铸 Pool2 产出 A → 扣还贷 A；
 *      与编排器传入的报价数值一致（与双池 prepare 并行时的「视图时刻」假设同 amm-chainspace）。
 *      初始 A note：`mintInitialBatch`（与 ChainspaceWalletSimulator 同 ABI，供 bootstrap 使用）。
 */
contract MevBotWalletSimulator is TwoPCPeerBase {
    enum AssetKind {
        A,
        B
    }
    enum Status {
        NONE,
        ACTIVE,
        INACTIVE
    }

    struct Note {
        address owner;
        AssetKind asset;
        uint256 value;
        Status status;
    }

    mapping(bytes32 => Note) public objects;
    mapping(address => bytes32[]) private byOwner;
    uint256 private idSalt;

    struct SpentSnapshot {
        bytes32 id;
        uint256 valueBefore;
        AssetKind asset;
        bool removedFromOwner;
    }

    struct WalletPrepare {
        address user;
        SpentSnapshot[] spent;
        bytes32[] minted;
        bool exists;
    }

    mapping(bytes32 => WalletPrepare) private _locks;

    event MevWalletPrepared(
        bytes32 indexed txId,
        address indexed user,
        uint256 borrowAmount,
        uint256 aIn1,
        uint256 bOut1,
        uint256 bIn2,
        uint256 aOut2,
        uint256 repayDue
    );
    event MevWalletCommitted(bytes32 indexed txId);
    event MevWalletAborted(bytes32 indexed txId);

    constructor(
        uint8 myIndex_,
        uint8 totalParticipants_,
        address peerFlash_,
        uint32 peerFlashShard_,
        address peerPool1_,
        uint32 peerPool1Shard_,
        address peerPool2_,
        uint32 peerPool2Shard_
    )
        TwoPCPeerBase(
            myIndex_,
            _requireFour(totalParticipants_),
            _threePeers(peerFlash_, peerPool1_, peerPool2_),
            _threeShards(peerFlashShard_, peerPool1Shard_, peerPool2Shard_)
        )
    {}

    /// @dev 与 `ChainspaceWalletSimulator.mintInitialBatch` 命名一致，便于 bootstrap 共用 ABI；每人一张 A 侧 ACTIVE note。
    function mintInitialBatch(address[] calldata users, uint256 amount) external {
        require(amount > 0, "MW: amt");
        for (uint256 i; i < users.length; i++) {
            address u = users[i];
            require(u != address(0), "MW: zero");
            bytes32 id = _nextInitId(u, amount);
            objects[id] = Note({owner: u, asset: AssetKind.A, value: amount, status: Status.ACTIVE});
            _pushOwner(u, id);
        }
    }

    function _nextInitId(address to, uint256 amount) private returns (bytes32) {
        unchecked {
            idSalt++;
        }
        return keccak256(abi.encodePacked("MEV_W_INIT", to, amount, idSalt, block.timestamp));
    }

    function _requireFour(uint8 n) private pure returns (uint8) {
        require(n == 4, "MW: N=4");
        return n;
    }

    function _threePeers(address a, address b, address c) private pure returns (address[] memory o) {
        require(a != address(0) && b != address(0) && c != address(0), "MW: peer");
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

    function _nextId(bytes32 txId, string memory tag) private returns (bytes32 id) {
        unchecked {
            idSalt++;
        }
        return keccak256(abi.encodePacked("MEV_W", tag, txId, idSalt, block.timestamp));
    }

    function _pushOwner(address who, bytes32 id) private {
        byOwner[who].push(id);
    }

    function _removeFromOwner(address who, bytes32 id) private {
        bytes32[] storage arr = byOwner[who];
        uint256 n = arr.length;
        for (uint256 i; i < n; i++) {
            if (arr[i] == id) {
                arr[i] = arr[n - 1];
                arr.pop();
                return;
            }
        }
    }

    function _mintNote(
        bytes32 txId,
        WalletPrepare storage w,
        address owner,
        AssetKind asset,
        uint256 value,
        string memory tag
    ) private returns (bytes32 id) {
        id = _nextId(txId, tag);
        objects[id] = Note({owner: owner, asset: asset, value: value, status: Status.ACTIVE});
        _pushOwner(owner, id);
        w.minted.push(id);
    }

    function _sumActiveA(address account) private view returns (uint256 s) {
        bytes32[] storage arr = byOwner[account];
        for (uint256 i; i < arr.length; i++) {
            Note storage n = objects[arr[i]];
            if (n.owner == account && n.status == Status.ACTIVE && n.asset == AssetKind.A) {
                s += n.value;
            }
        }
    }

    function _sumActiveB(address account) private view returns (uint256 s) {
        bytes32[] storage arr = byOwner[account];
        for (uint256 i; i < arr.length; i++) {
            Note storage n = objects[arr[i]];
            if (n.owner == account && n.status == Status.ACTIVE && n.asset == AssetKind.B) {
                s += n.value;
            }
        }
    }

    function _spendA(WalletPrepare storage w, address account, uint256 need) private returns (bool) {
        if (need == 0) return true;
        if (_sumActiveA(account) < need) return false;
        uint256 rem = need;
        uint256 i;
        bytes32[] storage arr = byOwner[account];
        while (rem > 0 && i < arr.length) {
            bytes32 oid = arr[i];
            Note storage n = objects[oid];
            if (n.owner != account || n.status != Status.ACTIVE || n.asset != AssetKind.A) {
                unchecked {
                    i++;
                }
                continue;
            }
            uint256 vb = n.value;
            if (vb <= rem) {
                w.spent.push(SpentSnapshot({id: oid, valueBefore: vb, asset: AssetKind.A, removedFromOwner: true}));
                n.value = 0;
                n.status = Status.INACTIVE;
                _removeFromOwner(account, oid);
                unchecked {
                    rem -= vb;
                }
            } else {
                w.spent.push(SpentSnapshot({id: oid, valueBefore: vb, asset: AssetKind.A, removedFromOwner: false}));
                n.value = vb - rem;
                rem = 0;
                unchecked {
                    i++;
                }
            }
        }
        return rem == 0;
    }

    function _spendB(WalletPrepare storage w, address account, uint256 need) private returns (bool) {
        if (need == 0) return true;
        if (_sumActiveB(account) < need) return false;
        uint256 rem = need;
        uint256 i;
        bytes32[] storage arr = byOwner[account];
        while (rem > 0 && i < arr.length) {
            bytes32 oid = arr[i];
            Note storage n = objects[oid];
            if (n.owner != account || n.status != Status.ACTIVE || n.asset != AssetKind.B) {
                unchecked {
                    i++;
                }
                continue;
            }
            uint256 vb = n.value;
            if (vb <= rem) {
                w.spent.push(SpentSnapshot({id: oid, valueBefore: vb, asset: AssetKind.B, removedFromOwner: true}));
                n.value = 0;
                n.status = Status.INACTIVE;
                _removeFromOwner(account, oid);
                unchecked {
                    rem -= vb;
                }
            } else {
                w.spent.push(SpentSnapshot({id: oid, valueBefore: vb, asset: AssetKind.B, removedFromOwner: false}));
                n.value = vb - rem;
                rem = 0;
                unchecked {
                    i++;
                }
            }
        }
        return rem == 0;
    }

    function prepareMevBot(
        bytes32 txId,
        address user,
        uint256 borrowAmount,
        uint256 aIn1,
        uint256 bOut1,
        uint256 bIn2,
        uint256 aOut2,
        uint256 repayDue
    ) external returns (bool) {
        if (_txPrepareNacked[txId]) return false;
        PeerProgress storage pr = _peer[txId];
        if (pr.terminal || pr.localPrepared) return false;
        if (user == address(0)) return false;
        if (borrowAmount == 0 || aIn1 == 0 || bOut1 == 0 || aOut2 == 0) return false;
        if (aIn1 > borrowAmount) return false;
        if (bIn2 != bOut1) return false;
        if (aOut2 < repayDue) return false;
        if (_locks[txId].exists) {
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.LOCK_CONFLICT);
            return false;
        }

        WalletPrepare storage w = _locks[txId];
        w.user = user;
        w.exists = true;

        _mintNote(txId, w, user, AssetKind.A, borrowAmount, "BOR");
        if (!_spendA(w, user, aIn1)) {
            _rollbackWallet(txId);
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }
        _mintNote(txId, w, user, AssetKind.B, bOut1, "B1");
        if (!_spendB(w, user, bIn2)) {
            _rollbackWallet(txId);
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }
        _mintNote(txId, w, user, AssetKind.A, aOut2, "A2");
        if (!_spendA(w, user, repayDue)) {
            _rollbackWallet(txId);
            _failPrepareAndNotifyPeers(txId, ChainspaceReason.BUSINESS_RULE);
            return false;
        }

        emit MevWalletPrepared(txId, user, borrowAmount, aIn1, bOut1, bIn2, aOut2, repayDue);
        _markPreparedAndSync(txId);
        return true;
    }

    function _rollbackWallet(bytes32 txId) private {
        WalletPrepare storage w = _locks[txId];
        if (!w.exists) return;
        address u = w.user;
        uint256 m = w.minted.length;
        for (uint256 j; j < m; j++) {
            bytes32 mid = w.minted[j];
            _removeFromOwner(u, mid);
            delete objects[mid];
        }
        uint256 sn = w.spent.length;
        for (uint256 k; k < sn; k++) {
            SpentSnapshot memory sv = w.spent[k];
            Note storage n = objects[sv.id];
            n.value = sv.valueBefore;
            n.status = Status.ACTIVE;
            n.owner = u;
            n.asset = sv.asset;
            if (sv.removedFromOwner) {
                _pushOwner(u, sv.id);
            }
        }
        delete w.spent;
        delete w.minted;
        w.exists = false;
    }

    function _abortLocal(bytes32 txId) internal override {
        WalletPrepare storage w = _locks[txId];
        if (!w.exists) return;
        address u = w.user;
        uint256 m = w.minted.length;
        for (uint256 j; j < m; j++) {
            bytes32 mid = w.minted[j];
            _removeFromOwner(u, mid);
            delete objects[mid];
        }
        uint256 sn = w.spent.length;
        for (uint256 k; k < sn; k++) {
            SpentSnapshot memory sv = w.spent[k];
            Note storage n = objects[sv.id];
            n.value = sv.valueBefore;
            n.status = Status.ACTIVE;
            n.owner = u;
            n.asset = sv.asset;
            if (sv.removedFromOwner) {
                _pushOwner(u, sv.id);
            }
        }
        delete w.spent;
        delete w.minted;
        w.exists = false;
        emit MevWalletAborted(txId);
    }

    function _commitLocal(bytes32 txId) internal override returns (bool) {
        WalletPrepare storage w = _locks[txId];
        if (!w.exists) return false;
        delete _locks[txId];
        emit MevWalletCommitted(txId);
        return true;
    }
}
