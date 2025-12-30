package consensus

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	proto_node "github.com/harmony-one/harmony/api/proto/node"
	"github.com/harmony-one/harmony/block"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/types"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/harmony/staking/availability"
	"github.com/harmony-one/harmony/webhooks"
)

// postConsensusProcessing is called by consensus participants, after consensus is done, to:
// 1. [leader] send new block to the client
// 2. [leader] send cross shard tx receipts to destination shard
func (consensus *Consensus) postConsensusProcessing(newBlock *types.Block) error {
	if consensus.IsLeader() {
		if IsRunningBeaconChain(consensus) {
			// TODO: consider removing this and letting other nodes broadcast new blocks.
			// But need to make sure there is at least 1 node that will do the job.
			BroadcastNewBlock(consensus.host, newBlock, consensus.registry.GetNodeConfig())
		}
		BroadcastCXReceipts(newBlock, consensus)
	} else {
		if mode := consensus.mode(); mode != Listening {
			numSignatures := consensus.NumSignaturesIncludedInBlock(newBlock)
			consensus.getLogger().Info().
				Uint64("blockNum", newBlock.NumberU64()).
				Uint64("epochNum", newBlock.Epoch().Uint64()).
				Uint64("ViewId", newBlock.Header().ViewID().Uint64()).
				Str("blockHash", newBlock.Hash().String()).
				Int("numTxns", len(newBlock.Transactions())).
				Int("numStakingTxns", len(newBlock.StakingTransactions())).
				Uint32("numSignatures", numSignatures).
				Str("mode", mode.String()).
				Msg("BINGO !!! Reached Consensus")
			if consensus.mode() == Syncing {
				mode = consensus.updateConsensusInformation("consensus.mode() == Syncing")
				consensus.getLogger().Info().Msgf("Switching to mode %s", mode)
				consensus.setMode(mode)
			}

			consensus.UpdateValidatorMetrics(float64(numSignatures), float64(newBlock.NumberU64()))

			// 1% of the validator also need to do broadcasting
			rnd := rand.Intn(100)
			if rnd < 1 {
				// Beacon validators also broadcast new blocks to make sure beacon sync is strong.
				if IsRunningBeaconChain(consensus) {
					BroadcastNewBlock(consensus.host, newBlock, consensus.registry.GetNodeConfig())
				}
				BroadcastCXReceipts(newBlock, consensus)
			}
		}
	}

	// Broadcast client requested missing cross shard receipts if there is any
	BroadcastMissingCXReceipts(consensus)

	if h := consensus.registry.GetNodeConfig().WebHooks.Hooks; h != nil {
		if h.Availability != nil {
			shardState, err := consensus.Blockchain().ReadShardState(newBlock.Epoch())
			if err != nil {
				consensus.getLogger().Error().Err(err).
					Int64("epoch", newBlock.Epoch().Int64()).
					Uint32("shard-id", consensus.ShardID).
					Msg("failed to read shard state")
				return err
			}

			for _, addr := range consensus.Registry().GetAddressToBLSKey().GetAddresses(consensus.getPublicKeys(), shardState, newBlock.Epoch()) {
				wrapper, err := consensus.Beaconchain().ReadValidatorInformation(addr)
				if err != nil {
					consensus.getLogger().Err(err).Str("addr", addr.Hex()).Msg("failed reaching validator info")
					return nil
				}
				snapshot, err := consensus.Beaconchain().ReadValidatorSnapshot(addr)
				if err != nil {
					consensus.getLogger().Err(err).Str("addr", addr.Hex()).Msg("failed reaching validator snapshot")
					return nil
				}
				computed := availability.ComputeCurrentSigning(
					snapshot.Validator, wrapper, consensus.Blockchain().Config().IsHIP32(newBlock.Epoch()),
				)
				lastBlockOfEpoch := shard.Schedule.EpochLastBlock(consensus.Beaconchain().CurrentBlock().Header().Epoch().Uint64())

				computed.BlocksLeftInEpoch = lastBlockOfEpoch - consensus.Beaconchain().CurrentBlock().Header().Number().Uint64()

				if err != nil && computed.IsBelowThreshold {
					url := h.Availability.OnDroppedBelowThreshold
					go func() {
						webhooks.DoPost(url, computed)
					}()
				}
			}
		}
	}

	// 检查是否有新的主合约部署（仅在共识完成后）
	nodeConfig := consensus.registry.GetNodeConfig()
	if nodeConfig.Joyue.AutoDeployEnabled {
		go checkAndDeployAgents(consensus, newBlock)
	}

	return nil
}

func checkAndDeployAgents(consensus *Consensus, block *types.Block) {
	// 1. 遍历区块中的所有交易
	receipts := consensus.Blockchain().GetReceiptsByHash(block.Hash())
	for i, tx := range block.Transactions() {
		// 2. 检查是否是合约创建交易
		if tx.To() != nil {
			continue
		}

		// 3. 获取该交易的 receipt
		if i >= len(receipts) {
			continue
		}
		receipt := receipts[i]

		// 4. 检查 receipt 是否成功且包含合约地址
		if receipt.Status != 1 || receipt.ContractAddress == (common.Address{}) {
			continue
		}

		// 5. 检查是否是 JoyueMaster 合约（通过事件）
		if isJoyueMaster(receipt.Logs) {
			utils.Logger().Info().
				Str("master", receipt.ContractAddress.Hex()).
				Str("tx", tx.Hash().Hex()).
				Uint64("block", block.NumberU64()).
				Msg("[JOYUE] detected JoyueMaster deployment")
			triggerAgentDeployment(receipt.ContractAddress, tx, receipt, consensus)
		}
	}
}

// masterDeployedEvent 是 MasterDeployed 事件解析后的结构
type masterDeployedEvent struct {
	Master            common.Address
	Salt              [32]byte
	AgentCreationCode []byte
	MasterShardID     uint32
}

// triggerAgentDeployment 从事件中解析 agentCreationCode，然后向其他分片发送部署交易
func triggerAgentDeployment(
	masterAddr common.Address,
	tx *types.Transaction,
	receipt *types.Receipt,
	consensus *Consensus,
) {
	// 1. 从 receipt.Logs 中解析 MasterDeployed 事件
	evt, err := parseMasterDeployedEvent(receipt.Logs)
	if err != nil {
		utils.Logger().Error().Err(err).Str("master", masterAddr.Hex()).Msg("[JOYUE] failed to parse MasterDeployed event")
		return
	}

	nodeConfig := consensus.registry.GetNodeConfig()

	// 2. 解析其他分片的 RPC 地址
	shardRPCs, err := parseShardRPCs(nodeConfig.Joyue.OtherShardRPCs)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[JOYUE] failed to parse other shard RPCs")
		return
	}

	// 3. 解析部署私钥
	privKey, err := parseDeployPrivateKey(nodeConfig.Joyue.DeployPrivateKey)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[JOYUE] failed to parse deploy private key")
		return
	}

	// 4. 向每个分片发送部署交易
	deployCount := 0
	for _, shardRPC := range shardRPCs {
		// 跳过 master shard（已经在 master shard 部署了）
		if shardRPC.ShardID == evt.MasterShardID {
			continue
		}
		deployCount++
		go deployAgentToShard(
			shardRPC.ShardID,
			shardRPC.RPCURL,
			evt.AgentCreationCode,
			masterAddr,
			evt.MasterShardID,
			privKey,
			consensus,
		)
	}

	if deployCount > 0 {
		utils.Logger().Info().
			Str("master", masterAddr.Hex()).
			Uint32("masterShard", evt.MasterShardID).
			Int("scheduledDeployments", deployCount).
			Msg("[JOYUE] scheduled agent deployments to other shards")
	}
}

// parseMasterDeployedEvent 从 logs 中解析 MasterDeployed 事件
func parseMasterDeployedEvent(logs []*types.Log) (*masterDeployedEvent, error) {
	// MasterDeployed 事件的签名（完整的 hash，32 字节）
	masterDeployedSig := common.BytesToHash(crypto.Keccak256([]byte("MasterDeployed(address,bytes32,bytes,uint32)")))

	// 定义事件 ABI（用于解析）
	eventABI := `[{"anonymous":false,"inputs":[{"indexed":true,"name":"master","type":"address"},{"indexed":true,"name":"salt","type":"bytes32"},{"indexed":false,"name":"agentCreationCode","type":"bytes"},{"indexed":false,"name":"masterShardId","type":"uint32"}],"name":"MasterDeployed","type":"event"}]`

	parsedABI, err := abi.JSON(strings.NewReader(eventABI))
	if err != nil {
		return nil, fmt.Errorf("failed to parse ABI: %w", err)
	}

	evt := parsedABI.Events["MasterDeployed"]

	// 查找匹配的 log
	for _, log := range logs {
		if len(log.Topics) == 0 {
			continue
		}
		if log.Topics[0] != masterDeployedSig {
			continue
		}

		// topics: [sig, indexed master, indexed salt]
		if len(log.Topics) < 3 {
			continue
		}

		result := &masterDeployedEvent{
			Master: common.BytesToAddress(log.Topics[1].Bytes()[12:]),
		}
		copy(result.Salt[:], log.Topics[2].Bytes())

		// 非 indexed：agentCreationCode(bytes) + masterShardId(uint32)
		vals, err := evt.Inputs.NonIndexed().Unpack(log.Data)
		if err != nil {
			utils.Logger().Error().Err(err).Msg("[JOYUE] failed to unpack MasterDeployed event data")
			continue
		}
		if len(vals) != 2 {
			utils.Logger().Warn().Int("valsCount", len(vals)).Msg("[JOYUE] unexpected number of unpacked values")
			continue
		}

		// agentCreationCode（动态 bytes）
		switch v := vals[0].(type) {
		case []byte:
			result.AgentCreationCode = append(result.AgentCreationCode[:0:0], v...)
			// 验证：如果长度是 2952，说明解析错误（应该是 2334）
			if len(result.AgentCreationCode) == 2952 {
				utils.Logger().Error().Int("agentCodeLen", len(result.AgentCreationCode)).Msg("[JOYUE] parsed agentCreationCode length matches master contract code length, this is wrong")
			}
		default:
			utils.Logger().Warn().Str("type", fmt.Sprintf("%T", v)).Msg("[JOYUE] agentCreationCode type mismatch")
			continue
		}

		// masterShardId
		switch v := vals[1].(type) {
		case uint32:
			result.MasterShardID = v
		case uint64:
			result.MasterShardID = uint32(v)
		case *big.Int:
			result.MasterShardID = uint32(v.Uint64())
		default:
			continue
		}

		return result, nil
	}

	return nil, fmt.Errorf("MasterDeployed event not found in logs")
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// shardRPC 表示一个分片的 RPC 地址
type shardRPC struct {
	ShardID uint32
	RPCURL  string
}

// parseShardRPCs 解析配置中的分片 RPC 地址
// 格式：shardID=rpcURL,shardID=rpcURL
// 例如：1=http://127.0.0.1:9501,2=http://127.0.0.1:9502
func parseShardRPCs(s string) ([]shardRPC, error) {
	if s == "" {
		return nil, fmt.Errorf("empty shard RPCs config")
	}

	parts := strings.Split(s, ",")
	result := make([]shardRPC, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid shard RPC entry: %q", p)
		}

		shardIDStr := strings.TrimSpace(kv[0])
		rpcURL := strings.TrimSpace(kv[1])

		shardID, err := strconv.ParseUint(shardIDStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid shard ID %q: %w", shardIDStr, err)
		}

		if rpcURL == "" {
			return nil, fmt.Errorf("empty RPC URL for shard %d", shardID)
		}

		result = append(result, shardRPC{
			ShardID: uint32(shardID),
			RPCURL:  rpcURL,
		})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no valid shard RPCs found")
	}

	return result, nil
}

// parseDeployPrivateKey 解析部署私钥（hex，不带 0x）
func parseDeployPrivateKey(s string) (*ecdsa.PrivateKey, error) {
	if s == "" {
		return nil, fmt.Errorf("empty deploy private key")
	}

	// 移除 0x 前缀
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if s == "" {
		return nil, fmt.Errorf("empty deploy private key after trimming")
	}

	// 如果是奇数长度，前面补 0
	if len(s)%2 == 1 {
		s = "0" + s
	}

	keyBytes, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("failed to decode hex private key: %w", err)
	}

	privKey, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to convert to ECDSA private key: %w", err)
	}

	return privKey, nil
}

// deployAgentToShard 向指定分片发送代理合约部署交易
func deployAgentToShard(
	shardID uint32,
	rpcURL string,
	agentCreationCode []byte,
	masterAddr common.Address,
	masterShardID uint32,
	privKey *ecdsa.PrivateKey,
	consensus *Consensus,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. 连接目标分片 RPC
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Str("rpc", rpcURL).Msg("[JOYUE] failed to dial shard RPC")
		return
	}
	defer client.Close()

	// 2. 获取部署地址和余额
	from := crypto.PubkeyToAddress(privKey.PublicKey)
	balance, err := client.BalanceAt(ctx, from, nil)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Str("from", from.Hex()).Msg("[JOYUE] failed to get deployer balance")
		return
	}

	// 3. 获取 chainID 和 nonce
	chainID, err := client.ChainID(ctx)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Msg("[JOYUE] failed to get chain ID")
		return
	}

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Str("from", from.Hex()).Msg("[JOYUE] failed to get nonce")
		return
	}

	// 4. 获取 gas price
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Msg("[JOYUE] failed to suggest gas price")
		return
	}

	// 5. 检查余额是否足够支付 gas
	gasLimit := uint64(2500000)
	requiredBalance := new(big.Int).Mul(gasPrice, big.NewInt(int64(gasLimit)))
	if balance.Cmp(requiredBalance) < 0 {
		utils.Logger().Error().
			Uint32("shard", shardID).
			Str("from", from.Hex()).
			Str("balance", balance.String()).
			Str("requiredBalance", requiredBalance.String()).
			Msg("[JOYUE] insufficient balance to deploy contract")
		return
	}

	// 6. 构造包含构造函数参数的完整 creation code
	// agentCreationCode 是纯 bytecode（不含构造函数参数）
	// 需要添加构造函数参数：constructor(address master_, uint32 masterShardId_, uint32 agentShardId_)
	agentCreationCodeWithCtor, err := packAgentConstructorArgs(agentCreationCode, masterAddr, masterShardID, shardID)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Msg("[JOYUE] failed to pack agent constructor args")
		return
	}

	// 7. 构造以太坊兼容的交易（合约创建：to = nil）
	// 使用 EthTransaction 因为 eth_sendRawTransaction 期望以太坊兼容格式
	// 对于合约创建，Recipient 必须为 nil
	// 由于 newEthTransaction 是私有的，我们使用反射来创建合约创建交易
	ethTx := createEthContractCreation(nonce, big.NewInt(0), gasLimit, gasPrice, agentCreationCodeWithCtor)

	// 8. 验证交易数据
	txData := ethTx.Data()
	if len(txData) != len(agentCreationCodeWithCtor) || !bytes.Equal(txData, agentCreationCodeWithCtor) {
		utils.Logger().Error().
			Uint32("shard", shardID).
			Int("agentCodeWithCtorLen", len(agentCreationCodeWithCtor)).
			Int("txDataLen", len(txData)).
			Msg("[JOYUE] transaction data mismatch")
		return
	}

	// 9. 签名交易
	signer := types.NewEIP155Signer(chainID)
	signedTx, err := types.SignEthTx(ethTx, signer, privKey)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Msg("[JOYUE] failed to sign transaction")
		return
	}

	txHash := signedTx.Hash()

	// 10. 编码并发送交易
	txBytes, err := rlp.EncodeToBytes(signedTx)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Str("txHash", txHash.Hex()).Msg("[JOYUE] failed to encode transaction")
		return
	}

	rpcClient, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Str("rpc", rpcURL).Msg("[JOYUE] failed to dial RPC client")
		return
	}
	defer rpcClient.Close()

	var rpcTxHash common.Hash
	err = rpcClient.CallContext(ctx, &rpcTxHash, "eth_sendRawTransaction", hexutil.Encode(txBytes))
	if err != nil {
		utils.Logger().Error().Err(err).Uint32("shard", shardID).Str("txHash", txHash.Hex()).Str("master", masterAddr.Hex()).Msg("[JOYUE] failed to send agent deployment tx")
		return
	}

	if rpcTxHash != txHash {
		utils.Logger().Warn().
			Uint32("shard", shardID).
			Str("expectedTxHash", txHash.Hex()).
			Str("rpcTxHash", rpcTxHash.Hex()).
			Msg("[JOYUE] RPC returned different tx hash")
	}

	utils.Logger().Info().
		Uint32("shard", shardID).
		Str("txHash", txHash.Hex()).
		Str("master", masterAddr.Hex()).
		Str("from", from.Hex()).
		Msg("[JOYUE] agent deployment tx sent successfully")
}

// createEthContractCreation 创建以太坊兼容的合约创建交易
// 由于 newEthTransaction 是私有的，我们使用反射来设置 Recipient 为 nil
func createEthContractCreation(nonce uint64, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) *types.EthTransaction {
	// 创建一个临时的交易（使用零地址）
	ethTx := types.NewEthTransaction(nonce, common.Address{}, amount, gasLimit, gasPrice, data)

	// 使用反射访问私有字段 data 并设置 Recipient 为 nil
	rv := reflect.ValueOf(ethTx).Elem()
	dataField := rv.FieldByName("data")
	if dataField.IsValid() {
		// 使用 unsafe 包访问私有字段
		dataPtr := unsafe.Pointer(dataField.UnsafeAddr())
		// 将 dataPtr 转换为 *ethTxdata 类型（通过反射获取类型）
		dataType := dataField.Type()
		// 创建一个指向 data 字段的指针
		dataValue := reflect.NewAt(dataType, dataPtr).Elem()
		// 获取 Recipient 字段
		recipientField := dataValue.FieldByName("Recipient")
		if recipientField.IsValid() && recipientField.CanSet() {
			// 设置 Recipient 为 nil（合约创建）
			recipientField.Set(reflect.ValueOf((*common.Address)(nil)))
		}
	}

	return ethTx
}

// packAgentConstructorArgs 将构造函数参数编码并拼接到 agent bytecode 后面
// 构造函数签名：constructor(address master_, uint32 masterShardId_, uint32 agentShardId_)
// 返回：agentBytecode || ABI编码的构造函数参数
func packAgentConstructorArgs(
	agentBytecode []byte,
	masterAddr common.Address,
	masterShardID uint32,
	agentShardID uint32,
) ([]byte, error) {
	// 定义构造函数参数类型
	tAddress, err := abi.NewType("address", "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create address type: %w", err)
	}
	tUint32, err := abi.NewType("uint32", "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create uint32 type: %w", err)
	}

	// 构造 ABI 参数列表
	args := abi.Arguments{
		{Type: tAddress},
		{Type: tUint32},
		{Type: tUint32},
	}

	// 编码构造函数参数
	ctorArgs, err := args.Pack(masterAddr, masterShardID, agentShardID)
	if err != nil {
		return nil, fmt.Errorf("failed to pack constructor args: %w", err)
	}

	// 拼接：bytecode || 构造函数参数
	creationCode := append(common.CopyBytes(agentBytecode), ctorArgs...)
	return creationCode, nil
}

// 通过事件识别 JoyueMaster 合约
func isJoyueMaster(logs []*types.Log) bool {
	// MasterDeployed 事件的 topic0（完整的 hash，32 字节）
	masterDeployedSig := common.BytesToHash(crypto.Keccak256([]byte("MasterDeployed(address,bytes32,bytes,uint32)")))
	for _, log := range logs {
		if len(log.Topics) > 0 && log.Topics[0] == masterDeployedSig {
			return true
		}
	}
	return false
}

// BroadcastNewBlock is called by consensus leader to sync new blocks with other clients/nodes.
// NOTE: For now, just send to the client (basically not broadcasting)
// TODO (lc): broadcast the new blocks to new nodes doing state sync
func BroadcastNewBlock(host p2p.Host, newBlock *types.Block, nodeConfig *nodeconfig.ConfigType) {
	groups := []nodeconfig.GroupID{nodeConfig.GetClientGroupID()}
	utils.Logger().Info().
		Msgf(
			"broadcasting new block %d, group %s", newBlock.NumberU64(), groups[0],
		)
	msg := p2p.ConstructMessage(
		proto_node.ConstructBlocksSyncMessage([]*types.Block{newBlock}),
	)
	if err := host.SendMessageToGroups(groups, msg); err != nil {
		utils.Logger().Warn().Err(err).Msg("cannot broadcast new block")
	}
}

func IsRunningBeaconChain(c *Consensus) bool {
	return c.ShardID == shard.BeaconChainShardID
}

// BroadcastCXReceipts broadcasts cross shard receipts to correspoding
// destination shards
func BroadcastCXReceipts(newBlock *types.Block, consensus *Consensus) {
	commitSigAndBitmap := newBlock.GetCurrentCommitSig()
	//#### Read payload data from committed msg
	if len(commitSigAndBitmap) <= 96 {
		utils.Logger().Debug().Int("commitSigAndBitmapLen", len(commitSigAndBitmap)).Msg("[BroadcastCXReceipts] commitSigAndBitmap Not Enough Length")
		return
	}
	commitSig := make([]byte, 96)
	commitBitmap := make([]byte, len(commitSigAndBitmap)-96)
	offset := 0
	copy(commitSig[:], commitSigAndBitmap[offset:offset+96])
	offset += 96
	copy(commitBitmap[:], commitSigAndBitmap[offset:])
	//#### END Read payload data from committed msg

	epoch := newBlock.Header().Epoch()
	shardingConfig := shard.Schedule.InstanceForEpoch(epoch)
	shardNum := int(shardingConfig.NumShards())
	myShardID := consensus.ShardID
	utils.Logger().Info().Int("shardNum", shardNum).Uint32("myShardID", myShardID).Uint64("blockNum", newBlock.NumberU64()).Msg("[BroadcastCXReceipts]")

	for i := 0; i < shardNum; i++ {
		if i == int(myShardID) {
			continue
		}
		BroadcastCXReceiptsWithShardID(newBlock.Header(), commitSig, commitBitmap, uint32(i), consensus)
	}
}

// BroadcastCXReceiptsWithShardID broadcasts cross shard receipts to given ToShardID
func BroadcastCXReceiptsWithShardID(block *block.Header, commitSig []byte, commitBitmap []byte, toShardID uint32, consensus *Consensus) {
	myShardID := consensus.ShardID
	utils.Logger().Debug().
		Uint32("toShardID", toShardID).
		Uint32("myShardID", myShardID).
		Uint64("blockNum", block.NumberU64()).
		Msg("[BroadcastCXReceiptsWithShardID]")

	cxReceipts, err := consensus.Blockchain().ReadCXReceipts(toShardID, block.NumberU64(), block.Hash())
	if err != nil || len(cxReceipts) == 0 {
		utils.Logger().Debug().Uint32("ToShardID", toShardID).
			Int("numCXReceipts", len(cxReceipts)).
			Msg("[CXMerkleProof] No receipts found for the destination shard")
		return
	}

	merkleProof, err := consensus.Blockchain().CXMerkleProof(toShardID, block)
	if err != nil {
		utils.Logger().Warn().
			Uint32("ToShardID", toShardID).
			Msg("[BroadcastCXReceiptsWithShardID] Unable to get merkleProof")
		return
	}

	cxReceiptsProof := &types.CXReceiptsProof{
		Receipts:     cxReceipts,
		MerkleProof:  merkleProof,
		Header:       block,
		CommitSig:    commitSig,
		CommitBitmap: commitBitmap,
	}

	groupID := nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(toShardID))
	utils.Logger().Info().Uint32("ToShardID", toShardID).
		Str("GroupID", string(groupID)).
		Interface("cxp", cxReceiptsProof).
		Msg("[BroadcastCXReceiptsWithShardID] ReadCXReceipts and MerkleProof ready. Sending CX receipts...")
	// TODO ek – limit concurrency
	consensus.GetHost().SendMessageToGroups([]nodeconfig.GroupID{groupID},
		p2p.ConstructMessage(proto_node.ConstructCXReceiptsProof(cxReceiptsProof)),
	)
}

// BroadcastMissingCXReceipts broadcasts missing cross shard receipts per request
func BroadcastMissingCXReceipts(c *Consensus) {
	var (
		sendNextTime = make([]core.CxEntry, 0)
		cxPool       = c.Registry().GetCxPool()
		blockchain   = c.Blockchain()
	)
	it := cxPool.Pool().Iterator()
	for entry := range it.C {
		cxEntry := entry.(core.CxEntry)
		toShardID := cxEntry.ToShardID
		blk := blockchain.GetBlockByHash(cxEntry.BlockHash)
		if blk == nil {
			continue
		}
		blockNum := blk.NumberU64()
		nextHeader := blockchain.GetHeaderByNumber(blockNum + 1)
		if nextHeader == nil {
			sendNextTime = append(sendNextTime, cxEntry)
			continue
		}
		sig := nextHeader.LastCommitSignature()
		bitmap := nextHeader.LastCommitBitmap()
		BroadcastCXReceiptsWithShardID(blk.Header(), sig[:], bitmap, toShardID, c)
	}
	cxPool.Clear()
	// this should not happen or maybe happen for impatient user
	for _, entry := range sendNextTime {
		cxPool.Add(entry)
	}
}
