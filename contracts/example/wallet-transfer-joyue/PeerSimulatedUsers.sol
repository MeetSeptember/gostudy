// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @dev 两 Master、两 Agent 共用的模拟账户表（写死规则，便于测试对齐）。
 *      地址由哈希派生，两链部署同一 Master 字节码时得到同一套地址与初始余额。
 */
library PeerSimulatedUsers {
    uint256 internal constant COUNT = 20;
    uint256 internal constant INITIAL_BALANCE = 1_000_000;

    function addr(uint256 i) internal pure returns (address) {
        require(i < COUNT, "JOYUE: peer idx");
        return address(uint160(uint256(keccak256(abi.encodePacked("JOYUE_PEER_WALLET_SIM", i)))));
    }

    function contains(address u) internal pure returns (bool) {
        for (uint256 i = 0; i < COUNT; i++) {
            if (addr(i) == u) return true;
        }
        return false;
    }
}
