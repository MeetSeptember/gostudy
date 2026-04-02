// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title SparrowIntentShop
 * @dev 用户侧「单笔」购买意图：只发事件，不触发 2PC。
 *      Relayer 监听 SparrowBuyIntent，在链下按买家/水果聚批后调用 SparrowCoordinator.buyFruitWave。
 *      测试：与 SparrowWallet 相同的 10 个演示账户；每笔意图按自增序号对 10 取模轮询选一个 buyer（与 msg.sender 无关，便于单私钥 trigger 稳定轮换多用户）。
 */
contract SparrowIntentShop {
    uint256 private constant _DEMO_LEN = 10;

    address[_DEMO_LEN] private _demoUsers;

    /// @dev 每发一笔意图 +1，用于 `_pickDemoBuyer()` 取模轮询
    uint256 private _demoBuyerNonce;

    mapping(address => uint256) public intentSeq;

    /// @dev intentId 即 metrics / Coordinator 使用的 tx_id（与 SparrowCoordinator.BuyItem.txId 对齐）
    event SparrowBuyIntent(
        address indexed buyer,
        bytes32 indexed fruitType,
        uint256 quantity,
        bytes32 intentId
    );

    /// @dev 与 SparrowWallet 构造函数内用户列表一致，便于 Intent 与 Wallet 余额侧对齐测试
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
        for (uint256 i = 0; i < _DEMO_LEN; i++) {
            _demoUsers[i] = users[i];
        }
    }

    /// @dev 本地测试：第 n 笔意图使用 `_demoUsers[n % 10]`，顺序确定、可复现
    function _pickDemoBuyer() internal returns (address buyer) {
        unchecked {
            uint256 idx = _demoBuyerNonce++ % _DEMO_LEN;
            buyer = _demoUsers[idx];
        }
    }

    function buyFruitIntent(bytes32 fruitType, uint256 quantity) public {
        require(quantity > 0, "SparrowIntent: qty");
        address buyer = _pickDemoBuyer();
        uint256 seq = ++intentSeq[buyer];
        bytes32 iid = keccak256(abi.encodePacked(buyer, fruitType, quantity, block.number, seq));
        emit SparrowBuyIntent(buyer, fruitType, quantity, iid);
    }

    function buyFruitIntentByName(string calldata fruitName, uint256 quantity) external {
        buyFruitIntent(keccak256(bytes(fruitName)), quantity);
    }
}
