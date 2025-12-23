// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "./JoyueLib.sol";

/**
 * @title JoyueMockCache
 * @dev Mock cache for local testing. In production, cache should be node-provided P2P cache.
 *
 * TODO(P2P Cache):
 * - This contract will be replaced by a protocol-provided cache interface (possibly a precompile).
 */
contract JoyueMockCache is IJoyueCache {
    struct Entry {
        bytes value;
        uint64 version;
        bool ok;
    }

    mapping(address => mapping(bytes32 => Entry)) internal _cache;

    function set(address contractAddr, bytes32 key, bytes calldata value, uint64 version) external {
        _cache[contractAddr][key] = Entry({value: value, version: version, ok: true});
    }

    function setUint(address contractAddr, bytes32 key, uint256 value, uint64 version) external {
        _cache[contractAddr][key] = Entry({value: abi.encode(value), version: version, ok: true});
    }

    function get(address contractAddr, bytes32 key) external view returns (bytes memory value, uint64 version, bool ok) {
        Entry storage e = _cache[contractAddr][key];
        return (e.value, e.version, e.ok);
    }
}


