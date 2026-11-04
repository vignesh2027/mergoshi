package near

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
)

// Borsh writer for the subset of NEAR types this project signs.
type borsh struct{ buf []byte }

func (w *borsh) u8(v uint8)   { w.buf = append(w.buf, v) }
func (w *borsh) u32(v uint32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }
func (w *borsh) u64(v uint64) { w.buf = binary.LittleEndian.AppendUint64(w.buf, v) }
func (w *borsh) raw(b []byte) { w.buf = append(w.buf, b...) }
func (w *borsh) bytes(b []byte) {
	w.u32(uint32(len(b)))
	w.raw(b)
}
func (w *borsh) str(s string) { w.bytes([]byte(s)) }

// u128 writes a non-negative big integer as 16 little-endian bytes.
func (w *borsh) u128(v *big.Int) {
	var out [16]byte
	if v != nil {
		be := v.Bytes()
		for i := 0; i < len(be) && i < 16; i++ {
			out[i] = be[len(be)-1-i]
		}
	}
	w.raw(out[:])
}

func (w *borsh) pubkey(pk []byte) {
	w.u8(0) // ed25519
	w.raw(pk)
}

// Yocto is the number of yoctoNEAR in one NEAR.
var Yocto = new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)

// ParseNEAR turns a decimal NEAR amount such as "0.01" into yoctoNEAR.
func ParseNEAR(s string) (*big.Int, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 24 {
		return nil, errors.New("too many decimals")
	}
	frac += strings.Repeat("0", 24-len(frac))
	v, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return nil, errors.New("invalid NEAR amount")
	}
	return v, nil
}

// MustNEAR is ParseNEAR for constants.
func MustNEAR(s string) *big.Int {
	v, err := ParseNEAR(s)
	if err != nil {
		panic(err)
	}
	return v
}

// TGas is one teragas.
const TGas uint64 = 1_000_000_000_000

// Action is one NEAR transaction action.
type Action interface{ encode(w *borsh) }

// CreateAccount creates the receiver account.
type CreateAccount struct{}

// DeployContract deploys wasm code to the receiver.
type DeployContract struct{ Code []byte }

// FunctionCall calls a method on the receiver.
type FunctionCall struct {
	Method  string
	Args    []byte
	Gas     uint64
	Deposit *big.Int
}

// Transfer sends NEAR to the receiver.
type Transfer struct{ Deposit *big.Int }

// AddKey adds an access key to the receiver. A nil Permission means full access.
type AddKey struct {
	PublicKey  []byte
	Permission *FunctionCallPermission
}

// FunctionCallPermission restricts a key to calling one contract.
type FunctionCallPermission struct {
	Allowance   *big.Int // nil means unlimited
	ReceiverID  string
	MethodNames []string // empty means any method
}

// Delegate wraps a signed meta transaction (NEP-366).
type Delegate struct{ Signed SignedDelegateAction }

func (CreateAccount) encode(w *borsh) { w.u8(0) }
func (a DeployContract) encode(w *borsh) {
	w.u8(1)
	w.bytes(a.Code)
}
func (a FunctionCall) encode(w *borsh) {
	w.u8(2)
	w.str(a.Method)
	w.bytes(a.Args)
	w.u64(a.Gas)
	w.u128(a.Deposit)
}
func (a Transfer) encode(w *borsh) {
	w.u8(3)
	w.u128(a.Deposit)
}
func (a AddKey) encode(w *borsh) {
	w.u8(5)
	w.pubkey(a.PublicKey)
	w.u64(0) // access key nonce
	if a.Permission == nil {
		w.u8(1) // FullAccess
		return
	}
	w.u8(0) // FunctionCall
	if a.Permission.Allowance == nil {
		w.u8(0)
	} else {
		w.u8(1)
		w.u128(a.Permission.Allowance)
	}
	w.str(a.Permission.ReceiverID)
	w.u32(uint32(len(a.Permission.MethodNames)))
	for _, m := range a.Permission.MethodNames {
		w.str(m)
	}
}
func (a Delegate) encode(w *borsh) {
	w.u8(8)
	a.Signed.encode(w)
}

// Transaction is a NEAR transaction (version 0 layout).
type Transaction struct {
	SignerID   string
	PublicKey  []byte
	Nonce      uint64
	ReceiverID string
	BlockHash  [32]byte
	Actions    []Action
}

func (t Transaction) encode(w *borsh) {
	w.str(t.SignerID)
	w.pubkey(t.PublicKey)
	w.u64(t.Nonce)
	w.str(t.ReceiverID)
	w.raw(t.BlockHash[:])
	w.u32(uint32(len(t.Actions)))
	for _, a := range t.Actions {
		a.encode(w)
	}
}

// Bytes returns the borsh encoding of the transaction.
func (t Transaction) Bytes() []byte {
	var w borsh
	t.encode(&w)
	return w.buf
}

// Sign returns the borsh-encoded SignedTransaction and the transaction hash.
func (t Transaction) Sign(k KeyPair) (signed []byte, hash [32]byte) {
	body := t.Bytes()
	hash = sha256.Sum256(body)
	sig := k.Sign(hash[:])
	var w borsh
	w.raw(body)
	w.u8(0)
	w.raw(sig)
	return w.buf, hash
}

// DelegateAction is the body of a NEP-366 meta transaction.
type DelegateAction struct {
	SenderID       string
	ReceiverID     string
	Actions        []Action // must not contain Delegate
	Nonce          uint64
	MaxBlockHeight uint64
	PublicKey      []byte
}

func (d DelegateAction) encode(w *borsh) {
	w.str(d.SenderID)
	w.str(d.ReceiverID)
	w.u32(uint32(len(d.Actions)))
	for _, a := range d.Actions {
		a.encode(w)
	}
	w.u64(d.Nonce)
	w.u64(d.MaxBlockHeight)
	w.pubkey(d.PublicKey)
}

// nep366Prefix is the borsh u32 message discriminant (2^30 + 366) required by NEP-461.
const nep366Prefix uint32 = 1<<30 + 366

// SigningHash returns the hash the sender signs for a delegate action.
func (d DelegateAction) SigningHash() [32]byte {
	var w borsh
	w.u32(nep366Prefix)
	d.encode(&w)
	return sha256.Sum256(w.buf)
}

// SignedDelegateAction is a delegate action with the sender's signature.
type SignedDelegateAction struct {
	Action    DelegateAction
	Signature []byte
}

func (s SignedDelegateAction) encode(w *borsh) {
	s.Action.encode(w)
	w.u8(0)
	w.raw(s.Signature)
}

// SignDelegate signs a delegate action with the sender's key.
func SignDelegate(d DelegateAction, k KeyPair) SignedDelegateAction {
	h := d.SigningHash()
	return SignedDelegateAction{Action: d, Signature: k.Sign(h[:])}
}
