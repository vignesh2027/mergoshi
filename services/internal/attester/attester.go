// Package attester reads GitHub, commits to canonical claims and reveals them.
// Each attester runs with its own NEAR key and its own view of GitHub; nothing
// is shared between attesters except the contract.
package attester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mergoshi/internal/claim"
	"mergoshi/internal/github"
	"mergoshi/internal/near"
	"mergoshi/internal/token"
)

// Chain is what the attester needs from the contract.
type Chain interface {
	Params(ctx context.Context) (token.Params, error)
	Repos(ctx context.Context) ([]token.RepoInfo, error)
	OpenRounds(ctx context.Context) ([]token.Round, error)
	PendingLinks(ctx context.Context) ([]token.Request, error)
	PendingRepos(ctx context.Context) ([]token.Request, error)
	IsDone(ctx context.Context, key string) (bool, error)
	Attesters(ctx context.Context) ([]token.Attester, error)
	Stats(ctx context.Context) (token.Stats, error)
	Escrows(ctx context.Context) ([]token.Escrow, error)
	// Call sends one function call to the contract and returns its JSON result.
	Call(ctx context.Context, fc near.FunctionCall) ([]byte, string, error)
}

// Config tunes one attester.
type Config struct {
	ID       string // NEAR account
	Secret   []byte // derives salts; the attester's private key bytes work
	Lookback time.Duration
	// Keeper also calls the feast and burns stale escrow when due.
	Keeper bool
	// StatePath stores committed claims so a restart can still reveal.
	StatePath string
	// Only, when set, limits merge discovery to these keys (used for a first mint).
	Only map[string]bool
	// Liar flips every merge claim's author; for the slashing demo only.
	Liar bool
	Logf func(format string, args ...any)
}

// Attester is one attester's loop state.
type Attester struct {
	cfg   Config
	chain Chain
	gh    *github.Client

	mu        sync.Mutex
	committed map[string]*string // key to claim text (nil means NONE)
	done      map[string]bool
	users     map[int64]*github.User
	params    *token.Params
	since     time.Time
	lastTx    []string
}

// New returns an attester and loads its saved commitments.
func New(cfg Config, chain Chain, gh *github.Client) *Attester {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	a := &Attester{cfg: cfg, chain: chain, gh: gh, committed: map[string]*string{}, done: map[string]bool{}, users: map[int64]*github.User{}}
	if cfg.StatePath != "" {
		if raw, err := os.ReadFile(cfg.StatePath); err == nil {
			_ = json.Unmarshal(raw, &a.committed)
		}
	}
	return a
}

func (a *Attester) save() {
	if a.cfg.StatePath == "" {
		return
	}
	raw, _ := json.Marshal(a.committed)
	_ = os.MkdirAll(filepath.Dir(a.cfg.StatePath), 0o700)
	_ = os.WriteFile(a.cfg.StatePath, raw, 0o600)
}

// Run ticks until the context ends.
func (a *Attester) Run(ctx context.Context, every time.Duration) error {
	for {
		if err := a.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.cfg.Logf("%s: %v", a.cfg.ID, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// Tick runs one pass: reveal what is ready, commit to new work, sweep, keep.
func (a *Attester) Tick(ctx context.Context) error {
	if a.params == nil {
		p, err := a.chain.Params(ctx)
		if err != nil {
			return err
		}
		a.params = &p
	}
	rounds, err := a.chain.OpenRounds(ctx)
	if err != nil {
		return err
	}
	atts, err := a.chain.Attesters(ctx)
	if err != nil {
		return err
	}
	active := 0
	for _, x := range atts {
		if x.Active {
			active++
		}
	}
	if err := a.reveal(ctx, rounds, active); err != nil {
		a.cfg.Logf("%s: reveal: %v", a.cfg.ID, err)
	}
	if err := a.commit(ctx, rounds); err != nil {
		a.cfg.Logf("%s: commit: %v", a.cfg.ID, err)
	}
	if err := a.sweep(ctx, rounds); err != nil {
		a.cfg.Logf("%s: sweep: %v", a.cfg.ID, err)
	}
	if a.cfg.Keeper {
		if err := a.keep(ctx); err != nil {
			a.cfg.Logf("%s: keeper: %v", a.cfg.ID, err)
		}
	}
	return nil
}

// RevealReady reports whether this attester should reveal in round r now.
func RevealReady(r token.Round, me string, active int, now int64) bool {
	if !r.HasCommitted(me) || r.HasRevealed(me) || now > r.RevealDeadline {
		return false
	}
	return now > r.CommitDeadline || len(r.Committed) >= active
}

// Sweepable reports whether round r can be closed.
func Sweepable(r token.Round, now int64) bool {
	if r.Outcome != nil {
		return len(r.Revealed) == len(r.Committed) || now > r.RevealDeadline
	}
	return now > r.RevealDeadline
}

func (a *Attester) reveal(ctx context.Context, rounds []token.Round, active int) error {
	now := time.Now().Unix()
	var items []token.RevealItem
	for _, r := range rounds {
		if !RevealReady(r, a.cfg.ID, active, now) {
			continue
		}
		text, ok := a.committed[r.Key]
		if !ok {
			// Lost state: rebuild the claim and hope GitHub still says the same.
			t, err := a.build(ctx, r.Key)
			if err != nil {
				continue
			}
			text = t
		}
		salt := claim.Salt(a.cfg.Secret, r.Key)
		items = append(items, token.RevealItem{Key: r.Key, Claim: text, Salt: claim.Hex(salt)})
	}
	for len(items) > 0 {
		n := min(len(items), 10)
		batch := items[:n]
		items = items[n:]
		out, hash, err := a.chain.Call(ctx, token.Reveal(batch))
		if err != nil {
			return err
		}
		for i, s := range token.Statuses(out) {
			if i < len(batch) {
				a.cfg.Logf("%s reveal %s: %s (tx %s)", a.cfg.ID, batch[i].Key, s, hash)
			}
		}
		a.noteTx(hash)
	}
	return nil
}

func (a *Attester) commit(ctx context.Context, rounds []token.Round) error {
	open := map[string]token.Round{}
	for _, r := range rounds {
		open[r.Key] = r
	}
	now := time.Now().Unix()
	var items []token.CommitItem
	var texts []*string
	add := func(key string, text *string) {
		if _, dup := a.committed[key]; dup {
			return
		}
		for _, it := range items {
			if it.Key == key {
				return
			}
		}
		h := claim.Hash(text)
		c := claim.Commitment(a.cfg.ID, key, h, claim.Salt(a.cfg.Secret, key))
		items = append(items, token.CommitItem{Key: key, Commitment: claim.Hex(c)})
		texts = append(texts, text)
	}

	// 1. Rounds others opened: answer with our own reading, or NONE.
	for _, r := range rounds {
		if r.HasCommitted(a.cfg.ID) || now > r.CommitDeadline {
			continue
		}
		text, err := a.build(ctx, r.Key)
		if errors.Is(err, errUnconfirmed) {
			add(r.Key, nil)
		} else if err == nil {
			add(r.Key, text)
		}
	}

	// 2. Link requests and repository proposals waiting on the contract.
	links, err := a.chain.PendingLinks(ctx)
	if err == nil {
		for _, l := range links {
			if _, isOpen := open[l.Key]; isOpen {
				continue
			}
			if text, err := a.build(ctx, l.Key); err == nil {
				add(l.Key, text)
			}
		}
	}
	props, err := a.chain.PendingRepos(ctx)
	if err == nil {
		for _, p := range props {
			key := claim.RepoKey(p.Key)
			if _, isOpen := open[key]; isOpen {
				continue
			}
			if text, err := a.build(ctx, key); err == nil {
				add(key, text)
			}
		}
	}

	// Answer what is already open before the slow search, so the commit
	// window is not missed while GitHub search is rate limited.
	if err := a.send(ctx, items, texts); err != nil {
		return err
	}
	items, texts = nil, nil

	// 3. New merges in enrolled repositories, newest first.
	if err := a.discover(ctx, open, add); err != nil {
		a.cfg.Logf("%s: discover: %v", a.cfg.ID, err)
	}
	return a.send(ctx, items, texts)
}

func (a *Attester) send(ctx context.Context, items []token.CommitItem, texts []*string) error {
	for len(items) > 0 {
		n := min(len(items), 40)
		batch, btexts := items[:n], texts[:n]
		items, texts = items[n:], texts[n:]
		out, hash, err := a.chain.Call(ctx, token.Commit(batch))
		if err != nil {
			return err
		}
		sts := token.Statuses(out)
		for i, s := range sts {
			if i >= len(batch) {
				break
			}
			if s == "ok" || s == "already committed" {
				a.committed[batch[i].Key] = btexts[i]
			} else if s == "already settled" {
				a.done[batch[i].Key] = true
			}
			label := "claim"
			if btexts[i] == nil {
				label = "NONE"
			}
			a.cfg.Logf("%s commit %s (%s): %s (tx %s)", a.cfg.ID, batch[i].Key, label, s, hash)
		}
		a.noteTx(hash)
		a.save()
	}
	return nil
}

func (a *Attester) discover(ctx context.Context, open map[string]token.Round, add func(string, *string)) error {
	repos, err := a.chain.Repos(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
	}
	if len(names) == 0 {
		return nil
	}
	maxAge := time.Duration(a.params.MaxMergeAgeSecs) * time.Second
	look := a.cfg.Lookback
	if look <= 0 || look > maxAge {
		look = maxAge - time.Hour
	}
	since := time.Now().Add(-look)
	if !a.since.IsZero() && a.since.After(since) {
		since = a.since
	}
	found, err := a.gh.SearchMergedMulti(ctx, names, since)
	if err != nil {
		return err
	}
	newest := since
	built := 0
	for _, m := range found {
		key := claim.MergeKey(m.Repo, m.Number)
		if a.cfg.Only != nil && !a.cfg.Only[key] {
			continue
		}
		if m.Author.IsBot() || a.done[key] {
			continue
		}
		if _, ok := a.committed[key]; ok {
			continue
		}
		if _, ok := open[key]; ok {
			continue // handled above
		}
		if built >= 25 {
			break // keep each tick short and inside the API budget
		}
		done, err := a.chain.IsDone(ctx, key)
		if err != nil {
			return err
		}
		if done {
			a.done[key] = true
			continue
		}
		text, err := a.build(ctx, key)
		if err != nil {
			if errors.Is(err, errSkip) || errors.Is(err, errUnconfirmed) {
				a.done[key] = true
				continue
			}
			return err
		}
		built++
		add(key, text)
		if m.MergedAt.After(newest) {
			newest = m.MergedAt
		}
	}
	// Overlap a little: search results can lag behind the merge.
	if built < 25 {
		a.since = newest.Add(-30 * time.Minute)
	}
	return nil
}

var (
	errUnconfirmed = errors.New("cannot confirm on GitHub")
	errSkip        = errors.New("not attested by policy")
)

// build reads GitHub and returns the canonical claim for key.
func (a *Attester) build(ctx context.Context, key string) (*string, error) {
	switch {
	case strings.HasPrefix(key, "merge:"):
		return a.buildMerge(ctx, key)
	case strings.HasPrefix(key, "link:"):
		return a.buildLink(ctx, key)
	case strings.HasPrefix(key, "repo:"):
		return a.buildRepo(ctx, strings.TrimPrefix(key, "repo:"))
	}
	return nil, errUnconfirmed
}

func (a *Attester) user(ctx context.Context, id int64) (*github.User, error) {
	if u, ok := a.users[id]; ok {
		return u, nil
	}
	u, err := a.gh.UserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	a.users[id] = u
	return u, nil
}

func (a *Attester) buildMerge(ctx context.Context, key string) (*string, error) {
	rest := strings.TrimPrefix(key, "merge:")
	i := strings.LastIndex(rest, "#")
	if i < 0 {
		return nil, errUnconfirmed
	}
	repo := rest[:i]
	var number int
	if _, err := fmt.Sscanf(rest[i+1:], "%d", &number); err != nil {
		return nil, errUnconfirmed
	}
	pr, err := a.gh.Pull(ctx, repo, number)
	if errors.Is(err, github.ErrNotFound) {
		return nil, errUnconfirmed
	}
	if err != nil {
		return nil, err
	}
	if !pr.Merged || pr.MergedAt == nil || !strings.EqualFold(pr.Base.Repo.FullName, repo) {
		return nil, errUnconfirmed
	}
	if pr.User.IsBot() {
		return nil, errSkip
	}
	author, err := a.user(ctx, pr.User.ID)
	if err != nil {
		return nil, err
	}
	apps, err := a.gh.Approvers(ctx, repo, number, pr.User.ID, *pr.MergedAt)
	if err != nil {
		return nil, err
	}
	m := claim.Merge{
		Repo: repo, Number: number, MergeSHA: pr.MergeCommitSHA, MergedAt: pr.MergedAt.Unix(),
		AuthorID: author.ID, AuthorLogin: author.Login, AuthorCreatedAt: author.CreatedAt.Unix(),
	}
	for _, u := range apps {
		m.Approvers = append(m.Approvers, claim.Approver{ID: u.ID, Login: u.Login})
	}
	if a.cfg.Liar {
		m.AuthorID, m.AuthorLogin = 1, "mallory"
	}
	text := m.Text()
	return &text, nil
}

func (a *Attester) buildLink(ctx context.Context, key string) (*string, error) {
	parts := strings.SplitN(strings.TrimPrefix(key, "link:"), ":", 2)
	if len(parts) != 2 {
		return nil, errUnconfirmed
	}
	var id int64
	if _, err := fmt.Sscanf(parts[0], "%d", &id); err != nil {
		return nil, errUnconfirmed
	}
	account := parts[1]
	u, err := a.gh.UserByID(ctx, id)
	if errors.Is(err, github.ErrNotFound) {
		return nil, errUnconfirmed
	}
	if err != nil {
		return nil, err
	}
	ok, err := a.gh.GistContains(ctx, u.Login, claim.LinkProof(account))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errUnconfirmed
	}
	text := claim.Link{GitHubID: u.ID, Login: u.Login, Account: account, GitHubCreatedAt: u.CreatedAt.Unix()}.Text()
	return &text, nil
}

func (a *Attester) buildRepo(ctx context.Context, repo string) (*string, error) {
	r, err := a.gh.Repository(ctx, repo)
	if errors.Is(err, github.ErrNotFound) {
		return nil, errUnconfirmed
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(r.FullName, repo) {
		return nil, errUnconfirmed // renamed or transferred
	}
	text := claim.Repo{Repo: repo, RepoID: r.ID, StarsOK: r.Stars >= a.params.MinRepoStars, CreatedAt: r.CreatedAt.Unix(), Fork: r.Fork}.Text()
	return &text, nil
}

func (a *Attester) sweep(ctx context.Context, rounds []token.Round) error {
	now := time.Now().Unix()
	var keys []string
	for _, r := range rounds {
		if r.HasCommitted(a.cfg.ID) && Sweepable(r, now) {
			keys = append(keys, r.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	keys = keys[:min(len(keys), 60)]
	out, hash, err := a.chain.Call(ctx, token.Sweep(keys))
	if err != nil {
		return err
	}
	for i, s := range token.Statuses(out) {
		if i < len(keys) && (s == "closed" || s == "expired" || s == "no round") {
			delete(a.committed, keys[i])
			// Only merges are final; a link or repository can be asked for again.
			a.done[keys[i]] = s == "closed" && strings.HasPrefix(keys[i], "merge:")
		}
	}
	a.cfg.Logf("%s swept %d rounds (tx %s)", a.cfg.ID, len(keys), hash)
	a.noteTx(hash)
	a.save()
	return nil
}

// keep calls the feast when it is due and burns escrow past its fuse.
func (a *Attester) keep(ctx context.Context) error {
	st, err := a.chain.Stats(ctx)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if now >= st.LastFeastAt+a.params.FeastPeriodSecs+5 {
		if _, hash, err := a.chain.Call(ctx, token.Feast()); err == nil {
			a.cfg.Logf("%s called the feast (tx %s)", a.cfg.ID, hash)
			a.noteTx(hash)
		} else {
			a.cfg.Logf("%s feast: %v", a.cfg.ID, err)
		}
	}
	es, err := a.chain.Escrows(ctx)
	if err != nil {
		return err
	}
	var stale []int64
	for _, e := range es {
		if now >= e.Since+a.params.EscrowTTLSecs+5 {
			stale = append(stale, e.GitHubID)
		}
	}
	for len(stale) > 0 {
		n := min(len(stale), 50)
		if _, hash, err := a.chain.Call(ctx, token.EatStaleEscrow(stale[:n])); err == nil {
			a.cfg.Logf("%s burned %d stale escrows (tx %s)", a.cfg.ID, n, hash)
			a.noteTx(hash)
		}
		stale = stale[n:]
	}
	return nil
}

func (a *Attester) noteTx(hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastTx = append(a.lastTx, hash)
	if len(a.lastTx) > 50 {
		a.lastTx = a.lastTx[len(a.lastTx)-50:]
	}
}

// RecentTx returns the attester's latest transaction hashes, oldest first.
func (a *Attester) RecentTx() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.lastTx...)
}
