// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/**
 * @title JoyueCrossShardExecutor
 * @dev 目标分片执行器：执行目标调用后，通过 RPC precompile(0x6D) 向源分片发送回调交易
 */
contract JoyueCrossShardExecutor {
    address private constant JOYUE_RPC_SEND_TX_PRECOMPILE = address(0x6D); // 109: RPC 发送交易

    event ExecutedAndCallbackSent(
        uint32 indexed sourceShardId,
        address indexed callbackAddr,
        uint256 indexed requestId,
        bool ok,
        bytes returnData,
        bytes32 callbackTxHash
    );

    /**
     * @dev 在目标分片执行目标调用，并向源分片回调
     */
    function executeAndCallback(
        uint32 sourceShardId,
        address callbackAddr,
        bytes4 callbackSelector,
        uint256 requestId,
        address target,
        uint256 targetValue,
        bytes calldata targetCalldata
    ) external payable returns (bool ok, bytes memory returnData, bytes32 callbackTxHash) {
        require(msg.value == targetValue, "msg.value != targetValue");

        (ok, returnData) = target.call{value: targetValue}(targetCalldata);

        // 使用辅助函数减少栈变量
        callbackTxHash = _sendCallback(sourceShardId, callbackAddr, callbackSelector, requestId, ok, returnData);

        emit ExecutedAndCallbackSent(sourceShardId, callbackAddr, requestId, ok, returnData, callbackTxHash);
    }

    /**
     * @dev 辅助函数：发送回调交易（减少主函数的栈变量）
     */
    function _sendCallback(
        uint32 sourceShardId,
        address callbackAddr,
        bytes4 callbackSelector,
        uint256 requestId,
        bool ok,
        bytes memory returnData
    ) private returns (bytes32 callbackTxHash) {
        bytes memory cbCalldata = abi.encodeWithSelector(callbackSelector, requestId, ok, returnData);
        bytes memory input = abi.encodePacked(
            uint32(sourceShardId),
            callbackAddr,
            uint256(0),
            uint32(cbCalldata.length),
            cbCalldata
        );

        bool sent;
        bytes memory ret;
        (sent, ret) = JOYUE_RPC_SEND_TX_PRECOMPILE.call(input);
        require(sent, "RPC callback tx send failed");
        require(ret.length >= 32, "bad callback tx hash");

        assembly {
            callbackTxHash := mload(add(ret, 32))
        }
    }
}


