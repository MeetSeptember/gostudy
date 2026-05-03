// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title ChainspaceReason
 * @dev 跨 chainspace 编排与 metrics 共用的腿级/笔级 `uint8` 约定（`onParticipantFinished` / `*IntentFinalized` / `ChainspaceRevert`）。
 */
library ChainspaceReason {
    uint8 internal constant OK = 1;
    uint8 internal constant OTHER = 2;
    uint8 internal constant LOCK_CONFLICT = 3;
    uint8 internal constant BUSINESS_RULE = 4;
    uint8 internal constant FOLLOWER_ABORT = 5;
}
