package types

import (
	"io"

	"github.com/ethereum/go-ethereum/rlp"
)

// CXDeployBody mirrors CXReceiptsProofs encoding/decoding for deploy proofs.
type CXDeployBody struct {
	DeployProofs CXDeployProofs
}

// EncodeRLP implements rlp.Encoder
func (b *CXDeployBody) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &b.DeployProofs)
}

// DecodeRLP implements rlp.Decoder
func (b *CXDeployBody) DecodeRLP(s *rlp.Stream) error {
	var proofs CXDeployProofs
	if err := s.Decode(&proofs); err != nil {
		return err
	}
	b.DeployProofs = proofs
	return nil
}
