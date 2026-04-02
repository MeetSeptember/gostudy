// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowWallet
 * @dev 同 batchId + buyer：若被其它 batch 占用则 revert；否则 **先占锁** 再逐行判余额，行结果经 returnData 回协调者。
 *      任一行失败则不扣款、不写 prepareLock，但锁保留至协调者 commit/abort；二者均释放锁（无 prepare 时仅清锁）。
 */
contract SparrowWallet {
    mapping(address => uint256) public balance;

    mapping(address => bytes32) public lockBuyer;
    mapping(bytes32 => PrepareLock) public prepareLocks;
    /// @dev 本 batch 关联的买家，用于仅有锁无 prepareLock 时的 commit/abort 清锁
    mapping(bytes32 => address) public prepareBatchUser;

    struct PrepareLock {
        address user;
        uint256 totalAmount;
        bool exists;
    }

    event PreparedBatchLines(bytes32 indexed batchId, address indexed user, bool[] lineOk);
    event Committed(bytes32 indexed batchId);
    event Aborted(bytes32 indexed batchId);

    /// @dev 部署时初始化 10 个演示用户（Hardhat/Anvil 默认账号 0–9，便于本地用已知私钥签名）
    constructor() {
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
            uint256 rnd = uint256(keccak256(abi.encodePacked("SparrowWalletInit", i)));
            balance[users[i]] = 300 + (rnd % 2200);
        }
    }

    function setBalance(address user, uint256 amount) external {
        balance[user] = amount;
    }

    function prepareBatch(bytes32 batchId, address user, uint256[] calldata amounts)
        external
        returns (bool allOk, bool[] memory lineOk)
    {
        require(amounts.length > 0, "SparrowW: empty batch");
        require(lockBuyer[user] == bytes32(0) || lockBuyer[user] == batchId, "SparrowW: buyer locked");
        require(!prepareLocks[batchId].exists, "SparrowW: batch exists");

        if (lockBuyer[user] == bytes32(0)) {
            lockBuyer[user] = batchId;
        }
        prepareBatchUser[batchId] = user;

        lineOk = new bool[](amounts.length);
        uint256 rem = balance[user];
        allOk = true;
        for (uint256 i = 0; i < amounts.length; i++) {
            uint256 a = amounts[i];
            if (rem >= a) {
                unchecked {
                    rem -= a;
                }
                lineOk[i] = true;
            } else {
                lineOk[i] = false;
                allOk = false;
            }
        }

        emit PreparedBatchLines(batchId, user, lineOk);

        if (allOk) {
            uint256 total = balance[user] - rem;
            balance[user] = rem;
            prepareLocks[batchId] = PrepareLock({ user: user, totalAmount: total, exists: true });
        }
        return (allOk, lineOk);
    }

    function commitBatch(bytes32 batchId) external {
        address user = prepareBatchUser[batchId];
        require(user != address(0), "SparrowW: unknown batch");
        require(lockBuyer[user] == batchId, "SparrowW: lock mismatch");
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
        require(lockBuyer[user] == batchId, "SparrowW: lock mismatch");
        if (prepareLocks[batchId].exists) {
            PrepareLock memory lock = prepareLocks[batchId];
            balance[lock.user] += lock.totalAmount;
            delete prepareLocks[batchId];
        }
        lockBuyer[user] = bytes32(0);
        delete prepareBatchUser[batchId];
        emit Aborted(batchId);
    }
}
