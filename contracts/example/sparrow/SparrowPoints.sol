// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowPoints
 * @dev 与 SparrowWallet 对称：未被其它 batch 占用时 **先占锁**，再逐行判溢出；全成功才加积分与 prepareLock；commit/abort 均清锁（含仅锁无记账）。
 */
contract SparrowPoints {
    mapping(address => uint256) public points;

    mapping(address => bytes32) public lockBuyer;
    mapping(bytes32 => PrepareLock) public prepareLocks;
    mapping(bytes32 => address) public prepareBatchUser;

    struct PrepareLock {
        address user;
        uint256 amount;
        bool exists;
    }

    event PreparedBatchLines(bytes32 indexed batchId, address indexed user, bool[] lineOk);
    event Committed(bytes32 indexed batchId);
    event Aborted(bytes32 indexed batchId);

    /// @dev 部署时初始化 10 个演示用户（Hardhat Network 默认 account 0..9，与 SparrowWallet 一致）
    constructor() {
        // Hardhat Network 默认助记词下 account 0..9（见 hardhat.org hardhat-network reference）
        address[10] memory users = [
            0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266,
            0x70997970C51812dc3A010C7d01b50e0d17dc79C8,
            0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC,
            0x90F79bf6EB2c4f870365E785982E1f101E93b906,
            0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65,
            0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc,
            0x976EA74026E726554dB657fA54763abd0C3a0aa9,
            0x14dC79964da2C08b23698B3D3cc7Ca32193d9955,
            0x23618e81E3f5cdF7f54C3d65f7FBc0aBf5B21E8f,
            0xa0Ee7A142d267C1f36714E4a8F75612F20a79720
        ];
        for (uint256 i = 0; i < 10; i++) {
            uint256 rnd = uint256(keccak256(abi.encodePacked("SparrowPointsInit", i)));
            points[users[i]] = rnd % 550;
        }
    }

    function setPoints(address user, uint256 amount) external {
        points[user] = amount;
    }

    /// @param amounts 本批内每笔加积分（顺序与协调者波内该买家的行一致）；逐行检测溢出，任一行失败则整批不写状态。
    function prepareBatch(bytes32 batchId, address user, uint256[] calldata amounts)
        external
        returns (bool allOk, bool[] memory lineOk)
    {
        require(amounts.length > 0, "SparrowP: empty batch");
        require(lockBuyer[user] == bytes32(0) || lockBuyer[user] == batchId, "SparrowP: buyer locked");
        require(!prepareLocks[batchId].exists, "SparrowP: batch exists");

        if (lockBuyer[user] == bytes32(0)) {
            lockBuyer[user] = batchId;
        }
        prepareBatchUser[batchId] = user;

        lineOk = new bool[](amounts.length);
        uint256 sim = points[user];
        allOk = true;
        for (uint256 i = 0; i < amounts.length; i++) {
            uint256 a = amounts[i];
            if (a > type(uint256).max - sim) {
                lineOk[i] = false;
                allOk = false;
            } else {
                sim += a;
                lineOk[i] = true;
            }
        }

        emit PreparedBatchLines(batchId, user, lineOk);

        if (allOk) {
            uint256 total = sim - points[user];
            points[user] = sim;
            prepareLocks[batchId] = PrepareLock({ user: user, amount: total, exists: true });
        }
        return (allOk, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        address user = prepareBatchUser[batchId];
        require(user != address(0), "SparrowP: unknown batch");
        require(lockBuyer[user] == batchId, "SparrowP: lock mismatch");
        if (prepareLocks[batchId].exists) {
            delete prepareLocks[batchId];
        }
        lockBuyer[user] = bytes32(0);
        delete prepareBatchUser[batchId];
        emit Committed(batchId);
    }

    function abortBatch(bytes32 batchId) external {
        address user = prepareBatchUser[batchId];
        if (user == address(0)) {
            return;
        }
        require(lockBuyer[user] == batchId, "SparrowP: lock mismatch");
        if (prepareLocks[batchId].exists) {
            PrepareLock memory lock = prepareLocks[batchId];
            points[lock.user] -= lock.amount;
            delete prepareLocks[batchId];
        }
        lockBuyer[user] = bytes32(0);
        delete prepareBatchUser[batchId];
        emit Aborted(batchId);
    }
}
