package types

import (
	"io"

	"github.com/ethereum/go-ethereum/rlp"
)

// BlockTransaction is a transaction type that can be included in a block.
//
// It must be both:
// - PoolTransaction: for txpool validation / sizing / encoding
// - InternalTransaction: for signing / sender derivation / message conversion
type BlockTransaction interface {
	PoolTransaction
	InternalTransaction
}

// BlockTransactions is a list of block transactions.
//
// RLP encoding (canonical, EIP-2718 style):
// - legacy tx: rlp(tx)
// - typed tx : typeByte || rlp(payload)
//
// In block bodies, these are stored as an RLP list of byte strings, each being the
// canonical bytes above. This allows mixed tx types while keeping the tx trie root
// consistent with canonical bytes.
type BlockTransactions []BlockTransaction

func (s BlockTransactions) Len() int { return len(s) }

// GetRlp returns the canonical bytes used for trie/root calculation.
func (s BlockTransactions) GetRlp(i int) []byte {
	if i < 0 || i >= len(s) || s[i] == nil {
		return []byte{}
	}
	switch tx := s[i].(type) {
	case *JoyueDeployTx:
		b, err := tx.CanonicalBytes()
		if err != nil {
			return []byte{}
		}
		return b
	default:
		enc, err := rlp.EncodeToBytes(s[i])
		if err != nil {
			return []byte{}
		}
		return enc
	}
}

// EncodeRLP encodes BlockTransactions as an RLP list of canonical transaction byte strings.
func (s BlockTransactions) EncodeRLP(w io.Writer) error {
	raws := make([][]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		raws = append(raws, s.GetRlp(i))
	}
	return rlp.Encode(w, raws)
}

// DecodeRLP decodes BlockTransactions from an RLP list of canonical transaction byte strings.
func (s *BlockTransactions) DecodeRLP(stream *rlp.Stream) error {
	var raws [][]byte
	if err := stream.Decode(&raws); err != nil {
		return err
	}
	out := make(BlockTransactions, 0, len(raws))
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		if raw[0] == JoyueDeployTxType {
			tx, err := DecodeJoyueDeployTxBytes(raw)
			if err != nil {
				return err
			}
			out = append(out, tx)
			continue
		}
		// legacy hmy tx
		ltx := new(Transaction)
		if err := rlp.DecodeBytes(raw, ltx); err != nil {
			return err
		}
		out = append(out, ltx)
	}
	*s = out
	return nil
}

// Copy returns a deep copy of the receiver.
func (s BlockTransactions) Copy() (cpy BlockTransactions) {
	for _, tx := range s {
		if tx == nil {
			continue
		}
		switch t := tx.(type) {
		case *Transaction:
			cpy = append(cpy, t.Copy())
		case *JoyueDeployTx:
			// Re-decode from canonical bytes to ensure deep copy.
			if b, err := t.CanonicalBytes(); err == nil {
				if dec, err := DecodeJoyueDeployTxBytes(b); err == nil {
					cpy = append(cpy, dec)
				}
			}
		default:
			// fallback: best-effort via RLP roundtrip
			if b, err := rlp.EncodeToBytes(t); err == nil {
				var tmp JoyueDeployTx
				_ = rlp.DecodeBytes(b, &tmp)
				// can't safely append unknown type; skip
			}
		}
	}
	return cpy
}

// BlockTransactionsFromLegacy converts legacy hmy transactions to block transactions.
func BlockTransactionsFromLegacy(txs []*Transaction) BlockTransactions {
	out := make(BlockTransactions, 0, len(txs))
	for i := range txs {
		out = append(out, txs[i])
	}
	return out
}
