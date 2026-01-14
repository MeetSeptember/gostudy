// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22;

/**
 * @title JoyueRpcOracle
 * @dev RPC Oracle 用于通过 RPC 查询其他分片的主合约权威状态（非缓存）
 *
 * 工作流程：
 * 1. 合约调用 getUint() 函数
 * 2. getUint() 调用 precompile (0x0000000000000000000000000000000000000103)
 * 3. Precompile 通过 RPC 查询指定分片的主合约状态
 * 4. 返回权威状态值、版本号和是否存在标志
 *
 * 注意：
 * - 这是查询权威状态（从其他分片的节点直接查询），不是缓存
 * - 需要配置其他分片的 RPC 地址
 * - 当前实现不考虑安全性，仅实现基本功能
 */
contract JoyueRpcOracle {
    // Precompile 地址：0x0000000000000000000000000000000000000103 (0x67 = 103)
    address private constant JOYUE_RPC_ORACLE_PRECOMPILE = address(0x67);

    /**
     * @dev 通过 RPC 查询其他分片的主合约状态（调用 getState 函数）
     * @param shardID 目标分片 ID
     * @param contractAddr 主合约地址
     * @param key 状态键
     * @return value 状态值
     * @return version 版本号
     * @return ok 是否存在
     */
    function getUint(uint32 shardID, address contractAddr, bytes32 key) external view returns (uint256 value, uint64 version, bool ok) {
        // 使用 callGetter 调用 getState(bytes32 key) 函数
        // getState 的签名：getState(bytes32) returns (uint256, uint64)
        // 函数选择器：keccak256("getState(bytes32)") 的前 4 字节 = 0x09648a9d
        bytes4 selector = bytes4(0x09648a9d);
        
        // 构造 calldata：selector + key
        bytes memory calldata_ = abi.encodePacked(selector, key);
        
        // 调用 callGetter
        bytes memory result = this.callGetter(shardID, contractAddr, calldata_);
        
        // 检查返回值长度
        // getState 返回 (uint256, uint64) = 32 + 32 = 64 字节
        if (result.length < 64) {
            return (0, 0, false);
        }
        
        // 解析返回值：abi.decode(result, (uint256, uint64))
        // result[0:32] = value (uint256)
        // result[32:64] = version (uint64, padded to 32 bytes)
        
        // 提取 value (前 32 字节)
        bytes memory valueBytes = new bytes(32);
        for (uint256 i = 0; i < 32; i++) {
            valueBytes[i] = result[i];
        }
        value = abi.decode(valueBytes, (uint256));
        
        // 提取 version (32-64 字节)
        bytes memory versionBytes = new bytes(32);
        for (uint256 i = 0; i < 32; i++) {
            versionBytes[i] = result[32 + i];
        }
        version = uint64(uint256(abi.decode(versionBytes, (uint256))));
        
        // 如果 value 和 version 都是 0，可能表示不存在
        // 但这里我们假设调用成功就返回 true
        ok = true;
        
        return (value, version, ok);
    }

    /**
     * @dev 通过 RPC 调用其他分片的主合约 getter 函数
     * @param shardID 目标分片 ID
     * @param contractAddr 主合约地址
     * @param calldata_ 函数调用的 calldata（函数选择器 + 参数）
     * @return result 函数返回值
     */
    function callGetter(uint32 shardID, address contractAddr, bytes calldata calldata_) external view returns (bytes memory result) {
        // 调用 precompile
        // 输入格式：4 bytes (shardID) + 20 bytes (contractAddr) + 4 bytes (calldataLen) + calldata
        bytes memory input = abi.encodePacked(
            uint32(shardID),
            contractAddr,
            uint32(calldata_.length),
            calldata_
        );

        bool success;
        bytes memory ret;
        (success, ret) = JOYUE_RPC_ORACLE_PRECOMPILE.staticcall(input);

        if (!success) {
            return "";
        }

        return ret;
    }
}


