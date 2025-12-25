package types

import (
	"github.com/ethereum/go-ethereum/common"
)

// CXDeploy represents a cross-shard deploy intent produced on the source shard
// after a JoyueDeployTx successfully deploys the master contract.
type CXDeploy struct {
	MasterAddr    common.Address // deployed master address on source shard
	FromShardID   uint32         // source shard (master shard)
	ToShardID     uint32         // destination shard for agent deployment
	AgentInitCode []byte         // agent init code (constructor-encoded), to be deployed on destination shard
	Salt          [32]byte       // salt used for CREATE2 to derive agent address
}

// Copy returns a deep copy of CXDeploy.
func (cx *CXDeploy) Copy() *CXDeploy {
	if cx == nil {
		return nil
	}
	cpy := *cx
	cpy.AgentInitCode = common.CopyBytes(cx.AgentInitCode)
	return &cpy
}

// CXDeploys is a list of CXDeploy
type CXDeploys []*CXDeploy

// Len returns the length of s.
func (cs CXDeploys) Len() int { return len(cs) }

// Copy makes a deep copy of the receiver.
func (cs CXDeploys) Copy() (cpy CXDeploys) {
	for _, r := range cs {
		cpy = append(cpy, r.Copy())
	}
	return cpy
}
