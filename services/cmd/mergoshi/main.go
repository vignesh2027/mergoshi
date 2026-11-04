// Command mergoshi runs the attesters, the dashboard and the setup tasks for the
// Mergoshi token.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"mergoshi/internal/github"
)

const usage = `usage: mergoshi <command> [flags]

commands:
  bootstrap  create the testnet accounts, deploy and launch the token, bond the attesters
  status     print supply, counters and attesters
  attest     run the attesters (flags: -which 1,2,3  -only key,key  -every 20s  -until-settled)
  local      run a sandbox node, the token, three attesters and the dashboard on this machine
  serve      run the dashboard (flags: -addr :8080  -attesters)
  snapshot   record public merge history for the simulation (sim/data/snapshot.json)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	envPath := os.Getenv("MERGOSHI_ENV")
	if envPath == "" {
		envPath = ".env"
	}
	switch cmd {
	case "bootstrap", "status", "attest", "serve":
		a, lerr := loadApp(envPath)
		if lerr != nil {
			err = lerr
			break
		}
		switch cmd {
		case "bootstrap":
			err = a.bootstrap(ctx)
		case "status":
			err = a.status(ctx)
		case "serve":
			fs := flag.NewFlagSet(cmd, flag.ExitOnError)
			addr := fs.String("addr", ":"+a.env.Or("PORT", "8080"), "listen address")
			withAttesters := fs.Bool("attesters", false, "also run the three attesters in this process")
			_ = fs.Parse(args)
			err = a.serve(ctx, *addr, "testnet", *withAttesters, nil)
		case "attest":
			fs := flag.NewFlagSet(cmd, flag.ExitOnError)
			which := fs.String("which", "1,2,3", "attester numbers to run in this process")
			only := fs.String("only", "", "comma separated merge keys to attest (default: every merge)")
			every := fs.Duration("every", 20*time.Second, "time between passes")
			until := fs.Bool("until-settled", false, "stop once every -only key has settled")
			_ = fs.Parse(args)
			err = a.runAttest(ctx, *which, *only, *every, *until)
		}
	case "local":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		o := localOpts{}
		fs.StringVar(&o.dir, "dir", ".localnet", "state directory")
		fs.StringVar(&o.port, "rpc-port", "3031", "local node RPC port")
		fs.StringVar(&o.p2p, "p2p-port", "24569", "local node network port")
		fs.BoolVar(&o.reset, "reset", false, "start from a fresh chain")
		fs.StringVar(&o.github, "github", "fake", "fake (local simulated GitHub) or real (public GitHub API)")
		fs.StringVar(&o.only, "only", "", "comma separated merge keys to attest")
		fs.BoolVar(&o.until, "until-settled", false, "stop once every -only key has settled")
		fs.DurationVar(&o.every, "every", 5*time.Second, "time between attester passes")
		fs.StringVar(&o.addr, "addr", ":8080", "dashboard listen address")
		fs.StringVar(&o.snapshot, "snapshot", "sim/data/snapshot.json", "repository data for the fake GitHub")
		_ = fs.Parse(args)
		err = runLocal(ctx, o)
	case "snapshot":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		out := fs.String("out", "sim/data/snapshot.json", "output file")
		days := fs.Int("days", 90, "days of history")
		owner := fs.String("owner", "vignesh2027", "GitHub login whose merges seed the demo")
		sample := fs.Int("reviews", 45, "merges to sample for the approvals distribution")
		_ = fs.Parse(args)
		gh := github.New(os.Getenv("GITHUB_API_URL"), os.Getenv("GITHUB_TOKEN"))
		err = runSnapshot(ctx, gh, *out, *days, *owner, *sample)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func (a *app) runAttest(ctx context.Context, which, only string, every time.Duration, until bool) error {
	var nums []int
	for _, s := range strings.Split(which, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("-which: %w", err)
		}
		nums = append(nums, n)
	}
	var keys map[string]bool
	if only != "" {
		keys = map[string]bool{}
		for _, k := range strings.Split(only, ",") {
			keys[strings.ToLower(strings.TrimSpace(k))] = true
		}
	}
	atts, err := a.attesters(keys, nums)
	if err != nil {
		return err
	}
	if !until {
		var wg sync.WaitGroup
		for _, at := range atts {
			wg.Add(1)
			go func() { defer wg.Done(); _ = at.Run(ctx, every) }()
		}
		wg.Wait()
		return nil
	}
	if keys == nil {
		return fmt.Errorf("-until-settled needs -only")
	}
	tc, err := a.client()
	if err != nil {
		return err
	}
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	for _, at := range atts {
		go func() { _ = at.Run(runCtx, every) }()
	}
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		settled := true
		for k := range keys {
			done, err := tc.IsDone(ctx, k)
			if err != nil || !done {
				settled = false
			}
		}
		if settled {
			// Give late reveals and sweeps a moment to land.
			time.Sleep(3 * every)
			stopRun()
			for _, at := range atts {
				for _, h := range at.RecentTx() {
					fmt.Println("  ", a.explorer(h))
				}
			}
			return a.status(ctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("keys did not settle in time")
}
