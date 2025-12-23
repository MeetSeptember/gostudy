package types

import (
	"io"

	"github.com/ethereum/go-ethereum/rlp"

	"github.com/harmony-one/harmony/block"
	staking "github.com/harmony-one/harmony/staking/types"
)

// BodyV2 is the V2 block body
type BodyV2 struct {
	f bodyFieldsV2
}

type bodyFieldsV2 struct {
	Transactions        BlockTransactions
	StakingTransactions []*staking.StakingTransaction
	Uncles              []*block.Header
	IncomingReceipts    CXReceiptsProofs
	IncomingDeploys     CXDeployProofs
	OutgoingDeploys     CXDeploys
}

// Transactions returns the list of transactions.
//
// The returned list is a deep copy; the caller may do anything with it without
// affecting the original.
func (b *BodyV2) Transactions() BlockTransactions {
	return b.f.Transactions.Copy()
}

// StakingTransactions returns the list of staking transactions.
// The returned list is a deep copy; the caller may do anything with it without
// affecting the original.
func (b *BodyV2) StakingTransactions() (txs []*staking.StakingTransaction) {
	for _, tx := range b.f.StakingTransactions {
		txs = append(txs, tx.Copy())
	}
	return txs
}

// TransactionAt returns the transaction at the given index in this block.
// It returns nil if index is out of bounds.
func (b *BodyV2) TransactionAt(index int) BlockTransaction {
	if index < 0 || index >= len(b.f.Transactions) {
		return nil
	}
	// Return a deep copy via Copy() of the container.
	return b.f.Transactions.Copy()[index]
}

// StakingTransactionAt returns the staking transaction at the given index in this block.
// It returns nil if index is out of bounds.
func (b *BodyV2) StakingTransactionAt(index int) *staking.StakingTransaction {
	if index < 0 || index >= len(b.f.StakingTransactions) {
		return nil
	}
	return b.f.StakingTransactions[index].Copy()
}

// CXReceiptAt returns the CXReceipt at given index in this block
// It returns nil if index is out of bounds
func (b *BodyV2) CXReceiptAt(index int) *CXReceipt {
	if index < 0 {
		return nil
	}
	for _, cxp := range b.f.IncomingReceipts {
		cxs := cxp.Receipts
		if index < len(cxs) {
			return cxs[index].Copy()
		}
		index -= len(cxs)
	}
	return nil
}

// SetTransactions sets the list of transactions with a deep copy of the given
// list.
func (b *BodyV2) SetTransactions(newTransactions BlockTransactions) {
	b.f.Transactions = newTransactions.Copy()
}

// SetStakingTransactions sets the list of staking transactions with a deep copy of the given
// list.
func (b *BodyV2) SetStakingTransactions(newStakingTransactions []*staking.StakingTransaction) {
	var txs []*staking.StakingTransaction
	for _, tx := range newStakingTransactions {
		txs = append(txs, tx.Copy())
	}
	b.f.StakingTransactions = txs
}

// Uncles returns a deep copy of the list of uncle headers of this block.
func (b *BodyV2) Uncles() (uncles []*block.Header) {
	for _, uncle := range b.f.Uncles {
		uncles = append(uncles, CopyHeader(uncle))
	}
	return uncles
}

// SetUncles sets the list of uncle headers with a deep copy of the given list.
func (b *BodyV2) SetUncles(newUncle []*block.Header) {
	var uncles []*block.Header
	for _, uncle := range newUncle {
		uncles = append(uncles, CopyHeader(uncle))
	}
	b.f.Uncles = uncles
}

// IncomingReceipts returns a deep copy of the list of incoming cross-shard
// transaction receipts of this block.
func (b *BodyV2) IncomingReceipts() (incomingReceipts CXReceiptsProofs) {
	return b.f.IncomingReceipts.Copy()
}

// IncomingDeploys returns incoming deploy proofs.
func (b *BodyV2) IncomingDeploys() (incoming CXDeployProofs) {
	return b.f.IncomingDeploys.Copy()
}

// SetIncomingReceipts sets the list of incoming cross-shard transaction
// receipts of this block with a dep copy of the given list.
func (b *BodyV2) SetIncomingReceipts(newIncomingReceipts CXReceiptsProofs) {
	b.f.IncomingReceipts = newIncomingReceipts.Copy()
}

// SetIncomingDeploys sets incoming deploy proofs.
func (b *BodyV2) SetIncomingDeploys(newIncomingDeploys CXDeployProofs) {
	b.f.IncomingDeploys = newIncomingDeploys.Copy()
}

// OutgoingDeploys returns outgoing deploy intents.
func (b *BodyV2) OutgoingDeploys() CXDeploys {
	return b.f.OutgoingDeploys.Copy()
}

// SetOutgoingDeploys sets outgoing deploy intents.
func (b *BodyV2) SetOutgoingDeploys(newOutgoingDeploys CXDeploys) {
	b.f.OutgoingDeploys = newOutgoingDeploys.Copy()
}

// EncodeRLP RLP-encodes the block body into the given writer.
func (b *BodyV2) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &b.f)
}

// DecodeRLP RLP-decodes a block body from the given RLP stream into the
// receiver.
func (b *BodyV2) DecodeRLP(s *rlp.Stream) error {
	return s.Decode(&b.f)
}
