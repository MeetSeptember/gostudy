// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/**
 * @title JoyueCrossShardCall
 * @dev 跨分片合约调用库（RPC 异步 + RPC 回调）
 * 
 * 使用方式：
 * - 读操作：callView(target, targetShardID, data)
 * - 写操作：callWriteWithCallback(...) - 通过目标分片 Executor 执行并自动回调源分片
 * 
 * Precompile 地址：
 * - 0x67 (103): RPC Oracle 查询（读）
 * - 0x6C (108): 获取当前分片 ID
 * - 0x6D (109): RPC Oracle 发送交易（写）
 */
library JoyueCrossShardCall {
    // Precompile 地址
    address private constant RPC_ORACLE_PRECOMPILE = address(0x67);  // 103: RPC Oracle 查询
    address private constant CURRENT_SHARD_PRECOMPILE = address(0x6C); // 108: 获取当前分片 ID
    address private constant RPC_SEND_TX_PRECOMPILE = address(0x6D);  // 109: RPC 发送交易
    
    /**
     * @dev 获取当前分片 ID
     * @return shardID 当前分片 ID
     */
    function getCurrentShardID() internal view returns (uint32 shardID) {
        bool success;
        bytes memory result;
        (success, result) = CURRENT_SHARD_PRECOMPILE.staticcall("");
        require(success, "Failed to get current shard ID");
        assembly {
            shardID := mload(add(result, 28)) // 最后 4 bytes
        }
    }
    
    /**
     * @dev 跨分片 View 调用（读操作）
     * @param target 目标合约地址
     * @param targetShardID 目标分片 ID
     * @param data 函数调用的 calldata
     * @return result 函数返回值
     * @return success 是否成功
     */
    function callView(
        address target,
        uint32 targetShardID,
        bytes memory data
    ) internal view returns (bytes memory result, bool success) {
        uint32 currentShardID = getCurrentShardID();
        
        if (targetShardID == currentShardID) {
            // 同一分片：使用标准 staticcall
            (success, result) = target.staticcall(data);
        } else {
            // 跨分片：使用 RPC Oracle precompile
            // 输入格式：4 bytes (shardID) + 20 bytes (contractAddr) + 4 bytes (calldataLen) + calldata
            bytes memory input = abi.encodePacked(
                uint32(targetShardID),
                target,
                uint32(data.length),
                data
            );
            
            (success, result) = RPC_ORACLE_PRECOMPILE.staticcall(input);
        }
    }
    
    /**
     * @dev 跨分片写：通过目标分片 Executor 执行，并在目标分片内用 RPC 回调源分片
     *
     * @param executor 目标分片已部署的 JoyueCrossShardExecutor 地址
     * @param targetShardID 目标分片 ID
     * @param target 目标合约地址（位于 targetShardID）
     * @param targetCalldata 调用 target 的 calldata
     * @param targetValue 调用 target 时携带的 value（会作为 msg.value 转给 executor）
     * @param callbackAddr 源分片回调合约地址（通常是发起方/请求管理器）
     * @param callbackSelector 回调 selector，例如 bytes4(keccak256("onCrossShardCallback(uint256,bool,bytes)"))
     * @param requestId 源分片生成的 requestId，用于回调时读取链上保存的上下文
     *
     * @return txHash 发往目标分片 Executor 的交易 hash（异步）
     * @return success 是否成功把交易发送到目标分片（仅代表“发送成功”，不代表目标执行成功）
     */
    function callWriteWithCallback(
        address executor,
        uint32 targetShardID,
        address target,
        bytes memory targetCalldata,
        uint256 targetValue,
        address callbackAddr,
        bytes4 callbackSelector,
        uint256 requestId
    ) internal returns (bytes32 txHash, bool success) {
        uint32 sourceShardID = getCurrentShardID();

        // 同分片：直接执行并同步回调
        if (targetShardID == sourceShardID) {
            (bool ok, bytes memory ret) = target.call{value: targetValue}(targetCalldata);
            bytes memory cb = abi.encodeWithSelector(callbackSelector, requestId, ok, ret);
            (success, ) = callbackAddr.call(cb);
            txHash = bytes32(0);
            return (txHash, success);
        }

        // 跨分片：构造对 Executor.executeAndCallback(...) 的 calldata
        // 目标分片 Executor 将在目标分片内执行 target.call，然后用 precompile(0x6D) 回调源分片
        bytes memory execCalldata = abi.encodeWithSignature(
            "executeAndCallback(uint32,address,bytes4,uint256,address,uint256,bytes)",
            uint32(sourceShardID),
            callbackAddr,
            callbackSelector,
            requestId,
            target,
            targetValue,
            targetCalldata
        );

        // 通过 RPC SendTx precompile 发送到目标分片的 executor
        // input = 4 bytes (shardID) + 20 bytes (to) + 32 bytes (value) + 4 bytes (calldataLen) + calldata
        bytes memory input = abi.encodePacked(
            uint32(targetShardID),
            executor,
            targetValue,
            uint32(execCalldata.length),
            execCalldata
        );

        bytes memory result;
        (success, result) = RPC_SEND_TX_PRECOMPILE.call(input);
        if (success && result.length >= 32) {
            assembly {
                txHash := mload(add(result, 32))
            }
        }
    }

}

