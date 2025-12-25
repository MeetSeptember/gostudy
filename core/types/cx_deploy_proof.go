package types

import (
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/block"
)

// CXDeployProof is analogous to CXReceiptsProof, but for deploy intents.
// It packages deploy intents and the source block header for verification.
type CXDeployProof struct {
	Deploys      CXDeploys
	Header       *block.Header
	CommitSig    []byte
	CommitBitmap []byte
	// TODO: add MerkleProof if needed
}

// Copy returns a deep copy of the proof.
func (p *CXDeployProof) Copy() *CXDeployProof {
	if p == nil {
		return nil
	}
	cpy := *p
	cpy.Deploys = p.Deploys.Copy()
	cpy.CommitSig = append(cpy.CommitSig[:0:0], p.CommitSig...)
	cpy.CommitBitmap = append(cpy.CommitBitmap[:0:0], p.CommitBitmap...)
	cpy.Header = CopyHeader(p.Header)
	return &cpy
}

// CXDeployProofs is a list of CXDeployProof.
type CXDeployProofs []*CXDeployProof

// Len returns the length of the slice.
func (ps CXDeployProofs) Len() int { return len(ps) }

// GetRlp implements Rlpable and returns the i'th element encoded.
func (ps CXDeployProofs) GetRlp(i int) []byte {
	if len(ps) == 0 {
		return []byte{}
	}
	enc, _ := rlp.EncodeToBytes(ps[i])
	return enc
}

// Copy makes a deep copy.
func (ps CXDeployProofs) Copy() (cpy CXDeployProofs) {
	for _, p := range ps {
		cpy = append(cpy, p.Copy())
	}
	return cpy
}
