package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"mergoshi/internal/attester"
	"mergoshi/internal/config"
	"mergoshi/internal/github"
	"mergoshi/internal/near"
	"mergoshi/internal/token"
)

type app struct {
	env *config.Env
	rpc *near.Client
}

func loadApp(envPath string) (*app, error) {
	env, err := config.Load(envPath)
	if err != nil {
		return nil, err
	}
	return &app{env: env, rpc: near.NewClient(env.Or("NEAR_RPC_URL", "https://rpc.testnet.fastnear.com"))}, nil
}

func (a *app) explorer(hash string) string {
	base := a.env.Or("NEAR_EXPLORER_URL", "https://testnet.nearblocks.io")
	if base == "none" {
		return "tx " + hash
	}
	return strings.TrimRight(base, "/") + "/txns/" + hash
}

func (a *app) account(idKey, keyKey string) (*near.Account, error) {
	id, err := a.env.Must(idKey)
	if err != nil {
		return nil, err
	}
	raw, err := a.env.Must(keyKey)
	if err != nil {
		return nil, err
	}
	k, err := near.ParseSecretKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyKey, err)
	}
	return near.NewAccount(a.rpc, id, k), nil
}

func (a *app) root() (*near.Account, error) { return a.account("MERGOSHI_ROOT", "MERGOSHI_ROOT_KEY") }

func (a *app) tokenID() (string, error) { return a.env.Must("MERGOSHI_TOKEN") }

func (a *app) client() (*token.Client, error) {
	id, err := a.tokenID()
	if err != nil {
		return nil, err
	}
	return &token.Client{RPC: a.rpc, Contract: id}, nil
}

func attesterEnv(i int) (string, string) {
	return fmt.Sprintf("MERGOSHI_ATTESTER_%d", i), fmt.Sprintf("MERGOSHI_ATTESTER_%d_KEY", i)
}

func (a *app) exists(ctx context.Context, id string) bool {
	_, ok, err := a.rpc.AccountBalance(ctx, id)
	return err == nil && ok
}

func (a *app) waitForAccount(ctx context.Context, id string) error {
	for i := 0; i < 30; i++ {
		if a.exists(ctx, id) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("account %s did not appear", id)
}

func e18(n int64) token.U128 {
	return token.U128(new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)).String())
}

// launchParams are the testnet demo parameters described in DESIGN.md section 5.
func launchParams(attesters []string, fast bool) token.Params {
	repos := make([]string, len(DefaultRepos))
	for i, r := range DefaultRepos {
		repos[i] = strings.ToLower(r)
	}
	p := token.Params{
		Attesters: attesters, Quorum: len(attesters)/2 + 1,
		Bond:             token.U128(near.MustNEAR("0.2").String()),
		CommitWindowSecs: 120, RevealWindowSecs: 180, ExitDelaySecs: 900, SlashBps: 2000,
		BaseReward: e18(100), HalvingEvery: 10_000,
		AuthorBps: 7000, ReviewBps: 2000, AttesterBps: 500, FeastBps: 500,
		MaxPaidReviewers: 3, StreakStepBps: 1000, StreakMaxBonusBps: 4000,
		RepoDailyCap: 12, RepoFloor: 1, AuthorsPerSlot: 3, AuthorDailyCap: 4,
		MaxMergeAgeSecs:  7 * 86400,
		MinGitHubAgeDays: 90, MinRepoStars: 100, MinRepoAgeDays: 180,
		RepoEnrollBurn: e18(500), ExileMin: e18(1000),
		FeastPeriodSecs: 6 * 3600, FeastBurnBps: 5000, FeastMaxGuests: 20,
		EscrowTTLSecs:  7 * 86400,
		RequestDeposit: token.U128(near.MustNEAR("0.01").String()),
		GenesisRepos:   repos,
	}
	if fast {
		// Local sandbox: rituals every few minutes so a demo can show them.
		p.CommitWindowSecs, p.RevealWindowSecs, p.ExitDelaySecs = 20, 30, 60
		p.FeastPeriodSecs, p.EscrowTTLSecs = 180, 600
		p.RepoEnrollBurn, p.ExileMin = e18(50), e18(100)
	}
	return p
}

// bootstrap creates the root account through the public testnet helper, three
// attester accounts, deploys the token and bonds the attesters. Finished steps
// are skipped, so it is safe to run again.
func (a *app) bootstrap(ctx context.Context) error {
	if a.env.Get("MERGOSHI_ROOT") == "" {
		k, err := near.GenerateKey()
		if err != nil {
			return err
		}
		var b [3]byte
		_, _ = rand.Read(b[:])
		id := "mergoshi-" + hex.EncodeToString(b[:]) + ".testnet"
		helper := a.env.Or("NEAR_HELPER_URL", "https://helper.nearprotocol.com")
		fmt.Println("creating root account", id, "through the testnet helper")
		if err := near.FaucetCreate(ctx, helper, id, k.Public); err != nil {
			return err
		}
		if err := a.env.Set("MERGOSHI_ROOT", id); err != nil {
			return err
		}
		if err := a.env.Set("MERGOSHI_ROOT_KEY", k.SecretKeyString()); err != nil {
			return err
		}
		if err := a.waitForAccount(ctx, id); err != nil {
			return err
		}
	}
	root, err := a.root()
	if err != nil {
		return err
	}
	fmt.Println("root:", root.ID)

	var attesters []string
	for i := 1; i <= 3; i++ {
		idKey, keyKey := attesterEnv(i)
		id := fmt.Sprintf("attester-%d.%s", i, root.ID)
		attesters = append(attesters, id)
		if a.exists(ctx, id) {
			continue
		}
		k, err := near.GenerateKey()
		if err != nil {
			return err
		}
		out, err := root.Send(ctx, id, near.CreateAccount{}, near.Transfer{Deposit: near.MustNEAR(a.env.Or("MERGOSHI_ATTESTER_FUNDING", "0.45"))}, near.AddKey{PublicKey: k.Public})
		if err != nil {
			return fmt.Errorf("create %s: %w", id, err)
		}
		if err := a.env.Set(idKey, id); err != nil {
			return err
		}
		if err := a.env.Set(keyKey, k.SecretKeyString()); err != nil {
			return err
		}
		fmt.Println("created", id, a.explorer(out.Hash))
	}

	tokenID := "token." + root.ID
	if !a.exists(ctx, tokenID) {
		code, err := os.ReadFile(a.env.Or("MERGOSHI_WASM", "contract/target/near/mergoshi_token.wasm"))
		if err != nil {
			return fmt.Errorf("read contract wasm (build it with `make contract`): %w", err)
		}
		p := launchParams(attesters, a.env.Get("MERGOSHI_FAST") == "1")
		out, err := root.Send(ctx, tokenID,
			near.CreateAccount{},
			near.Transfer{Deposit: near.MustNEAR(a.env.Or("MERGOSHI_TOKEN_FUNDING", "6"))},
			near.DeployContract{Code: code},
			token.New(p),
		)
		if err != nil {
			return fmt.Errorf("deploy token: %w", err)
		}
		fmt.Println("deployed and launched", tokenID, a.explorer(out.Hash))
	}
	if err := a.env.Set("MERGOSHI_TOKEN", tokenID); err != nil {
		return err
	}
	if err := a.bondAll(ctx); err != nil {
		return err
	}
	if a.env.Get("MERGOSHI_SALT_SECRET") == "" {
		var b [32]byte
		_, _ = rand.Read(b[:])
		if err := a.env.Set("MERGOSHI_SALT_SECRET", hex.EncodeToString(b[:])); err != nil {
			return err
		}
	}
	fmt.Println("bootstrap complete")
	return a.status(ctx)
}

func (a *app) bondAll(ctx context.Context) error {
	tc, err := a.client()
	if err != nil {
		return err
	}
	p, err := tc.Params(ctx)
	if err != nil {
		return err
	}
	atts, err := tc.Attesters(ctx)
	if err != nil {
		return err
	}
	for i := 1; i <= len(atts); i++ {
		idKey, keyKey := attesterEnv(i)
		acc, err := a.account(idKey, keyKey)
		if err != nil {
			return err
		}
		for _, st := range atts {
			if st.ID == acc.ID && !st.Active {
				out, err := acc.Send(ctx, tc.Contract, token.Bond(p.Bond.Big()))
				if err != nil {
					return fmt.Errorf("bond %s: %w", acc.ID, err)
				}
				fmt.Println("bonded", acc.ID, a.explorer(out.Hash))
			}
		}
	}
	return nil
}

func (a *app) status(ctx context.Context) error {
	tc, err := a.client()
	if err != nil {
		return err
	}
	supply, err := tc.TotalSupply(ctx)
	if err != nil {
		return err
	}
	st, err := tc.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("token %s: supply %s MRGO, minted %s, burned %s, pot %s, escrow %s\n", tc.Contract,
		supply.Tokens(2), st.TotalMinted.Tokens(2), st.TotalBurned.Tokens(2), st.FeastPot.Tokens(2), st.EscrowOutstanding.Tokens(2))
	fmt.Printf("merges minted %d, capped %d, stale %d, young %d, rejected %d, links %d, repos %d\n",
		st.MergesMinted, st.MergesCapped, st.MergesStale, st.MergesYoung, st.RoundsRejected, st.Links, st.Repos)
	atts, err := tc.Attesters(ctx)
	if err != nil {
		return err
	}
	for _, x := range atts {
		bal, _, _ := a.rpc.AccountBalance(ctx, x.ID)
		fmt.Printf("  attester %-40s active %-5v bond %s NEAR, settled %d, slashes %d, balance %s NEAR\n", x.ID, x.Active,
			token.FormatUnits(x.Bond.Big(), 24, 3), x.Settled, x.Slashes, token.FormatUnits(orZero(bal), 24, 3))
	}
	if root := a.env.Get("MERGOSHI_ROOT"); root != "" {
		bal, _, _ := a.rpc.AccountBalance(ctx, root)
		fmt.Printf("root %s: %s NEAR\n", root, token.FormatUnits(orZero(bal), 24, 3))
	}
	return nil
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}

// attesters builds one Attester per configured account.
func (a *app) attesters(only map[string]bool, which []int) ([]*attester.Attester, error) {
	tc, err := a.client()
	if err != nil {
		return nil, err
	}
	secretHex, err := a.env.Must("MERGOSHI_SALT_SECRET")
	if err != nil {
		return nil, err
	}
	secret, err := hex.DecodeString(secretHex)
	if err != nil {
		return nil, err
	}
	gh := github.New(a.env.Get("GITHUB_API_URL"), a.env.Get("GITHUB_TOKEN"))
	var out []*attester.Attester
	for _, i := range which {
		idKey, keyKey := attesterEnv(i)
		acc, err := a.account(idKey, keyKey)
		if err != nil {
			return nil, err
		}
		// Each attester gets its own salt secret and its own GitHub client.
		s := append(append([]byte{}, secret...), []byte(acc.ID)...)
		cfg := attester.Config{
			ID: acc.ID, Secret: s, Keeper: i == 1, Only: only,
			StatePath: fmt.Sprintf("%s/attester-%d.json", a.env.Or("MERGOSHI_STATE_DIR", ".state"), i),
		}
		ghc := github.New(gh.Base, gh.Token)
		out = append(out, attester.New(cfg, attester.NearChain{Client: tc, Signer: acc}, ghc))
	}
	return out, nil
}
