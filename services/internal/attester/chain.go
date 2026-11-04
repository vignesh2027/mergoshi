package attester

import (
	"context"

	"mergoshi/internal/near"
	"mergoshi/internal/token"
)

// NearChain is the Chain backed by a real contract and a signing account.
type NearChain struct {
	*token.Client
	Signer *near.Account
}

// Call signs and sends one function call to the contract.
func (c NearChain) Call(ctx context.Context, fc near.FunctionCall) ([]byte, string, error) {
	out, err := c.Signer.Send(ctx, c.Contract, fc)
	if err != nil {
		if out != nil {
			return nil, out.Hash, err
		}
		return nil, "", err
	}
	return out.Value, out.Hash, nil
}
