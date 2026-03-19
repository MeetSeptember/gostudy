// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title TwoPhaseLib
 * @dev 2PC 专用：封装预编译 0x6D 调用，发出 CrossShardRequest 事件
 *      与 JOYUE 的 JoyueLib 分离，供 TwoPhaseCoordinator 使用
 */
library TwoPhaseLib {
    address public constant PRECOMPILE_RPC_ORACLE = address(0x6D);  // 发出 CrossShardRequest
    address public constant PRECOMPILE_EXECUTOR = address(0x74);   // Executor 预编译
    address public constant PRECOMPILE_CURRENT_SHARD = address(0x6C);

    /**
     * @dev 通过 0x6D 发出跨分片请求（目标为 Executor 0x74）
     */
    function emitCrossShardRequest(
        uint32 targetShardID,
        address targetAddr,
        bytes memory executorCalldata
    ) internal returns (bool success) {
        uint256 calldataLen = executorCalldata.length;
        uint256 totalLen = 60 + calldataLen;
        address precompileAddr = PRECOMPILE_RPC_ORACLE;

        assembly {
            let inputPtr := mload(0x40)
            mstore(0x40, add(inputPtr, add(0x20, totalLen)))

            mstore(inputPtr, totalLen)
            let dataPtr := add(inputPtr, 0x20)

            mstore8(dataPtr, shr(24, targetShardID))
            mstore8(add(dataPtr, 1), shr(16, targetShardID))
            mstore8(add(dataPtr, 2), shr(8, targetShardID))
            mstore8(add(dataPtr, 3), targetShardID)

            let addrPtr := add(dataPtr, 4)
            mstore(addrPtr, shl(96, targetAddr))

            let lenPtr := add(dataPtr, 56)
            mstore8(lenPtr, shr(24, calldataLen))
            mstore8(add(lenPtr, 1), shr(16, calldataLen))
            mstore8(add(lenPtr, 2), shr(8, calldataLen))
            mstore8(add(lenPtr, 3), calldataLen)

            if gt(calldataLen, 0) {
                let calldataPtr := add(dataPtr, 60)
                let calldataSrcPtr := add(executorCalldata, 0x20)
                let i := 0
                for { } lt(i, calldataLen) { i := add(i, 32) } {
                    mstore(add(calldataPtr, i), mload(add(calldataSrcPtr, i)))
                }
            }

            success := call(gas(), precompileAddr, 0, dataPtr, totalLen, 0, 0)
        }
    }

    function getCurrentShardID() internal view returns (uint32) {
        (bool ok, bytes memory ret) = PRECOMPILE_CURRENT_SHARD.staticcall("");
        if (!ok || ret.length < 32) return 0;
        uint32 id;
        assembly {
            id := and(mload(add(ret, 32)), 0xffffffff)
        }
        return id;
    }

    /**
     * @dev 构建 executeAndCallback 的 calldata（Executor 0x74 格式）
     */
    function buildExecutorCalldata(
        uint32 sourceShardID,
        address callbackAddr,
        bytes4 callbackSelector,
        uint256 requestId,
        address target,
        bytes memory targetCalldata
    ) internal pure returns (bytes memory) {
        return abi.encodeWithSelector(
            0x2b5d76a4,
            sourceShardID,
            callbackAddr,
            callbackSelector,
            requestId,
            target,
            uint256(0),
            targetCalldata
        );
    }
}
