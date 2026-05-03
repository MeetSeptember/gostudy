// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @dev 通用链上 revert 负载（Wallet verify、其它非 2PC 编排等），`reason` 与 `ChainspaceReason` 常量对齐；
 *      Executor 将 `returnData` 透传到回调 `data` 供解码。
 */
error ChainspaceRevert(uint8 reason);
