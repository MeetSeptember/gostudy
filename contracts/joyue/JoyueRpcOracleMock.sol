// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title JoyueRpcOracleMock
 * @dev Mock "RPC prefetch" oracle for coordinator/master to fetch latest authoritative state
 *      of other shard contracts. In real Harmony integration this should be replaced by:
 *      - node RPC prefetch during batch execution (off-chain), then injected to master execution context; OR
 *      - a protocol precompile that exposes authoritative reads.
 *
 * TODO(RPC Prefetch):
 * - Replace this mock with real cross-shard RPC fetch of (contractAddr,key)->(value,version).
 * - Ensure responses are authenticated / come from shard leaders / proofs as needed.
 */
contract JoyueRpcOracleMock {
    struct Entry {
        uint256 value;
        uint64 version;
        bool ok;
    }

    mapping(address => mapping(bytes32 => Entry)) internal _m;

    function setUint(address contractAddr, bytes32 key, uint256 value, uint64 version) external {
        _m[contractAddr][key] = Entry({value: value, version: version, ok: true});
    }

    function getUint(address contractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok) {
        Entry storage e = _m[contractAddr][key];
        return (e.value, e.version, e.ok);
    }
}


