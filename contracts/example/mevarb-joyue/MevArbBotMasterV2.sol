    // SPDX-License-Identifier: MIT
    pragma solidity ^0.8.0;

    import "../../baselib/JoyueCoordinatorV2.sol";

    /**
     * @title MevArbBotMasterV2
     * @dev **Bot Master（编排锚点）**：无业务状态，仅提供 `executeArbIntent` 作为 JOYUE RawRequest 锚点与跨分片 Intent 投递目标。
     *      与 `MevArbFlashLenderMasterV2`（贷款池）分离，避免「贷款与 bot 编排」混在同一合约。
     */
    contract MevArbBotMasterV2 is JoyueCoordinatorV2 {
        /**
         * @dev RawRequest 占位：由 `MevArbBotAgentV2` 生成 Guard/Delta；参数与 Agent 入口一致。
         */
        function executeArbIntent(address user, uint256 borrowA, uint256 minNetProfitA) external pure returns (address, uint256, uint256) {
            return (user, borrowA, minNetProfitA);
        }
    }
