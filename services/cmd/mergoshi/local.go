package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mergoshi/internal/config"
	"mergoshi/internal/fakegh"
	"mergoshi/internal/near"
)

type localOpts struct {
	dir      string
	port     string
	p2p      string
	reset    bool
	github   string // "fake" or "real"
	only     string
	until    bool
	every    time.Duration
	addr     string
	snapshot string
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
		"NEAR_EXPLORER_URL":         "none",
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
	// A contributor account for the demo buttons (linking, proposing, exiling).
	contributorID := "vignesh." + env.Get("MERGOSHI_ROOT")
	if env.Get("MERGOSHI_CONTRIBUTOR_KEY") == "" {
		root, err := a.root()
		if err != nil {
			return err
		}
		k, err := near.GenerateKey()
		if err != nil {
			return err
		}
		if _, err := root.Send(ctx, contributorID, near.CreateAccount{}, near.Transfer{Deposit: near.MustNEAR("20")}, near.AddKey{PublicKey: k.Public}); err != nil {
			return fmt.Errorf("create %s: %w", contributorID, err)
		}
		if err := env.Set("MERGOSHI_CONTRIBUTOR_KEY", k.SecretKeyString()); err != nil {
			return err
		}
	}
	ck, err := near.ParseSecretKey(env.Get("MERGOSHI_CONTRIBUTOR_KEY"))
	if err != nil {
		return err
	}
	contributor := near.NewAccount(rpc, contributorID, ck)

	if o.github == "real" {
		_ = env.Set("GITHUB_API_URL", "")
		return a.runAttest(ctx, "1,2,3", o.only, o.every, o.until)
	}
	host := "127.0.0.1" + o.addr
	if !strings.HasPrefix(o.addr, ":") {
		host = o.addr
	}
	store := fakegh.New("http://" + host + "/gh")
	seedFake(store, o.snapshot)
	if err := env.Set("GITHUB_API_URL", "http://"+host+"/gh"); err != nil {
		return err
	}
	fmt.Println("\nOpen http://localhost" + o.addr + " for the live page and http://localhost" + o.addr + "/fake to merge a pull request.")
	fmt.Println("Press Ctrl+C to stop. Run with -reset to start a fresh chain.")
	return a.serve(ctx, o.addr, "local", true, a.demoRoutes(ctx, store, contributor))
}
