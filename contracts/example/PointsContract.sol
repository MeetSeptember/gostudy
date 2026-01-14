// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/**
 * @title PointsContract
 * @dev 积分合约示例（可能部署在其他分片）
 */
contract PointsContract {
    mapping(address => uint256) public balances;
    mapping(address => uint256) public totalEarned; // 累计获得的积分

    event PointsAdded(address indexed user, uint256 amount, string reason);
    event PointsDeducted(address indexed user, uint256 amount, string reason);

    /**
     * @dev 增加积分
     * @param user 用户地址
     * @param amount 积分数量
     * @param reason 原因（例如："购买苹果"）
     */
    function addPoints(address user, uint256 amount, string memory reason) external {
        balances[user] += amount;
        totalEarned[user] += amount;
        emit PointsAdded(user, amount, reason);
    }

    /**
     * @dev 扣除积分
     * @param user 用户地址
     * @param amount 积分数量
     * @param reason 原因
     */
    function deductPoints(address user, uint256 amount, string memory reason) external {
        require(balances[user] >= amount, "insufficient points");
        balances[user] -= amount;
        emit PointsDeducted(user, amount, reason);
    }

    /**
     * @dev 查询积分余额
     */
    function getBalance(address user) external view returns (uint256) {
        return balances[user];
    }

    /**
     * @dev 查询累计获得的积分
     */
    function getTotalEarned(address user) external view returns (uint256) {
        return totalEarned[user];
    }
}

