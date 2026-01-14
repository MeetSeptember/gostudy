// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "../baselib/JoyueCrossShardCall.sol";

/**
 * @title JoyueCrossShardAggregator
 * @dev 聚合请求管理器：等待多个跨分片回调完成后再继续执行
 *
 * 使用场景：
 * - 合约需要调用多个其他分片的合约
 * - 需要等所有回调都完成后才能继续执行后续逻辑
 *
 * 流程：
 * 1) 调用 sendAggregatedRequest(...) 发起多个跨分片请求
 * 2) 每个请求的回调都会调用 onCrossShardCallback(...)
 * 3) 当所有请求都回调完成时，自动触发 onAllCallbacksCompleted(...)
 */
contract JoyueCrossShardAggregator {
    using JoyueCrossShardCall for address;

    bytes4 public constant CALLBACK_SELECTOR = bytes4(keccak256("onCrossShardCallback(uint256,bool,bytes)"));

    struct AggregatedRequest {
        address requester;
        uint256 totalRequests;      // 总请求数
        uint256 completedRequests;   // 已完成的请求数
        uint256 successCount;        // 成功的请求数
        uint256 failedCount;         // 失败的请求数
        bool allCompleted;           // 是否全部完成
        mapping(uint256 => bool) requestCompleted;  // requestId -> 是否完成
        mapping(uint256 => bool) requestSuccess;    // requestId -> 是否成功
        mapping(uint256 => bytes) requestReturnData; // requestId -> 返回数据
    }

    mapping(uint256 => AggregatedRequest) public aggregatedRequests;
    mapping(uint256 => uint256) public requestIdToAggregatedId; // requestId -> aggregatedRequestId
    uint256 public nextAggregatedRequestId;

    event AggregatedRequestSent(
        uint256 indexed aggregatedRequestId,
        address indexed requester,
        uint256 totalRequests,
        bytes32[] sentTxHashes
    );

    event RequestCallbackReceived(
        uint256 indexed aggregatedRequestId,
        uint256 indexed requestId,
        bool ok,
        uint256 completedCount,
        uint256 totalCount
    );

    event AllCallbacksCompleted(
        uint256 indexed aggregatedRequestId,
        uint256 successCount,
        uint256 failedCount
    );

    /**
     * @notice 发起聚合跨分片请求（串行发送，避免并发数据库访问问题）
     * @param executors 目标分片 executor 地址数组
     * @param targetShardIDs 目标分片 ID 数组
     * @param targets 目标合约地址数组
     * @param targetCalldatas 目标调用 calldata 数组
     * @param targetValues 目标调用 value 数组
     */
    function sendAggregatedRequest(
        address[] memory executors,
        uint32[] memory targetShardIDs,
        address[] memory targets,
        bytes[] memory targetCalldatas,
        uint256[] memory targetValues
    ) public payable returns (uint256 aggregatedRequestId, bytes32[] memory sentTxHashes) {
        require(
            executors.length == targetShardIDs.length &&
            targetShardIDs.length == targets.length &&
            targets.length == targetCalldatas.length &&
            targetCalldatas.length == targetValues.length,
            "array length mismatch"
        );

        uint256 totalRequests = executors.length;
        require(totalRequests > 0, "no requests");

        aggregatedRequestId = ++nextAggregatedRequestId;

        // 初始化聚合请求
        AggregatedRequest storage aggReq = aggregatedRequests[aggregatedRequestId];
        aggReq.requester = msg.sender;
        aggReq.totalRequests = totalRequests;
        aggReq.completedRequests = 0;
        aggReq.successCount = 0;
        aggReq.failedCount = 0;
        aggReq.allCompleted = false;

        // 串行发送每个请求
        sentTxHashes = new bytes32[](totalRequests);
        for (uint256 i = 0; i < totalRequests; i++) {
            uint256 requestId = aggregatedRequestId * 1000000 + i; // 生成唯一的 requestId
            requestIdToAggregatedId[requestId] = aggregatedRequestId;

            // 串行调用跨分片写操作
            (bytes32 txHash, bool success) = JoyueCrossShardCall.callWriteWithCallback(
                executors[i],
                targetShardIDs[i],
                targets[i],
                targetCalldatas[i],
                targetValues[i],
                address(this),
                CALLBACK_SELECTOR,
                requestId
            );

            if (success) {
                sentTxHashes[i] = txHash;
            } else {
                // 发送失败，标记为失败回调（立即回调）
                bytes memory emptyReturnData;
                onCrossShardCallback(requestId, false, emptyReturnData);
            }
        }

        emit AggregatedRequestSent(aggregatedRequestId, msg.sender, totalRequests, sentTxHashes);
    }

    /**
     * @notice 回调函数（由目标分片 executor 触发）
     */
    function onCrossShardCallback(uint256 requestId, bool ok, bytes memory returnData) public {
        uint256 aggregatedRequestId = requestIdToAggregatedId[requestId];
        require(aggregatedRequestId != 0, "unknown requestId");

        AggregatedRequest storage aggReq = aggregatedRequests[aggregatedRequestId];
        require(!aggReq.allCompleted, "already all completed");
        require(!aggReq.requestCompleted[requestId], "request already completed");

        // 标记完成
        aggReq.requestCompleted[requestId] = true;
        aggReq.requestSuccess[requestId] = ok;
        aggReq.requestReturnData[requestId] = returnData;
        aggReq.completedRequests++;

        if (ok) {
            aggReq.successCount++;
        } else {
            aggReq.failedCount++;
        }

        emit RequestCallbackReceived(
            aggregatedRequestId,
            requestId,
            ok,
            aggReq.completedRequests,
            aggReq.totalRequests
        );

        // 检查是否全部完成
        if (aggReq.completedRequests == aggReq.totalRequests) {
            aggReq.allCompleted = true;
            emit AllCallbacksCompleted(aggregatedRequestId, aggReq.successCount, aggReq.failedCount);

            // 触发后续逻辑（可以在这里调用其他合约或执行业务逻辑）
            onAllCallbacksCompleted(aggregatedRequestId, aggReq);
        }
    }

    /**
     * @notice 所有回调完成后的处理函数（可被子类重写）
     */
    function onAllCallbacksCompleted(uint256 aggregatedRequestId, AggregatedRequest storage aggReq) internal virtual {
        // 默认实现：什么都不做
        // 子类可以重写此函数来实现业务逻辑
        // 例如：根据所有请求的结果执行后续操作
    }

    /**
     * @notice 查询单个请求的结果
     */
    function getRequestResult(uint256 requestId) external view returns (
        bool completed,
        bool success,
        bytes memory returnData
    ) {
        uint256 aggregatedRequestId = requestIdToAggregatedId[requestId];
        if (aggregatedRequestId == 0) {
            return (false, false, "");
        }

        AggregatedRequest storage aggReq = aggregatedRequests[aggregatedRequestId];
        completed = aggReq.requestCompleted[requestId];
        success = aggReq.requestSuccess[requestId];
        returnData = aggReq.requestReturnData[requestId];
    }

    /**
     * @notice 查询聚合请求的状态
     */
    function getAggregatedRequestStatus(uint256 aggregatedRequestId) external view returns (
        address requester,
        uint256 totalRequests,
        uint256 completedRequests,
        uint256 successCount,
        uint256 failedCount,
        bool allCompleted
    ) {
        AggregatedRequest storage aggReq = aggregatedRequests[aggregatedRequestId];
        require(aggReq.requester != address(0), "unknown aggregatedRequestId");

        return (
            aggReq.requester,
            aggReq.totalRequests,
            aggReq.completedRequests,
            aggReq.successCount,
            aggReq.failedCount,
            aggReq.allCompleted
        );
    }
}

