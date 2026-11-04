// Package token is a typed client for the Mergoshi contract: views for the
// dashboard and attesters, and action builders for transactions.
package token

import (
	"context"
	"encoding/json"
	"math/big"
	"strconv"

	"mergoshi/internal/near"
)

// Client reads the contract over RPC.
type Client struct {
	RPC      *near.Client
	Contract string
}

// U128 is a decimal string amount as NEAR JSON encodes u128.
type U128 string

// Big returns the amount as a big integer (zero if malformed).
func (u U128) Big() *big.Int {
	v, ok := new(big.Int).SetString(string(u), 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

// Tokens formats an 18-decimal amount with up to `places` decimals.
func (u U128) Tokens(places int) string { return FormatUnits(u.Big(), 18, places) }

// FormatUnits renders v / 10^decimals with thousands separators.
func FormatUnits(v *big.Int, decimals, places int) string {
	neg := v.Sign() < 0
	a := new(big.Int).Abs(v)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, frac := new(big.Int).QuoRem(a, scale, new(big.Int))
	ws := whole.String()
	var out []byte
	for i := range ws {
		if i > 0 && (len(ws)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, ws[i])
	}
	s := string(out)
	if places > 0 && frac.Sign() != 0 {
		fs := frac.String()
		for len(fs) < decimals {
			fs = "0" + fs
		}
		fs = fs[:places]
		for len(fs) > 0 && fs[len(fs)-1] == '0' {
			fs = fs[:len(fs)-1]
		}
		if fs != "" {
			s += "." + fs
		}
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Params mirrors the contract's launch parameters.
type Params struct {
	Attesters         []string `json:"attesters"`
	Quorum            int      `json:"quorum"`
	Bond              U128     `json:"bond"`
	CommitWindowSecs  int64    `json:"commit_window_secs"`
	RevealWindowSecs  int64    `json:"reveal_window_secs"`
	ExitDelaySecs     int64    `json:"exit_delay_secs"`
	SlashBps          int      `json:"slash_bps"`
	BaseReward        U128     `json:"base_reward"`
	HalvingEvery      int64    `json:"halving_every"`
	AuthorBps         int      `json:"author_bps"`
	ReviewBps         int      `json:"review_bps"`
	AttesterBps       int      `json:"attester_bps"`
	FeastBps          int      `json:"feast_bps"`
	MaxPaidReviewers  int      `json:"max_paid_reviewers"`
	StreakStepBps     int      `json:"streak_step_bps"`
	StreakMaxBonusBps int      `json:"streak_max_bonus_bps"`
	RepoDailyCap      int      `json:"repo_daily_cap"`
	RepoFloor         int      `json:"repo_floor"`
	AuthorsPerSlot    int      `json:"authors_per_slot"`
	AuthorDailyCap    int      `json:"author_daily_cap"`
	MaxMergeAgeSecs   int64    `json:"max_merge_age_secs"`
	MinGitHubAgeDays  int      `json:"min_github_age_days"`
	MinRepoStars      int      `json:"min_repo_stars"`
	MinRepoAgeDays    int      `json:"min_repo_age_days"`
	RepoEnrollBurn    U128     `json:"repo_enroll_burn"`
	ExileMin          U128     `json:"exile_min"`
	FeastPeriodSecs   int64    `json:"feast_period_secs"`
	FeastBurnBps      int      `json:"feast_burn_bps"`
	FeastMaxGuests    int      `json:"feast_max_guests"`
	EscrowTTLSecs     int64    `json:"escrow_ttl_secs"`
	RequestDeposit    U128     `json:"request_deposit"`
	GenesisRepos      []string `json:"genesis_repos"`
}

type Stats struct {
	MergesMinted      int64 `json:"merges_minted"`
	MergesCapped      int64 `json:"merges_capped"`
	MergesStale       int64 `json:"merges_stale"`
	MergesYoung       int64 `json:"merges_young"`
	Exiles            int64 `json:"exiles"`
	RoundsRejected    int64 `json:"rounds_rejected"`
	RoundsExpired     int64 `json:"rounds_expired"`
	Links             int64 `json:"links"`
	Repos             int64 `json:"repos"`
	TotalMinted       U128  `json:"total_minted"`
	TotalBurned       U128  `json:"total_burned"`
	Withheld          U128  `json:"withheld"`
	EscrowOutstanding U128  `json:"escrow_outstanding"`
	FeastPot          U128  `json:"feast_pot"`
	Feasts            int64 `json:"feasts"`
	LastFeastAt       int64 `json:"last_feast_at"`
	SlashedNEAR       U128  `json:"slashed_near"`
}

type Attester struct {
	ID      string
	Bond    U128   `json:"bond"`
	Active  bool   `json:"active"`
	ExitAt  *int64 `json:"exit_at"`
	Settled int64  `json:"settled"`
	Slashes int64  `json:"slashes"`
	Slashed U128   `json:"slashed"`
	Earned  U128   `json:"earned"`
}

type RepoInfo struct {
	Name       string
	EnrolledAt int64 `json:"enrolled_at"`
	RepoID     int64 `json:"repo_id"`
	Genesis    bool  `json:"genesis"`
}

type Request struct {
	Key   string
	By    string `json:"by"`
	At    int64  `json:"at"`
	Login string `json:"login"`
}

type Round struct {
	Key            string      `json:"key"`
	OpenedAt       int64       `json:"opened_at"`
	CommitDeadline int64       `json:"commit_deadline"`
	RevealDeadline int64       `json:"reveal_deadline"`
	Committed      []string    `json:"committed"`
	Revealed       [][2]string `json:"revealed"`
	Outcome        *string     `json:"outcome"`
	Result         string      `json:"result"`
}

// HasCommitted reports whether attester committed in this round.
func (r Round) HasCommitted(attester string) bool {
	for _, a := range r.Committed {
		if a == attester {
			return true
		}
	}
	return false
}

// HasRevealed reports whether attester revealed in this round.
func (r Round) HasRevealed(attester string) bool {
	for _, a := range r.Revealed {
		if a[0] == attester {
			return true
		}
	}
	return false
}

type Part struct {
	Role   string `json:"role"`
	To     string `json:"to"`
	Amount U128   `json:"amount"`
}

type LogEntry struct {
	Seq    int64  `json:"seq"`
	At     int64  `json:"at"`
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Minted U128   `json:"minted"`
	Burned U128   `json:"burned"`
	Parts  []Part `json:"parts"`
	Note   string `json:"note"`
}

type Escrow struct {
	GitHubID int64
	Login    string `json:"login"`
	Amount   U128   `json:"amount"`
	Since    int64  `json:"since"`
}

func (c *Client) view(ctx context.Context, method string, args any, out any) error {
	return c.RPC.View(ctx, c.Contract, method, args, out)
}

func (c *Client) Params(ctx context.Context) (Params, error) {
	var p Params
	return p, c.view(ctx, "params", nil, &p)
}

func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	return s, c.view(ctx, "stats", nil, &s)
}

func (c *Client) TotalSupply(ctx context.Context) (U128, error) {
	var s U128
	return s, c.view(ctx, "ft_total_supply", nil, &s)
}

func (c *Client) BalanceOf(ctx context.Context, account string) (U128, error) {
	var s U128
	return s, c.view(ctx, "ft_balance_of", map[string]string{"account_id": account}, &s)
}

func (c *Client) GenesisAt(ctx context.Context) (int64, error) {
	var t int64
	return t, c.view(ctx, "genesis_at", nil, &t)
}

func (c *Client) Attesters(ctx context.Context) ([]Attester, error) {
	var raw [][2]json.RawMessage
	if err := c.view(ctx, "attesters", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Attester, 0, len(raw))
	for _, r := range raw {
		var a Attester
		if err := json.Unmarshal(r[0], &a.ID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(r[1], &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (c *Client) Repos(ctx context.Context) ([]RepoInfo, error) {
	var raw [][2]json.RawMessage
	if err := c.view(ctx, "repos", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]RepoInfo, 0, len(raw))
	for _, r := range raw {
		var ri RepoInfo
		if err := json.Unmarshal(r[0], &ri.Name); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(r[1], &ri); err != nil {
			return nil, err
		}
		out = append(out, ri)
	}
	return out, nil
}

func (c *Client) requests(ctx context.Context, method string) ([]Request, error) {
	var raw [][2]json.RawMessage
	if err := c.view(ctx, method, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(raw))
	for _, r := range raw {
		var q Request
		if err := json.Unmarshal(r[0], &q.Key); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(r[1], &q); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

// PendingLinks returns link requests keyed by their round key.
func (c *Client) PendingLinks(ctx context.Context) ([]Request, error) {
	return c.requests(ctx, "pending_links")
}

// PendingRepos returns repository proposals keyed by repository name.
func (c *Client) PendingRepos(ctx context.Context) ([]Request, error) {
	return c.requests(ctx, "pending_repos")
}

// OpenRounds pages through every open round.
func (c *Client) OpenRounds(ctx context.Context) ([]Round, error) {
	var all []Round
	for from := 0; ; from += 100 {
		var page []Round
		if err := c.view(ctx, "open_rounds", map[string]int{"from_index": from, "limit": 100}, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 100 {
			return all, nil
		}
	}
}

func (c *Client) IsDone(ctx context.Context, key string) (bool, error) {
	var b bool
	return b, c.view(ctx, "is_done", map[string]string{"key": key}, &b)
}

func (c *Client) RepoCap(ctx context.Context, repo string, at int64) (int, error) {
	var n int
	return n, c.view(ctx, "repo_cap", map[string]any{"repo": repo, "at": at}, &n)
}

func (c *Client) RepoDayCount(ctx context.Context, repo string, day int64) (int, error) {
	var n int
	return n, c.view(ctx, "repo_day_count", map[string]any{"repo": repo, "day": day}, &n)
}

func (c *Client) ExileCost(ctx context.Context, repo string) (U128, error) {
	var u U128
	return u, c.view(ctx, "exile_cost", map[string]string{"repo": repo}, &u)
}

func (c *Client) LogLen(ctx context.Context) (int64, error) {
	var n int64
	return n, c.view(ctx, "log_len", nil, &n)
}

func (c *Client) Log(ctx context.Context, from, limit int64) ([]LogEntry, error) {
	var out []LogEntry
	return out, c.view(ctx, "log", map[string]int64{"from_index": from, "limit": limit}, &out)
}

func (c *Client) Escrows(ctx context.Context) ([]Escrow, error) {
	var all []Escrow
	for from := 0; ; from += 200 {
		var raw [][2]json.RawMessage
		if err := c.view(ctx, "escrows", map[string]int{"from_index": from, "limit": 200}, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			var e Escrow
			if err := json.Unmarshal(r[0], &e.GitHubID); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(r[1], &e); err != nil {
				return nil, err
			}
			all = append(all, e)
		}
		if len(raw) < 200 {
			return all, nil
		}
	}
}

type Link struct {
	Account  string `json:"account"`
	Login    string `json:"login"`
	LinkedAt int64  `json:"linked_at"`
}

func (c *Client) LinkOf(ctx context.Context, githubID int64) (*Link, error) {
	var l *Link
	return l, c.view(ctx, "link_of", map[string]int64{"github_id": githubID}, &l)
}

func (c *Client) GitHubOf(ctx context.Context, account string) (*int64, error) {
	var id *int64
	return id, c.view(ctx, "github_of", map[string]string{"account_id": account}, &id)
}

func (c *Client) StreakOf(ctx context.Context, githubID int64) (*[2]int64, error) {
	var s *[2]int64
	return s, c.view(ctx, "streak_of", map[string]int64{"github_id": githubID}, &s)
}

func (c *Client) Guests(ctx context.Context) ([][2]int64, error) {
	var g [][2]int64
	return g, c.view(ctx, "guests", nil, &g)
}

// ---- actions --------------------------------------------------------------

// CommitItem and RevealItem match the contract's batch arguments.
type CommitItem struct {
	Key        string `json:"key"`
	Commitment string `json:"commitment"`
}

type RevealItem struct {
	Key   string  `json:"key"`
	Claim *string `json:"claim"`
	Salt  string  `json:"salt"`
}

func New(p Params) near.FunctionCall {
	return near.CallArgs("new", map[string]any{"params": p}, 100)
}

func Bond(amount *big.Int) near.FunctionCall {
	fc := near.CallArgs("bond", map[string]any{}, 30)
	fc.Deposit = amount
	return fc
}

// Prepaid gas is reserved from the signer's balance up front (0.1 NEAR per
// 100 Tgas on testnet), so batches ask for what they need, not the maximum.
func gasFor(base, per uint64, n int) uint64 { return min(base+per*uint64(n), 300) }

func Commit(items []CommitItem) near.FunctionCall {
	return near.CallArgs("commit", map[string]any{"items": items}, gasFor(15, 5, len(items)))
}

func Reveal(items []RevealItem) near.FunctionCall {
	return near.CallArgs("reveal", map[string]any{"items": items}, gasFor(30, 27, len(items)))
}

func Sweep(keys []string) near.FunctionCall {
	return near.CallArgs("sweep", map[string]any{"keys": keys}, gasFor(10, 4, len(keys)))
}

func Feast() near.FunctionCall { return near.CallArgs("feast", map[string]any{}, 300) }

func EatStaleEscrow(ids []int64) near.FunctionCall {
	return near.CallArgs("eat_stale_escrow", map[string]any{"github_ids": ids}, 150)
}

func RequestLink(githubID int64, login string, deposit *big.Int) near.FunctionCall {
	fc := near.CallArgs("request_link", map[string]any{"github_id": githubID, "login": login}, 30)
	fc.Deposit = deposit
	return fc
}

func ProposeRepo(repo string, deposit *big.Int) near.FunctionCall {
	fc := near.CallArgs("propose_repo", map[string]any{"repo": repo}, 30)
	fc.Deposit = deposit
	return fc
}

func Exile(repo string, amount *big.Int) near.FunctionCall {
	fc := near.CallArgs("exile", map[string]any{"repo": repo, "amount": amount.String()}, 30)
	fc.Deposit = big.NewInt(1)
	return fc
}

func StorageDeposit(account string, deposit *big.Int) near.FunctionCall {
	fc := near.CallArgs("storage_deposit", map[string]any{"account_id": account}, 30)
	fc.Deposit = deposit
	return fc
}

func FtTransfer(to string, amount *big.Int, memo string) near.FunctionCall {
	fc := near.CallArgs("ft_transfer", map[string]any{"receiver_id": to, "amount": amount.String(), "memo": memo}, 30)
	fc.Deposit = big.NewInt(1)
	return fc
}

// Statuses decodes the per-item status list returned by commit, reveal and sweep.
func Statuses(value []byte) []string {
	var out []string
	_ = json.Unmarshal(value, &out)
	return out
}

// Itoa is strconv.FormatInt for int64, kept here to avoid an import in callers.
func Itoa(v int64) string { return strconv.FormatInt(v, 10) }
