// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/**
 * @title CurrencyContract
 * @dev 货币合约示例（可能部署在其他分片）
 */
contract CurrencyContract {
    mapping(address => uint256) public balances;

    event Transfer(address indexed from, address indexed to, uint256 amount, string reason);
    event Deposit(address indexed user, uint256 amount);
    event Withdraw(address indexed user, uint256 amount);

    /**
     * @dev 转账
     * @param from 发送方地址
     * @param to 接收方地址
     * @param amount 金额
     * @param reason 原因（例如："购买苹果"）
     */
    function transfer(address from, address to, uint256 amount, string memory reason) external {
        require(balances[from] >= amount, "insufficient balance");
        balances[from] -= amount;
        balances[to] += amount;
        emit Transfer(from, to, amount, reason);
    }

    /**
     * @dev 扣除余额（用于支付）
     * @param user 用户地址
     * @param amount 金额
     * @param reason 原因
     */
    function deduct(address user, uint256 amount, string memory reason) external {
        require(balances[user] >= amount, "insufficient balance");
        balances[user] -= amount;
        emit Transfer(user, address(0), amount, reason);
    }

    /**
     * @dev 充值
     * @param user 用户地址
     * @param amount 金额
     */
    function deposit(address user, uint256 amount) external {
        balances[user] += amount;
        emit Deposit(user, amount);
    }

    /**
     * @dev 查询余额
     */
    function getBalance(address user) external view returns (uint256) {
        return balances[user];
    }
}

