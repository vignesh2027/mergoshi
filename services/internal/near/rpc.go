package near

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client is a minimal NEAR JSON-RPC client.
type Client struct {
	URL  string
	HTTP *http.Client
	id   atomic.Int64
}

// NewClient returns a client for the given RPC URL.
func NewClient(url string) *Client {
	return &Client{URL: url, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// RPCError is an error returned by the node.
type RPCError struct {
	Name    string          `json:"name"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Cause   struct {
		Name string          `json:"name"`
		Info json.RawMessage `json:"info"`
	} `json:"cause"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc %s/%s: %s %s", e.Name, e.Cause.Name, e.Message, string(e.Data))
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params,
	})
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
		} else {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("rpc http %d: %s", resp.StatusCode, truncate(string(raw), 200))
			} else {
				var env struct {
					Result json.RawMessage `json:"result"`
					Error  *RPCError       `json:"error"`
				}
				if err := json.Unmarshal(raw, &env); err != nil {
					return fmt.Errorf("rpc decode: %w: %s", err, truncate(string(raw), 200))
				}
				if env.Error != nil {
					return env.Error
				}
				if out == nil {
					return nil
				}
				return json.Unmarshal(env.Result, out)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(300*(attempt+1)) * time.Millisecond):
		}
	}
	return lastErr
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Block is the part of a block header this project uses.
type Block struct {
	Hash   [32]byte
	Height uint64
}

// FinalBlock returns the latest final block.
func (c *Client) FinalBlock(ctx context.Context) (Block, error) {
	var out struct {
		Header struct {
			Hash   string `json:"hash"`
			Height uint64 `json:"height"`
		} `json:"header"`
	}
	if err := c.call(ctx, "block", map[string]any{"finality": "final"}, &out); err != nil {
		return Block{}, err
	}
	raw, err := Base58Decode(out.Header.Hash)
	if err != nil || len(raw) != 32 {
		return Block{}, errors.New("bad block hash")
	}
	var b Block
	copy(b.Hash[:], raw)
	b.Height = out.Header.Height
	return b, nil
}

// AccessKeyNonce returns the current nonce of an access key.
func (c *Client) AccessKeyNonce(ctx context.Context, account string, pub []byte) (uint64, error) {
	var out struct {
		Nonce uint64 `json:"nonce"`
	}
	err := c.call(ctx, "query", map[string]any{
		"request_type": "view_access_key", "finality": "optimistic",
		"account_id": account, "public_key": "ed25519:" + Base58Encode(pub),
	}, &out)
	return out.Nonce, err
}

// AccountBalance returns an account's NEAR balance in yoctoNEAR, or ok=false if it does not exist.
func (c *Client) AccountBalance(ctx context.Context, account string) (*big.Int, bool, error) {
	var out struct {
		Amount string `json:"amount"`
	}
	err := c.call(ctx, "query", map[string]any{
		"request_type": "view_account", "finality": "optimistic", "account_id": account,
	}, &out)
	if err != nil {
		var re *RPCError
		if errors.As(err, &re) && (re.Cause.Name == "UNKNOWN_ACCOUNT" || strings.Contains(string(re.Data), "does not exist")) {
			return nil, false, nil
		}
		return nil, false, err
	}
	v, _ := new(big.Int).SetString(out.Amount, 10)
	return v, true, nil
}

// View calls a view method and decodes its JSON result into out.
func (c *Client) View(ctx context.Context, contract, method string, args any, out any) error {
	argBytes, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if args == nil {
		argBytes = []byte("{}")
	}
	var res struct {
		Result []byte `json:"result"`
		Error  string `json:"error"`
	}
	err = c.call(ctx, "query", map[string]any{
		"request_type": "call_function", "finality": "optimistic",
		"account_id": contract, "method_name": method,
		"args_base64": base64.StdEncoding.EncodeToString(argBytes),
	}, &res)
	if err != nil {
		return err
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res.Result, out)
}

// Outcome is the result of a transaction once its receipts ran.
type Outcome struct {
	Hash    string
	Logs    []string
	Value   []byte // return value of the last successful function call
	Failure string // empty on success
}

// Failed reports whether any receipt failed.
func (o *Outcome) Failed() bool { return o.Failure != "" }

type execStatus struct {
	SuccessValue     *string         `json:"SuccessValue"`
	SuccessReceiptID *string         `json:"SuccessReceiptId"`
	Failure          json.RawMessage `json:"Failure"`
}

// SendTx submits a signed transaction and waits until all its receipts executed.
func (c *Client) SendTx(ctx context.Context, signed []byte) (*Outcome, error) {
	var out struct {
		Status             execStatus `json:"status"`
		TransactionOutcome struct {
			ID string `json:"id"`
		} `json:"transaction_outcome"`
		ReceiptsOutcome []struct {
			Outcome struct {
				Logs       []string   `json:"logs"`
				Status     execStatus `json:"status"`
				ExecutorID string     `json:"executor_id"`
			} `json:"outcome"`
		} `json:"receipts_outcome"`
	}
	err := c.call(ctx, "send_tx", map[string]any{
		"signed_tx_base64": base64.StdEncoding.EncodeToString(signed),
		"wait_until":       "EXECUTED_OPTIMISTIC",
	}, &out)
	if err != nil {
		return nil, err
	}
	o := &Outcome{Hash: out.TransactionOutcome.ID}
	if len(out.Status.Failure) > 0 {
		o.Failure = FailureMessage(out.Status.Failure)
	}
	for _, r := range out.ReceiptsOutcome {
		o.Logs = append(o.Logs, r.Outcome.Logs...)
		if len(r.Outcome.Status.Failure) > 0 && o.Failure == "" {
			o.Failure = FailureMessage(r.Outcome.Status.Failure)
		}
		if v := r.Outcome.Status.SuccessValue; v != nil && *v != "" {
			o.Value, _ = base64.StdEncoding.DecodeString(*v)
		}
	}
	if v := out.Status.SuccessValue; v != nil && *v != "" {
		o.Value, _ = base64.StdEncoding.DecodeString(*v)
	}
	return o, nil
}

// FailureMessage pulls the human message out of a NEAR failure object.
// Contract panics come through as ActionError.kind.FunctionCallError.ExecutionError.
func FailureMessage(raw json.RawMessage) string {
	var f struct {
		ActionError struct {
			Kind struct {
				FunctionCallError struct {
					ExecutionError string `json:"ExecutionError"`
				} `json:"FunctionCallError"`
			} `json:"kind"`
		} `json:"ActionError"`
	}
	if json.Unmarshal(raw, &f) == nil {
		if msg := f.ActionError.Kind.FunctionCallError.ExecutionError; msg != "" {
			msg = strings.TrimPrefix(msg, "Smart contract panicked: ")
			return msg
		}
	}
	return string(raw)
}
