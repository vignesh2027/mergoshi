package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mergoshi/internal/github"
)

// DefaultRepos are the genesis repositories: projects the author has merged work into.
var DefaultRepos = []string{
	"open-telemetry/opentelemetry-go-compile-instrumentation",
	"open-telemetry/opentelemetry-go",
	"open-telemetry/opentelemetry-go-contrib",
	"open-telemetry/opentelemetry-operator",
	"prometheus/client_golang",
	"prometheus/alertmanager",
	"DataDog/orchestrion",
	"DataDog/go-sqllexer",
	"DataDog/datadog-go",
	"helm/chart-testing",
}

type snapshotRepo struct {
	Repo      string    `json:"repo"`
	ID        int64     `json:"id"`
	Stars     int       `json:"stars"`
	CreatedAt time.Time `json:"created_at"`
	// Merges are [merged_at unix seconds, author id, 1 if the author is a bot].
	Merges [][3]int64 `json:"merges"`
}

type snapshotReview struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Approvals int    `json:"approvals"`
}

type snapshot struct {
	Note      string            `json:"note"`
	FetchedAt time.Time         `json:"fetched_at"`
	From      time.Time         `json:"from"`
	To        time.Time         `json:"to"`
	Repos     []snapshotRepo    `json:"repos"`
	Logins    map[int64]string  `json:"logins"`
	Reviews   []snapshotReview  `json:"reviews_sample"`
	Owner     string            `json:"owner"`
	OwnerPRs  []snapshotOwnerPR `json:"owner_merges"`
}

type snapshotOwnerPR struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	MergedAt time.Time `json:"merged_at"`
}

// runSnapshot records public merge history for the simulation. It uses the
// unauthenticated API by default and takes about ten minutes because of the
// search rate limit.
func runSnapshot(ctx context.Context, gh *github.Client, out string, days int, owner string, reviewSample int) error {
	to := time.Now().UTC().Truncate(24 * time.Hour)
	from := to.AddDate(0, 0, -days)
	snap := snapshot{
		Note:      "Public merged pull request history used by sim/. Built by `mergoshi snapshot`.",
		FetchedAt: time.Now().UTC().Truncate(time.Second),
		From:      from, To: to, Logins: map[int64]string{}, Owner: owner,
	}
	type pick struct {
		repo   string
		number int
		author int64
	}
	var candidates []pick
	for _, repo := range DefaultRepos {
		info, err := gh.Repository(ctx, repo)
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		sr := snapshotRepo{Repo: info.FullName, ID: info.ID, Stars: info.Stars, CreatedAt: info.CreatedAt}
		for start := from; start.Before(to); start = start.AddDate(0, 0, 15) {
			end := start.AddDate(0, 0, 15).Add(-time.Second)
			if end.After(to) {
				end = to.Add(-time.Second)
			}
			ms, err := gh.SearchMerged(ctx, repo, start, end)
			if err != nil {
				return fmt.Errorf("%s: %w", repo, err)
			}
			for _, m := range ms {
				bot := int64(0)
				if m.Author.IsBot() {
					bot = 1
				} else {
					candidates = append(candidates, pick{repo, m.Number, m.Author.ID})
				}
				sr.Merges = append(sr.Merges, [3]int64{m.MergedAt.Unix(), m.Author.ID, bot})
				snap.Logins[m.Author.ID] = m.Author.Login
			}
		}
		fmt.Printf("%-58s %5d merges, %6d stars\n", repo, len(sr.Merges), sr.Stars)
		snap.Repos = append(snap.Repos, sr)
	}

	if owner != "" {
		ms, err := gh.SearchOwnerMerges(ctx, owner)
		if err != nil {
			return err
		}
		for _, m := range ms {
			snap.OwnerPRs = append(snap.OwnerPRs, snapshotOwnerPR{m.Repo, m.Number, m.MergedAt})
		}
		fmt.Printf("%s: %d merged pull requests\n", owner, len(ms))
	}

	// An evenly spaced sample of human merges for the approvals distribution.
	if n := len(candidates); n > 0 && reviewSample > 0 {
		step := max(n/reviewSample, 1)
		for i := 0; i < n && len(snap.Reviews) < reviewSample; i += step {
			c := candidates[i]
			apps, err := gh.Approvers(ctx, c.repo, c.number, c.author, 0, time.Now().Add(24*time.Hour))
			if err != nil {
				fmt.Println("reviews sample stopped early:", err)
				break
			}
			snap.Reviews = append(snap.Reviews, snapshotReview{c.repo, c.number, len(apps)})
		}
		fmt.Printf("reviews sampled on %d merges\n", len(snap.Reviews))
	}
	sort.Slice(snap.OwnerPRs, func(i, j int) bool { return snap.OwnerPRs[i].MergedAt.Before(snap.OwnerPRs[j].MergedAt) })
	raw, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, raw, 0o644)
}
