// SPDX-License-Identifier: MIT
pragma solidity >=0.6.0 <0.9.0;

import "./JoyueLib.sol";
import "./JoyueStorageV2.sol";

/**
 * @title JoyueCoordinatorV2
 * @dev 流程层：使用预编译合约（0x73）作为执行引擎的 Shell Router。
 *
 * 通用透传：除 processIntent 外，所有对合约的调用（任意 selector + 参数）均通过 fallback
 * 原封不动 delegatecall 给预编译合约，由预编译合约根据 input[0:4] 解析选择器并分发。
 * 无需在 Solidity 中定义 batchVerifyAndFreeze、finalizeBatch、clearRound 等业务函数，
 * 也不存在 Solidity 侧对 ABI 的二次校验。
 * 预编译合约地址：0x0000000000000000000000000000000000000073 (0x73 = 115)
 */
contract JoyueCoordinatorV2 is JoyueStorageV2 {
    // 预编译合约地址
    address public constant PRECOMPILE_ADDRESS = address(0x73);

    enum FinalStatus {
        FINAL_COMMIT,
        FINAL_FAIL
    }

    struct FinalResult {
        bytes32 txHash;
        FinalStatus status;
        uint8 attemptsUsed;
    }

    // ============================================================
    //      通用透传：任意调用原样 delegatecall 到预编译合约
    // ============================================================
    /**
     * @dev 未匹配到任何函数时，将完整 msg.data（selector + 参数）delegatecall 给预编译合约。
     * 预编译合约根据 input[0:4] 解析选择器并分发（batchVerifyAndFreeze / finalizeBatch /
     * clearRound / onCrossShardCallback / processBundleWithOneRetry 等）。
     * 调用方（如 Executor）只需按预编译 ABI 构造 calldata，无需在合约中声明对应函数。
     * 需要 Solidity 0.6+ 以支持 fallback 返回 bytes。
     */
    fallback(bytes calldata) external returns (bytes memory) {
        (bool success, bytes memory returnData) = PRECOMPILE_ADDRESS.delegatecall(msg.data);
        require(success, "JOYUE: precompile delegatecall failed");
        return returnData;
    }

    /**
     * @dev 处理 Intent（标准接口，接收编码后的 payload）
     * 函数签名：processIntent(bytes)
     * 这是 Agent 通过 Relayer 发送 Intent 的统一入口
     */
    function processIntent(bytes calldata payload) external returns (FinalResult[] memory finals) {
        // 统一透传：由预编译合约解析 payload 并处理
        (bool success, bytes memory returnData) = PRECOMPILE_ADDRESS.delegatecall(msg.data);
        require(success, "JOYUE: processIntent delegatecall failed");

        finals = abi.decode(returnData, (FinalResult[]));
    }

}

