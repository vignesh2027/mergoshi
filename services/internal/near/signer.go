package near

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Account signs and sends transactions for one NEAR account. It may hold
// several full access keys and spreads transactions across them, so it can
// send in parallel without nonce clashes.
type Account struct {
	ID     string
	client *Client
	keys   []*poolKey
	next   atomic.Uint64
	blocks *blockCache
}

type poolKey struct {
	mu    sync.Mutex
	kp    KeyPair
	nonce uint64 // 0 means unknown
}

// NewAccount returns a signer for id using the given keys.
func NewAccount(c *Client, id string, keys ...KeyPair) *Account {
	a := &Account{ID: id, client: c, blocks: newBlockCache(c)}
	for _, k := range keys {
		a.keys = append(a.keys, &poolKey{kp: k})
	}
	return a
}

// Keys returns the account's key pairs.
func (a *Account) Keys() []KeyPair {
	out := make([]KeyPair, len(a.keys))
	for i, k := range a.keys {
		out[i] = k.kp
	}
	return out
}

// Client returns the RPC client.
func (a *Account) Client() *Client { return a.client }

// Send signs a transaction to receiver with the given actions and waits for its outcome.
// A failed receipt is returned as an error carrying the contract's message.
func (a *Account) Send(ctx context.Context, receiver string, actions ...Action) (*Outcome, error) {
	if len(a.keys) == 0 {
		return nil, errors.New("account has no keys")
	}
	k := a.keys[a.next.Add(1)%uint64(len(a.keys))]
	k.mu.Lock()
	defer k.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if k.nonce == 0 {
			n, err := a.client.AccessKeyNonce(ctx, a.ID, k.kp.Public)
			if err != nil {
				return nil, fmt.Errorf("nonce for %s: %w", a.ID, err)
			}
			k.nonce = n
		}
		blk, err := a.blocks.get(ctx)
		if err != nil {
			return nil, err
		}
		k.nonce++
		tx := Transaction{
			SignerID: a.ID, PublicKey: k.kp.Public, Nonce: k.nonce,
			ReceiverID: receiver, BlockHash: blk.Hash, Actions: actions,
		}
		signed, _ := tx.Sign(k.kp)
		out, err := a.client.SendTx(ctx, signed)
		if err != nil {
			lastErr = err
			if isNonceError(err) || isExpired(err) {
				k.nonce = 0
				a.blocks.reset()
				continue
			}
			if isTimeout(err) {
				// The transaction may still land; resync the nonce before the next one.
				k.nonce = 0
			}
			return nil, err
		}
		if out.Failed() {
			return out, &TxError{Hash: out.Hash, Message: out.Failure}
		}
		return out, nil
	}
	return nil, lastErr
}

// TxError is a transaction that was included but failed.
type TxError struct {
	Hash    string
	Message string
}

func (e *TxError) Error() string { return e.Message }

func isNonceError(err error) bool {
	var re *RPCError
	return errors.As(err, &re) && (strings.Contains(string(re.Data), "InvalidNonce") || strings.Contains(string(re.Cause.Info), "InvalidNonce") || strings.Contains(re.Message, "nonce"))
}

func isExpired(err error) bool {
	var re *RPCError
	return errors.As(err, &re) && (strings.Contains(string(re.Data), "Expired") || strings.Contains(string(re.Cause.Info), "Expired"))
}

func isTimeout(err error) bool {
	var re *RPCError
	return errors.As(err, &re) && re.Cause.Name == "TIMEOUT_ERROR"
}

// CallArgs builds a no-deposit function call action with JSON args.
func CallArgs(method string, args any, gasTGas uint64) FunctionCall {
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return FunctionCall{Method: method, Args: b, Gas: gasTGas * TGas}
}

type blockCache struct {
	mu   sync.Mutex
	c    *Client
	blk  Block
	when time.Time
}

func newBlockCache(c *Client) *blockCache { return &blockCache{c: c} }

func (b *blockCache) get(ctx context.Context) (Block, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.when) < 60*time.Second && b.blk.Height > 0 {
		return b.blk, nil
	}
	blk, err := b.c.FinalBlock(ctx)
	if err != nil {
		return Block{}, err
	}
	b.blk, b.when = blk, time.Now()
	return blk, nil
}

func (b *blockCache) reset() {
	b.mu.Lock()
	b.when = time.Time{}
	b.mu.Unlock()
}
