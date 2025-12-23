package consensus

import (
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/rawdb"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/shard"
	staking "github.com/harmony-one/harmony/staking/types"
	"github.com/pkg/errors"
)

// Constants of proposing a new block
const (
	IncomingReceiptsLimit = 6000 // 2000 * (numShards - 1)
	SleepPeriod           = 20 * time.Millisecond
)

// ProposeNewBlock proposes a new block...
func (consensus *Consensus) ProposeNewBlock(commitSigs chan []byte) (*types.Block, error) {
	var (
		// 获取当前链头的引用，方便后面拿数据
		currentHeader = consensus.Blockchain().CurrentHeader()
		// 获取当前处于第几个纪元 (Epoch)
		nowEpoch = currentHeader.Epoch()
		// 获取当前区块高度 (Block Number)
		blockNow = currentHeader.Number()
		// 获取 Worker (打包工)，这是真正干脏活累活的对象
		worker = consensus.registry.GetWorker()
	)
	utils.AnalysisStart("ProposeNewBlock", nowEpoch, blockNow)
	defer utils.AnalysisEnd("ProposeNewBlock", nowEpoch, blockNow)

	// Update worker's current header and
	// state data in preparation to propose/process new transactions
	// Worker 需要把它的“当前状态”同步到区块链的最新区块，准备好在一个新的基础上打包。
	// 在 Harmony（以及以太坊 Geth）的架构中，Node（节点） 是整个应用程序，而 Worker（工人） 是节点内部专门负责 “组装区块” 和 “执行交易” 的那个核心组件。
	// work在临时环境中试进行交易，复制状态，沙盒环境，模拟执行
	// Worker 需要把它的“当前状态”同步到区块链的最新区块，准备好在一个新的基础上打包。
	env, err := worker.UpdateCurrent()
	if err != nil {
		return nil, errors.Wrap(err, "failed to update worker")
	}

	// 拿到更新后的 Header 和分片状态 (ShardState)
	header := env.CurrentHeader()
	shardState, err := consensus.Blockchain().ReadShardState(header.Epoch())
	if err != nil {
		return nil, errors.WithMessage(err, "failed to read shard")
	}

	// 逻辑决定了出块奖励发给谁
	var (
		// 获取当前 Leader (也就是自己) 的公钥
		leaderKey = consensus.GetLeaderPubKey()

		// 计算初始 Coinbase 地址
		coinbase    = consensus.registry.GetAddressToBLSKey().GetAddressForBLSKey(consensus.GetPublicKeys(), shardState, leaderKey.Object, header.Epoch())
		beneficiary = coinbase
	)

	// After staking, all coinbase will be the address of bls pub key

	// 如果当前处于 Staking 纪元 (Config().IsStaking)，逻辑变了：
	if consensus.Blockchain().Config().IsStaking(header.Epoch()) {
		blsPubKeyBytes := leaderKey.Object.GetAddress()
		coinbase.SetBytes(blsPubKeyBytes[:])
	}

	if coinbase == (common.Address{}) {
		return nil, errors.New("[ProposeNewBlock] Failed setting coinbase")
	}

	// Must set coinbase here because the operations below depend on it
	header.SetCoinbase(coinbase)

	// Get beneficiary based on coinbase
	// Before staking, coinbase itself is the beneficial
	// After staking, beneficial is the corresponding ECDSA address of the bls key
	beneficiary, err = consensus.Blockchain().GetECDSAFromCoinbase(header)
	if err != nil {
		return nil, err
	}

	// Harmony 的分片安全性依赖于分布式随机数。
	// Add VRF
	if consensus.Blockchain().Config().IsVRF(header.Epoch()) {
		//generate a new VRF for the current block
		// 生成 VRF 和 Proof
		// 这一步会用 Leader 的私钥对“上一块的随机数+ViewID”进行签名，生成一个新的随机数。
		// 结果会被写入 header 对象中。
		if err := consensus.GenerateVrfAndProof(header); err != nil {
			return nil, err
		}
	}

	// 打包交易
	// Execute all the time except for last block of epoch for shard 0
	// 过滤逻辑：什么时候才打包交易？
	// 条件：(不是本 Epoch 的最后一个区块) 或者 (当前不是 Shard 0)
	// 含义：Shard 0 的最后一个区块通常用来做跨 Epoch 的交接 (选举)，为了求稳，不打包普通交易。
	if !shard.Schedule.IsLastBlock(header.Number().Uint64()) || consensus.ShardID != 0 {
		// Prepare normal and staking transactions retrieved from transaction pool
		utils.AnalysisStart("proposeNewBlockChooseFromTxnPool")

		// 从交易池获取所有待打包 (Pending) 交易
		pendingPoolTxs, err := consensus.registry.GetTxPool().Pending()
		if err != nil {
			consensus.GetLogger().Err(err).Msg("Failed to fetch pending transactions")
			return nil, err
		}

		// 扁平收集普通交易（兼容新 typed tx），质押交易单独收集
		pendingBlockTxs := types.BlockTransactions{}
		pendingStakingTxs := staking.StakingTransactions{}

		for addr, poolTxs := range pendingPoolTxs {
			for _, tx := range poolTxs {
				if plainTx, ok := tx.(*types.Transaction); ok {
					pendingBlockTxs = append(pendingBlockTxs, plainTx)
				} else if deployTx, ok := tx.(*types.JoyueDeployTx); ok {
					pendingBlockTxs = append(pendingBlockTxs, deployTx)
				} else if stakingTx, ok := tx.(*staking.StakingTransaction); ok {
					// Only process staking transactions after pre-staking epoch happened.
					if consensus.Blockchain().Config().IsPreStaking(worker.GetCurrentHeader().Epoch()) {
						pendingStakingTxs = append(pendingStakingTxs, stakingTx)
					}
				} else {
					consensus.GetLogger().Err(types.ErrUnknownPoolTxType).
						Msg("Failed to parse pending transactions")
					return nil, types.ErrUnknownPoolTxType
				}
			}
		}

		// Try commit normal and staking transactions based on the current state
		// The successfully committed transactions will be put in the proposed block
		// 核心：提交交易 (Commit)
		// worker.CommitTransactions 会模拟执行这些交易。
		// 如果交易 Gas 不够、余额不足，Worker 会自动剔除它们。
		// 执行成功的交易会被添加到 Worker 内部的 "receipts" 和 "txs" 列表中，准备最终打包。
		if err := worker.CommitBlockTransactions(
			pendingBlockTxs, pendingStakingTxs, beneficiary,
		); err != nil {
			consensus.GetLogger().Error().Err(err).Msg("cannot commit transactions")
			return nil, err
		}
		utils.AnalysisEnd("proposeNewBlockChooseFromTxnPool")
	}

	// Prepare incoming cross shard transaction receipts
	// These are accepted even during the epoch before hip-30
	// because the destination shard only receives them after
	// balance is deducted on source shard. to prevent this from
	// being a significant problem, the source shards will stop
	// accepting txs destined to the shards which are shutting down
	// one epoch prior the shut down

	// 这是一个复杂的内部函数，去查找还没有被处理的 Incoming Receipts。
	// 这里可能是其他分片发送给当前分片的跨分片交易
	receiptsList := consensus.proposeReceiptsProof()

	// 如果有收据，就提交给 Worker
	// 这相当于给本分片的用户加余额。跨分片转账交易
	if len(receiptsList) != 0 {
		if err := worker.CommitReceipts(receiptsList); err != nil {
			return nil, err
		}
	}

	// incoming deploy proofs
	deploysList := consensus.proposeDeployProof()
	if len(deploysList) != 0 {
		if err := worker.CommitDeploys(deploysList); err != nil {
			return nil, err
		}
	}

	// 判断条件：我是 Shard 0 吗？当前开启 CrossLink 功能了吗？
	isBeaconchainInCrossLinkEra := consensus.ShardID == shard.BeaconChainShardID &&
		consensus.Blockchain().Config().IsCrossLink(worker.GetCurrentHeader().Epoch())

	// 类似的，判断是否处于 Staking 时代
	isBeaconchainInStakingEra := consensus.ShardID == shard.BeaconChainShardID &&
		consensus.Blockchain().Config().IsStaking(worker.GetCurrentHeader().Epoch())

	utils.AnalysisStart("proposeNewBlockVerifyCrossLinks")
	// Prepare cross links and slashing messages
	// 如果我是 Shard 0，我要处理 CrossLink (其他分片汇报上来的状态)
	var crossLinksToPropose types.CrossLinks
	if isBeaconchainInCrossLinkEra {
		allPending, err := consensus.Blockchain().ReadPendingCrossLinks()
		invalidToDelete := []types.CrossLink{}
		if err == nil {
			for _, pending := range allPending {
				if !consensus.Blockchain().Config().IsCrossLink(pending.Epoch()) {
					invalidToDelete = append(invalidToDelete, pending)
					consensus.GetLogger().Debug().
						AnErr("[ProposeNewBlock] pending crosslink that's before crosslink epoch", err)
					continue
				}
				// ReadCrossLink beacon chain usage.
				exist, err := consensus.Blockchain().ReadCrossLink(pending.ShardID(), pending.BlockNum())
				if err == nil || exist != nil {
					invalidToDelete = append(invalidToDelete, pending)
					consensus.GetLogger().Debug().
						AnErr("[ProposeNewBlock] pending crosslink is already committed onchain", err)
					continue
				}
				last, err := consensus.Blockchain().ReadShardLastCrossLink(pending.ShardID())
				if err != nil {
					consensus.GetLogger().Debug().
						AnErr("[ProposeNewBlock] failed to read last crosslink", err)
					// no return
				}
				// if pending crosslink is older than the last crosslink, delete it and continue
				if err == nil && exist == nil && last != nil && last.BlockNum() >= pending.BlockNum() {
					// Crosslink is already verified before it's accepted to pending,
					// no need to verify again in proposal.
					invalidToDelete = append(invalidToDelete, pending)
					consensus.GetLogger().Debug().
						AnErr("[ProposeNewBlock] pending crosslink is older than last shard crosslink", err)
					continue
				}

				crossLinksToPropose = append(crossLinksToPropose, pending)
				if len(crossLinksToPropose) > 15 {
					break
				}
			}
			consensus.GetLogger().Info().
				Msgf("[ProposeNewBlock] Proposed %d crosslinks from %d pending crosslinks",
					len(crossLinksToPropose), len(allPending),
				)
		} else {
			consensus.GetLogger().Warn().Err(err).Msgf(
				"[ProposeNewBlock] Unable to Read PendingCrossLinks, number of crosslinks: %d",
				len(allPending),
			)
		}
		if n, err := consensus.Blockchain().DeleteFromPendingCrossLinks(invalidToDelete); err != nil {
			consensus.GetLogger().Error().
				Err(err).
				Msg("[ProposeNewBlock] invalid pending cross links failed")
		} else if len(invalidToDelete) > 0 {
			consensus.GetLogger().Info().
				Int("not-deleted", n).
				Int("deleted", len(invalidToDelete)).
				Msg("[ProposeNewBlock] deleted invalid pending cross links")
		}
	}
	utils.AnalysisEnd("proposeNewBlockVerifyCrossLinks")

	if isBeaconchainInStakingEra {
		// this will set a meaningful w.current.slashes
		if err := worker.CollectVerifiedSlashes(); err != nil {
			return nil, err
		}
	}

	worker.ApplyShardReduction()

	// Prepare shard state
	// 计算下一届委员会 (SuperCommittee)
	// 这是为了填充 header 里的 ShardState 字段，让大家知道下个 Epoch 谁说了算。
	if shardState, err = consensus.Blockchain().SuperCommitteeForNextEpoch(
		consensus.Beaconchain(), worker.GetCurrentHeader(), false,
	); err != nil {
		return nil, err
	}

	viewIDFunc := func() uint64 {
		return consensus.GetCurBlockViewID()
	}

	// 核心组装：FinalizeNewBlock
	// 这个函数会：
	// - 计算所有交易的 Merkle Root
	// - 计算所有 Receipt 的 Merkle Root
	// - 填充 Header 的所有字段 (StateRoot, TxRoot, etc.)
	// - 返回一个完整的 Block 对象
	finalizedBlock, err := worker.FinalizeNewBlock(
		commitSigs, viewIDFunc,
		coinbase, crossLinksToPropose, shardState,
	)
	if err != nil {
		consensus.GetLogger().Error().Err(err).Msg("[ProposeNewBlock] Failed finalizing the new block")
		return nil, err
	}
	consensus.GetLogger().Info().Msg("[ProposeNewBlock] verifying the new block header")

	// 自我质检：ValidateHeader
	// 在广播出去之前，先自己验证一下生成的 Block Header 是否合法。
	// 比如：Hash 算得对不对？时间戳是否合理？
	err = core.NewBlockValidator(consensus.Blockchain()).ValidateHeader(finalizedBlock, true)

	if err != nil {
		consensus.GetLogger().Error().Err(err).Msg("[ProposeNewBlock] Failed verifying the new block header")
		return nil, err
	}

	// Save process result in the cache for later use for faster block commitment to db.
	// 缓存结果
	// Worker 把刚刚辛苦算出来的结果 (Result) 存进缓存。
	// 这样当共识达成、需要真正把区块写入数据库时，就不用重新计算了，直接从缓存拿，速度快。
	// 注意 这里包含了之前的合约执行结果
	result := worker.GetCurrentResult()
	consensus.Blockchain().Processor().CacheProcessorResult(finalizedBlock.Hash(), result)
	return finalizedBlock, nil
}

func (consensus *Consensus) proposeReceiptsProof() []*types.CXReceiptsProof {
	if !consensus.Blockchain().Config().HasCrossTxFields(consensus.registry.GetWorker().GetCurrentHeader().Epoch()) {
		return []*types.CXReceiptsProof{}
	}

	numProposed := 0
	validReceiptsList := []*types.CXReceiptsProof{}
	pendingReceiptsList := []*types.CXReceiptsProof{}

	// not necessary to sort the list, but we just prefer to process the list ordered by shard and blocknum
	pendingCXReceipts := []*types.CXReceiptsProof{}
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	for _, v := range consensus.pendingCXReceipts {
		pendingCXReceipts = append(pendingCXReceipts, v)
	}

	sort.SliceStable(pendingCXReceipts, func(i, j int) bool {
		shardCMP := pendingCXReceipts[i].MerkleProof.ShardID < pendingCXReceipts[j].MerkleProof.ShardID
		shardEQ := pendingCXReceipts[i].MerkleProof.ShardID == pendingCXReceipts[j].MerkleProof.ShardID
		blockCMP := pendingCXReceipts[i].MerkleProof.BlockNum.Cmp(
			pendingCXReceipts[j].MerkleProof.BlockNum,
		) == -1
		return shardCMP || (shardEQ && blockCMP)
	})

	m := map[common.Hash]struct{}{}

Loop:
	for _, cxp := range consensus.pendingCXReceipts {
		if numProposed > IncomingReceiptsLimit {
			pendingReceiptsList = append(pendingReceiptsList, cxp)
			continue
		}
		// check double spent
		if consensus.Blockchain().IsSpent(cxp) {
			consensus.getLogger().Debug().Interface("cxp", cxp).Msg("[proposeReceiptsProof] CXReceipt is spent")
			continue
		}
		hash := cxp.MerkleProof.BlockHash
		// ignore duplicated receipts
		if _, ok := m[hash]; ok {
			continue
		} else {
			m[hash] = struct{}{}
		}

		for _, item := range cxp.Receipts {
			if item.ToShardID != consensus.Blockchain().ShardID() {
				continue Loop
			}
		}

		if err := core.NewBlockValidator(consensus.Blockchain()).ValidateCXReceiptsProof(cxp); err != nil {
			if strings.Contains(err.Error(), rawdb.MsgNoShardStateFromDB) {
				pendingReceiptsList = append(pendingReceiptsList, cxp)
			} else {
				consensus.getLogger().Error().Err(err).Msg("[proposeReceiptsProof] Invalid CXReceiptsProof")
			}
			continue
		}

		consensus.getLogger().Debug().Interface("cxp", cxp).Msg("[proposeReceiptsProof] CXReceipts Added")
		validReceiptsList = append(validReceiptsList, cxp)
		numProposed = numProposed + len(cxp.Receipts)
	}

	consensus.pendingCXReceipts = make(map[utils.CXKey]*types.CXReceiptsProof)
	for _, v := range pendingReceiptsList {
		blockNum := v.Header.Number().Uint64()
		shardID := v.Header.ShardID()
		key := utils.GetPendingCXKey(shardID, blockNum)
		consensus.pendingCXReceipts[key] = v
	}

	consensus.getLogger().Debug().Msgf("[proposeReceiptsProof] number of validReceipts %d", len(validReceiptsList))
	return validReceiptsList
}

func (consensus *Consensus) proposeDeployProof() []*types.CXDeployProof {
	if !consensus.Blockchain().Config().HasCrossTxFields(consensus.registry.GetWorker().GetCurrentHeader().Epoch()) {
		return []*types.CXDeployProof{}
	}
	valid := []*types.CXDeployProof{}
	pending := []*types.CXDeployProof{}

	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	for _, v := range consensus.pendingCXDeploys {
		pending = append(pending, v)
	}

	m := map[common.Hash]struct{}{}
	for _, cxd := range pending {
		if len(cxd.Deploys) == 0 {
			continue
		}
		// destination must be this shard
		if cxd.Deploys[0].ToShardID != consensus.ShardID {
			continue
		}
		if consensus.Blockchain().IsDeploySpent(cxd) {
			continue
		}
		hash := cxd.Header.Hash()
		if _, ok := m[hash]; ok {
			continue
		}
		m[hash] = struct{}{}
		valid = append(valid, cxd)
	}

	consensus.pendingCXDeploys = make(map[utils.CXKey]*types.CXDeployProof)
	for _, v := range pending {
		blockNum := v.Header.Number().Uint64()
		shardID := v.Header.ShardID()
		key := utils.GetPendingCXKey(shardID, blockNum)
		consensus.pendingCXDeploys[key] = v
	}
	return valid
}

func (consensus *Consensus) AddPendingReceipts(receipts *types.CXReceiptsProof) {
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	if receipts.ContainsEmptyField() {
		consensus.getLogger().Info().
			Int("totalPendingReceipts", len(consensus.pendingCXReceipts)).
			Msg("CXReceiptsProof contains empty field")
		return
	}

	blockNum := receipts.Header.Number().Uint64()
	shardID := receipts.Header.ShardID()

	// Sanity checks

	if err := core.NewBlockValidator(consensus.Blockchain()).ValidateCXReceiptsProof(receipts); err != nil {
		if !strings.Contains(err.Error(), rawdb.MsgNoShardStateFromDB) {
			consensus.getLogger().Error().Err(err).Msg("[AddPendingReceipts] Invalid CXReceiptsProof")
			return
		}
	}

	// cross-shard receipt should not be coming from our shard
	if s := consensus.ShardID; s == shardID {
		consensus.getLogger().Info().
			Uint32("my-shard", s).
			Uint32("receipt-shard", shardID).
			Msg("ShardID of incoming receipt was same as mine")
		return
	}

	if e := receipts.Header.Epoch(); blockNum == 0 ||
		!consensus.Blockchain().Config().AcceptsCrossTx(e) {
		consensus.getLogger().Info().
			Uint64("incoming-epoch", e.Uint64()).
			Msg("Incoming receipt had meaningless epoch")
		return
	}

	key := utils.GetPendingCXKey(shardID, blockNum)

	// DDoS protection
	const maxCrossTxnSize = 4096
	if s := len(consensus.pendingCXReceipts); s >= maxCrossTxnSize {
		consensus.getLogger().Info().
			Int("pending-cx-receipts-size", s).
			Int("pending-cx-receipts-limit", maxCrossTxnSize).
			Msg("Current pending cx-receipts reached size limit")
		return
	}

	if _, ok := consensus.pendingCXReceipts[key]; ok {
		consensus.getLogger().Info().
			Int("totalPendingReceipts", len(consensus.pendingCXReceipts)).
			Msg("Already Got Same Receipt message")
		return
	}
	consensus.pendingCXReceipts[key] = receipts
	consensus.getLogger().Info().
		Int("totalPendingReceipts", len(consensus.pendingCXReceipts)).
		Msg("Got ONE more receipt message")
}

func (consensus *Consensus) AddPendingDeploys(proof *types.CXDeployProof) {
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	if proof == nil || len(proof.Deploys) == 0 {
		return
	}
	// cross-shard deploy should not be coming from our shard
	if s := consensus.ShardID; s == proof.Header.ShardID() {
		return
	}
	// destination should match this shard
	if proof.Deploys[0].ToShardID != consensus.ShardID {
		return
	}
	blockNum := proof.Header.Number().Uint64()
	shardID := proof.Header.ShardID()
	key := utils.GetPendingCXKey(shardID, blockNum)
	consensus.pendingCXDeploys[key] = proof
}

// PendingCXReceipts returns node.pendingCXReceiptsProof
func (consensus *Consensus) PendingCXReceipts() []*types.CXReceiptsProof {
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	cxReceipts := make([]*types.CXReceiptsProof, len(consensus.pendingCXReceipts))
	i := 0
	for _, cxReceipt := range consensus.pendingCXReceipts {
		cxReceipts[i] = cxReceipt
		i++
	}
	return cxReceipts
}
