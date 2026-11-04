package near

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DeleteAccount deletes the receiver and sends its balance to the beneficiary.
type DeleteAccount struct{ Beneficiary string }

func (a DeleteAccount) encode(w *borsh) {
	w.u8(7)
	w.str(a.Beneficiary)
}

// FaucetCreate asks the testnet helper to create a funded top-level account
// with the given public key. No login is needed.
func FaucetCreate(ctx context.Context, helperURL, accountID string, pub []byte) error {
	body, _ := json.Marshal(map[string]string{
		"newAccountId":        accountID,
		"newAccountPublicKey": "ed25519:" + Base58Encode(pub),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, helperURL+"/account", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("faucet http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return nil
}
