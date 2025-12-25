package types

import (
	"io"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/crypto/hash"
	"github.com/pkg/errors"
)

// JoyueDeployTxType is the type byte prefix for JoyueDeployTx canonical encoding.
// Canonical encoding: 0x7a || rlp(joyueDeployTxData)
const JoyueDeployTxType byte = 0x7a

// JoyueDeployTx is a typed transaction carrying both master and agent initcode.
//
// Semantics (phase 1):
// - Execute MasterInitCode on source shard (contract creation)
// - After success, generate CXDeploy for other shards using AgentInitCode and Salt
type JoyueDeployTx struct {
	data joyueDeployTxData

	// caches
	hash atomic.Value
	size atomic.Value
	from atomic.Value
	// time at which the node received the tx (local wall time)
	time time.Time
}

// joyueDeployTxData is the RLP payload of JoyueDeployTx (without type byte prefix).
type joyueDeployTxData struct {
	AccountNonce uint64   `json:"nonce"    gencodec:"required"`
	Price        *big.Int `json:"gasPrice" gencodec:"required"`
	GasLimit     uint64   `json:"gas"      gencodec:"required"`
	ShardID      uint32   `json:"shardID"  gencodec:"required"`
	Amount       *big.Int `json:"value"    gencodec:"required"`

	MasterInitCode []byte   `json:"masterInitCode" gencodec:"required"`
	AgentInitCode  []byte   `json:"agentInitCode"  gencodec:"required"`
	Salt           [32]byte `json:"salt"           gencodec:"required"`

	// Signature values
	V *big.Int `json:"v" gencodec:"required"`
	R *big.Int `json:"r" gencodec:"required"`
	S *big.Int `json:"s" gencodec:"required"`

	// This is only used when marshaling to JSON.
	Hash *common.Hash `json:"hash" rlp:"-"`
}

type joyueDeployTxDataMarshaling struct {
	AccountNonce   hexutil.Uint64
	Price          *hexutil.Big
	GasLimit       hexutil.Uint64
	Amount         *hexutil.Big
	MasterInitCode hexutil.Bytes
	AgentInitCode  hexutil.Bytes
	V              *hexutil.Big
	R              *hexutil.Big
	S              *hexutil.Big
}

// NewJoyueDeployTx constructs a new JoyueDeployTx.
// toShard is implicitly all shards except ShardID (phase 1 default).
func NewJoyueDeployTx(
	nonce uint64,
	shardID uint32,
	amount *big.Int,
	gasLimit uint64,
	gasPrice *big.Int,
	masterInitCode []byte,
	agentInitCode []byte,
	salt [32]byte,
) *JoyueDeployTx {
	d := joyueDeployTxData{
		AccountNonce:   nonce,
		ShardID:        shardID,
		Amount:         new(big.Int),
		GasLimit:       gasLimit,
		Price:          new(big.Int),
		MasterInitCode: common.CopyBytes(masterInitCode),
		AgentInitCode:  common.CopyBytes(agentInitCode),
		Salt:           salt,
		V:              new(big.Int),
		R:              new(big.Int),
		S:              new(big.Int),
	}
	if amount != nil {
		d.Amount.Set(amount)
	}
	if gasPrice != nil {
		d.Price.Set(gasPrice)
	}
	return &JoyueDeployTx{data: d, time: time.Now()}
}

// From returns the sender cache slot.
func (tx *JoyueDeployTx) From() *atomic.Value { return &tx.from }

func (tx *JoyueDeployTx) V() *big.Int { return tx.data.V }
func (tx *JoyueDeployTx) R() *big.Int { return tx.data.R }
func (tx *JoyueDeployTx) S() *big.Int { return tx.data.S }

func (tx *JoyueDeployTx) Nonce() uint64    { return tx.data.AccountNonce }
func (tx *JoyueDeployTx) GasLimit() uint64 { return tx.data.GasLimit }
func (tx *JoyueDeployTx) GasPrice() *big.Int {
	return tx.data.Price
}
func (tx *JoyueDeployTx) ShardID() uint32   { return tx.data.ShardID }
func (tx *JoyueDeployTx) ToShardID() uint32 { return tx.data.ShardID }

func (tx *JoyueDeployTx) To() *common.Address { return nil } // contract creation

func (tx *JoyueDeployTx) Value() *big.Int { return tx.data.Amount }

// Data returns the initcode that is executed on the source shard (master initcode).
func (tx *JoyueDeployTx) Data() []byte { return common.CopyBytes(tx.data.MasterInitCode) }

func (tx *JoyueDeployTx) MasterInitCode() []byte { return common.CopyBytes(tx.data.MasterInitCode) }
func (tx *JoyueDeployTx) AgentInitCode() []byte  { return common.CopyBytes(tx.data.AgentInitCode) }
func (tx *JoyueDeployTx) DeploySalt() [32]byte   { return tx.data.Salt }

func (tx *JoyueDeployTx) Time() time.Time { return tx.time }

// Protected returns whether the transaction is replay-protected.
func (tx *JoyueDeployTx) Protected() bool { return isProtectedV(tx.data.V) }

func (tx *JoyueDeployTx) ChainID() *big.Int { return deriveChainID(tx.data.V) }

func (tx *JoyueDeployTx) Size() common.StorageSize {
	if size := tx.size.Load(); size != nil {
		return size.(common.StorageSize)
	}
	// Size is the canonical byte size (type byte + rlp payload)
	c, _ := tx.CanonicalBytes()
	tx.size.Store(common.StorageSize(len(c)))
	return common.StorageSize(len(c))
}

// IsEthCompatible returns whether the tx is ethereum compatible (always false for JoyueDeployTx).
func (tx *JoyueDeployTx) IsEthCompatible() bool { return false }

func (tx *JoyueDeployTx) Cost() (*big.Int, error) {
	total := new(big.Int).Mul(tx.data.Price, new(big.Int).SetUint64(tx.data.GasLimit))
	total.Add(total, tx.data.Amount)
	return total, nil
}

// CanonicalBytes returns the canonical encoding used for hashing and block inclusion.
func (tx *JoyueDeployTx) CanonicalBytes() ([]byte, error) {
	payload, err := rlp.EncodeToBytes(&tx.data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+len(payload))
	out[0] = JoyueDeployTxType
	copy(out[1:], payload)
	return out, nil
}

// Hash uniquely identifies the transaction (keccak(canonical-bytes)).
func (tx *JoyueDeployTx) Hash() common.Hash {
	if h := tx.hash.Load(); h != nil {
		return h.(common.Hash)
	}
	b, err := tx.CanonicalBytes()
	if err != nil {
		// should never happen for in-memory tx
		return common.Hash{}
	}
	v := hash.Keccak256Hash(b)
	tx.hash.Store(v)
	return v
}

// EncodeRLP implements rlp.Encoder for the payload (without type byte).
func (tx *JoyueDeployTx) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &tx.data)
}

// DecodeRLP implements rlp.Decoder for the payload (without type byte).
func (tx *JoyueDeployTx) DecodeRLP(s *rlp.Stream) error {
	_, _, _ = s.Kind()
	if err := s.Decode(&tx.data); err != nil {
		return err
	}
	tx.time = time.Now()
	return nil
}

// SenderAddress extracts the sender address of the transaction.
func (tx *JoyueDeployTx) SenderAddress() (common.Address, error) {
	var signer Signer
	if !tx.Protected() {
		signer = HomesteadSigner{}
	} else {
		signer = NewEIP155Signer(tx.ChainID())
	}
	addr, err := Sender(signer, tx)
	if err != nil {
		return common.Address{}, errors.WithMessage(err, "failed to extract sender address")
	}
	return addr, nil
}

// AsMessage converts the transaction into an executable message.
func (tx *JoyueDeployTx) AsMessage(s Signer) (Message, error) {
	msg := Message{
		nonce:      tx.data.AccountNonce,
		gasLimit:   tx.data.GasLimit,
		gasPrice:   new(big.Int).Set(tx.data.Price),
		to:         nil, // contract creation
		amount:     new(big.Int).Set(tx.data.Amount),
		data:       common.CopyBytes(tx.data.MasterInitCode),
		checkNonce: true,
	}
	var err error
	msg.from, err = Sender(s, tx)
	return msg, err
}

// WithSignature returns a new tx with the given signature.
func (tx *JoyueDeployTx) WithSignature(signer Signer, sig []byte) (*JoyueDeployTx, error) {
	r, s, v, err := signer.SignatureValues(tx, sig)
	if err != nil {
		return nil, err
	}
	cpy := &JoyueDeployTx{data: tx.data}
	cpy.data.R, cpy.data.S, cpy.data.V = r, s, v
	cpy.time = tx.time
	return cpy, nil
}

// ValidateSignature checks signature values validity.
func (tx *JoyueDeployTx) ValidateSignature() error {
	withSignature := tx.data.V.Sign() != 0 || tx.data.R.Sign() != 0 || tx.data.S.Sign() != 0
	if !withSignature {
		return nil
	}
	var V byte
	if isProtectedV(tx.data.V) {
		chainID := deriveChainID(tx.data.V).Uint64()
		V = byte(tx.data.V.Uint64() - 35 - 2*chainID)
	} else {
		V = byte(tx.data.V.Uint64() - 27)
	}
	if !crypto.ValidateSignatureValues(V, tx.data.R, tx.data.S, false) {
		return ErrInvalidSig
	}
	return nil
}

var errJoyueDeployDecode = errors.New("invalid joyue deploy tx encoding")

// DecodeJoyueDeployTxBytes decodes canonical bytes (typeByte||rlp(payload)).
func DecodeJoyueDeployTxBytes(b []byte) (*JoyueDeployTx, error) {
	if len(b) < 2 || b[0] != JoyueDeployTxType {
		return nil, errJoyueDeployDecode
	}
	var tx JoyueDeployTx
	if err := rlp.DecodeBytes(b[1:], &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}
