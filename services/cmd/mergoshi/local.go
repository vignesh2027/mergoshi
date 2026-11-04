package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mergoshi/internal/config"
	"mergoshi/internal/near"
)

type localOpts struct {
	dir     string
	port    string
	p2p     string
	reset   bool
	github  string // "fake" or "real"
	only    string
	until   bool
	every   time.Duration
	serveOn string
}

// runLocal runs everything on this machine: a near-sandbox node, the token,
// three attesters and the dashboard. State lives in .localnet/.
func runLocal(ctx context.Context, o localOpts) error {
	if o.reset {
		for _, p := range []string{"home", "env", "node.log", "state"} {
			if err := os.RemoveAll(filepath.Join(o.dir, p)); err != nil {
				return err
			}
		}
		fmt.Println("local network reset")
	}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return err
	}
	env, err := config.Load(filepath.Join(o.dir, "env"))
	if err != nil {
		return err
	}
	for k, v := range map[string]string{
		"NEAR_RPC_URL":              "http://127.0.0.1:" + o.port,
		"NEAR_EXPLORER_URL":         "",
		"MERGOSHI_FAST":             "1",
		"MERGOSHI_STATE_DIR":        filepath.Join(o.dir, "state"),
		"MERGOSHI_TOKEN_FUNDING":    "10",
		"MERGOSHI_ATTESTER_FUNDING": "5",
	} {
		if env.Get(k) == "" {
			if err := env.Set(k, v); err != nil {
				return err
			}
		}
	}
	rpc, stop, err := localNode(ctx, o.dir, o.port, o.p2p)
	if err != nil {
		return err
	}
	defer stop()
	fmt.Println("local NEAR node running at", env.Get("NEAR_RPC_URL"))
	a := &app{env: env, rpc: rpc}

	if env.Get("MERGOSHI_ROOT") == "" {
		genesis, err := genesisAccount(rpc, o.dir)
		if err != nil {
			return err
		}
		k, err := near.GenerateKey()
		if err != nil {
			return err
		}
		id := "mergoshi." + genesis.ID
		if _, err := genesis.Send(ctx, id, near.CreateAccount{}, near.Transfer{Deposit: near.MustNEAR("1000")}, near.AddKey{PublicKey: k.Public}); err != nil {
			return fmt.Errorf("create %s: %w", id, err)
		}
		if err := env.Set("MERGOSHI_ROOT", id); err != nil {
			return err
		}
		if err := env.Set("MERGOSHI_ROOT_KEY", k.SecretKeyString()); err != nil {
			return err
		}
	}
	if err := a.bootstrap(ctx); err != nil {
		return err
	}
	if o.github == "real" {
		_ = env.Set("GITHUB_API_URL", "")
	}
	return a.runAttest(ctx, "1,2,3", o.only, o.every, o.until)
}
