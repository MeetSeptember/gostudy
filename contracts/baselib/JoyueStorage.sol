// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22;

/**
 * @title JoyueStorage
 * @dev 数据层：权威状态 + 冻结态存储，提供基础读写工具。
 */
contract JoyueStorage {
    // 权威状态
    mapping(bytes32 => uint256) internal _u;
    mapping(bytes32 => uint64) internal _ver;

    // 协调者侧 RPC 预取（mock）
    address public rpcOracle;

    // 批次冻结的瞬时状态
    mapping(bytes32 => mapping(bytes32 => uint256)) internal _tU;
    mapping(bytes32 => mapping(bytes32 => bool)) internal _tSet;
    mapping(bytes32 => bytes32[]) internal _tKeys;
    mapping(bytes32 => mapping(bytes32 => bool)) internal _tKeySeen;

    // 已冻结的 delta（按 txHash 存放）
    mapping(bytes32 => mapping(bytes32 => bytes)) internal _frozenDeltas; // abi.encode(Delta[])
    mapping(bytes32 => mapping(bytes32 => bool)) internal _frozen;

    function setRpcOracle(address oracle) external {
        rpcOracle = oracle;
    }

    // ---------- 权威读写 ----------
    function getUint(bytes32 key) public view returns (uint256 value, uint64 version) {
        return (_u[key], _ver[key]);
    }

    function _setUint(bytes32 key, uint256 value) internal {
        _u[key] = value;
        _ver[key] = _ver[key] + 1;
    }

    // ---------- Transient 读写 ----------
    function _touchTransientKey(bytes32 batchId, bytes32 key) internal {
        if (!_tKeySeen[batchId][key]) {
            _tKeySeen[batchId][key] = true;
            _tKeys[batchId].push(key);
        }
    }

    function _getTransient(bytes32 batchId, bytes32 key) internal returns (uint256) {
        if (!_tSet[batchId][key]) {
            _tSet[batchId][key] = true;
            _tU[batchId][key] = _u[key];
            _touchTransientKey(batchId, key);
        }
        return _tU[batchId][key];
    }

    function _setTransient(bytes32 batchId, bytes32 key, uint256 v) internal {
        _tSet[batchId][key] = true;
        _tU[batchId][key] = v;
        _touchTransientKey(batchId, key);
    }
}

