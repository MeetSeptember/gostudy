// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22;

import "./JoyueLib.sol";

/**
 * @title JoyueCache
 * @dev P2P 缓存访问合约。通过 precompile 访问和修改节点层面的 P2P 缓存。
 *
 * 工作流程：
 * 读取：
 * 1. 合约调用 get() 函数
 * 2. get() 调用 precompile (0x0000000000000000000000000000000000000100)
 * 3. precompile 从节点层面的 P2P 缓存获取数据
 * 4. 返回缓存值、版本号和是否存在
 *
 * 写入（代理合约使用）：
 * 1. 代理合约调用 setUint() 函数
 * 2. setUint() 调用 write-capable precompile (0x0000000000000000000000000000000000000101)
 * 3. precompile 更新节点层面的本地 P2P 缓存（不广播）
 * 4. 只有主合约的状态修改才会通过 StateBroadcast 事件广播
 *
 * 注意：
 * - 代理合约的缓存修改只更新本地节点缓存，不广播
 * - 主合约的状态修改会 emit StateBroadcast 事件，节点会广播到其他节点
 */
contract JoyueCache is IJoyueCache, IJoyueCacheWriter {
    // Precompile 地址：读缓存 (0x64 = 100)
    address private constant JOYUE_CACHE_PRECOMPILE_READ = address(0x64);
    // Precompile 地址：写缓存 (0x65 = 101)
    address private constant JOYUE_CACHE_PRECOMPILE_WRITE = address(0x65);

    /**
     * @dev 从 P2P 缓存获取值（通过 precompile）
     * @param contractAddr 合约地址
     * @param key 状态键
     * @return value 缓存值
     * @return version 版本号
     * @return ok 是否存在
     */
    function get(address contractAddr, bytes32 key) external view returns (bytes memory value, uint64 version, bool ok) {
        // 从 precompile 获取（P2P 缓存）
        bool success;
        bytes memory result;
        (success, result) = JOYUE_CACHE_PRECOMPILE_READ.staticcall(
            abi.encodePacked(contractAddr, key)
        );

        if (success && result.length >= 96) {
            // 解析 precompile 返回结果：abi.encode(bytes value, uint64 version, bool ok)
            // result[0:32] = offset for bytes
            // result[32:64] = length of bytes
            // result[64:96] = version (uint64, padded to 32 bytes)
            // result[96:128] = ok (bool, padded to 32 bytes)
            // result[128:] = bytes data

            // 提取 offset (前 32 字节)
            bytes memory offsetBytes = new bytes(32);
            for (uint256 i = 0; i < 32; i++) {
                offsetBytes[i] = result[i];
            }
            uint256 offset = abi.decode(offsetBytes, (uint256));
            
            if (offset == 96 && result.length >= 128) {
                // 提取 valueLen (32-64 字节)
                bytes memory valueLenBytes = new bytes(32);
                for (uint256 i = 0; i < 32; i++) {
                    valueLenBytes[i] = result[32 + i];
                }
                uint256 valueLen = abi.decode(valueLenBytes, (uint256));
                
                // 提取 version (64-96 字节)
                bytes memory versionBytes = new bytes(32);
                for (uint256 i = 0; i < 32; i++) {
                    versionBytes[i] = result[64 + i];
                }
                version = uint64(uint256(abi.decode(versionBytes, (uint256))));
                
                // 提取 ok (96-128 字节)
                bytes memory okBytes = new bytes(32);
                for (uint256 i = 0; i < 32; i++) {
                    okBytes[i] = result[96 + i];
                }
                ok = abi.decode(okBytes, (bool)) != false;

                if (ok && valueLen > 0 && result.length >= 128 + valueLen) {
                    // 提取 bytes 值
                    value = new bytes(valueLen);
                    for (uint256 i = 0; i < valueLen; i++) {
                        value[i] = result[128 + i];
                    }
                    return (value, version, ok);
                } else if (!ok) {
                    // 缓存未命中，返回空值
                    return ("", 0, false);
                }
            }
        }

        // Precompile 调用失败或格式不正确，返回空值
        return ("", 0, false);
    }

    /**
     * @dev 设置本地 P2P 缓存值（通过 write-capable precompile）
     * @param contractAddr 合约地址
     * @param key 状态键
     * @param value 缓存值（uint256）
     * @param version 版本号
     * 
     * 注意：此操作只更新本地节点缓存，不广播到其他节点
     * 只有主合约的状态修改（通过 StateBroadcast 事件）才会广播
     */
    function setUint(address contractAddr, bytes32 key, uint256 value, uint64 version) external {
        // 调用 write-capable precompile 更新本地缓存
        // 输入格式：contractAddr (20 bytes) + key (32 bytes) + value (32 bytes) + version (32 bytes, padded)
        // value 和 version 需要是 32 字节的 big-endian 格式
        bytes memory input = abi.encodePacked(
            contractAddr,
            key,
            uint256(value),  // 自动转换为 32 bytes
            uint256(version) // 自动转换为 32 bytes (uint64 会被 padded)
        );

        (bool success, ) = JOYUE_CACHE_PRECOMPILE_WRITE.call(input);
        require(success, "JOYUE: failed to update local cache");
    }
}


