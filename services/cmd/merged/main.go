// Command merged runs the attesters, the dashboard and the setup tasks for the
// Merged token.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"merged/internal/github"
)

const usage = `usage: merged <command> [flags]

commands:
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
	switch cmd {
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
