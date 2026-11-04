// Package github reads the public GitHub REST API. It needs no token; when
// GITHUB_TOKEN is set it is sent to raise the rate limit, nothing else.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is a small read-only GitHub client.
type Client struct {
	Base  string // https://api.github.com, or a local fake in sandbox mode
	Token string
	HTTP  *http.Client

	mu         sync.Mutex
	lastSearch time.Time
	// SearchGap is the minimum spacing between search calls. The search API
	// allows 10 calls a minute without a token and 30 with one.
	SearchGap time.Duration
}

// New returns a client for base. An empty base means api.github.com.
func New(base, token string) *Client {
	if base == "" {
		base = "https://api.github.com"
	}
	gap := 6500 * time.Millisecond
	if token != "" {
		gap = 2100 * time.Millisecond
	}
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}, SearchGap: gap}
}

// ErrNotFound is returned for a 404.
var ErrNotFound = errors.New("github: not found")

// RateLimitError is returned when the API says to wait longer than we are willing to.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "github rate limit reached, resets at " + e.Reset.UTC().Format(time.RFC3339)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.Base + path
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "merged-attester")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt < 2 {
				time.Sleep(time.Second)
				continue
			}
			return err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			if out == nil {
				return nil
			}
			return json.Unmarshal(raw, out)
		case resp.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
			reset := time.Now().Add(time.Minute)
			if s := resp.Header.Get("X-RateLimit-Reset"); s != "" {
				if v, err := strconv.ParseInt(s, 10, 64); err == nil {
					reset = time.Unix(v, 0)
				}
			}
			if s := resp.Header.Get("Retry-After"); s != "" {
				if v, err := strconv.Atoi(s); err == nil {
					reset = time.Now().Add(time.Duration(v) * time.Second)
				}
			}
			wait := time.Until(reset) + time.Second
			if wait > 90*time.Second || attempt >= 3 {
				return &RateLimitError{Reset: reset}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		case resp.StatusCode >= 500 && attempt < 2:
			time.Sleep(2 * time.Second)
		default:
			return fmt.Errorf("github %s: http %d: %.200s", path, resp.StatusCode, raw)
		}
	}
}

func (c *Client) waitSearch(ctx context.Context) error {
	c.mu.Lock()
	wait := c.SearchGap - time.Since(c.lastSearch)
	c.lastSearch = time.Now().Add(max(wait, 0))
	c.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// User is a GitHub account.
type User struct {
	ID        int64     `json:"id"`
	Login     string    `json:"login"`
	Type      string    `json:"type"` // "User", "Bot" or "Organization"
	CreatedAt time.Time `json:"created_at"`
}

// IsBot reports whether the account is an app or bot account.
func (u User) IsBot() bool {
	return u.Type == "Bot" || strings.HasSuffix(u.Login, "[bot]")
}

// MergeSummary is one merged pull request as the search API lists it.
type MergeSummary struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	MergedAt time.Time `json:"merged_at"`
	Author   User      `json:"author"`
}

// SearchMerged lists pull requests merged into repo in [from, to] (dates in UTC).
// The search API returns at most 1000 results per query; callers keep windows small.
func (c *Client) SearchMerged(ctx context.Context, repo string, from, to time.Time) ([]MergeSummary, error) {
	q := fmt.Sprintf("repo:%s is:pr is:merged merged:%s..%s", repo, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	var out []MergeSummary
	for page := 1; page <= 10; page++ {
		if err := c.waitSearch(ctx); err != nil {
			return nil, err
		}
		var res struct {
			Total int `json:"total_count"`
			Items []struct {
				Number      int  `json:"number"`
				User        User `json:"user"`
				PullRequest struct {
					MergedAt *time.Time `json:"merged_at"`
				} `json:"pull_request"`
			} `json:"items"`
		}
		path := "/search/issues?per_page=100&sort=created&order=asc&page=" + strconv.Itoa(page) + "&q=" + url.QueryEscape(q)
		if err := c.get(ctx, path, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			if it.PullRequest.MergedAt == nil {
				continue
			}
			out = append(out, MergeSummary{Repo: repo, Number: it.Number, MergedAt: it.PullRequest.MergedAt.UTC(), Author: it.User})
		}
		if len(res.Items) < 100 || page*100 >= res.Total {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MergedAt.Before(out[j].MergedAt) })
	return out, nil
}

// PullRequest is the merge data an attester signs.
type PullRequest struct {
	Number         int        `json:"number"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	User           User       `json:"user"`
	Base           struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repo"`
	} `json:"base"`
}

// Pull reads one pull request.
func (c *Client) Pull(ctx context.Context, repo string, number int) (*PullRequest, error) {
	var pr PullRequest
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// Approvers returns the accounts whose latest review on the pull request is an
// approval, sorted by account id. The author is never counted.
func (c *Client) Approvers(ctx context.Context, repo string, number int, authorID int64) ([]User, error) {
	type review struct {
		User        User      `json:"user"`
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	latest := map[int64]review{}
	for page := 1; page <= 5; page++ {
		var rs []review
		if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100&page=%d", repo, number, page), &rs); err != nil {
			return nil, err
		}
		for _, r := range rs {
			// Comments do not change an earlier approval or rejection.
			if r.State == "COMMENTED" || r.State == "PENDING" {
				continue
			}
			if prev, ok := latest[r.User.ID]; !ok || !r.SubmittedAt.Before(prev.SubmittedAt) {
				latest[r.User.ID] = r
			}
		}
		if len(rs) < 100 {
			break
		}
	}
	var out []User
	for id, r := range latest {
		if id != authorID && r.State == "APPROVED" && !r.User.IsBot() {
			out = append(out, r.User)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Repo is the repository data used for enrollment.
type Repo struct {
	ID        int64     `json:"id"`
	FullName  string    `json:"full_name"`
	Stars     int       `json:"stargazers_count"`
	CreatedAt time.Time `json:"created_at"`
	Fork      bool      `json:"fork"`
	Archived  bool      `json:"archived"`
}

// Repository reads one repository.
func (c *Client) Repository(ctx context.Context, repo string) (*Repo, error) {
	var r Repo
	if err := c.get(ctx, "/repos/"+repo, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// UserByID reads an account by its numeric id, which never changes on rename.
func (c *Client) UserByID(ctx context.Context, id int64) (*User, error) {
	var u User
	if err := c.get(ctx, "/user/"+strconv.FormatInt(id, 10), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// UserByLogin reads an account by login.
func (c *Client) UserByLogin(ctx context.Context, login string) (*User, error) {
	var u User
	if err := c.get(ctx, "/users/"+url.PathEscape(login), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// GistContains reports whether any public gist owned by login has a file whose
// content contains needle. Only the newest 30 gists are checked.
func (c *Client) GistContains(ctx context.Context, login, needle string) (bool, error) {
	var gists []struct {
		Owner User `json:"owner"`
		Files map[string]struct {
			RawURL string `json:"raw_url"`
			Size   int    `json:"size"`
		} `json:"files"`
	}
	if err := c.get(ctx, "/users/"+url.PathEscape(login)+"/gists?per_page=30", &gists); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, g := range gists {
		if !strings.EqualFold(g.Owner.Login, login) {
			continue
		}
		for _, f := range g.Files {
			if f.Size > 4096 || f.RawURL == "" {
				continue
			}
			body, err := c.raw(ctx, f.RawURL)
			if err != nil {
				return false, err
			}
			if strings.Contains(body, needle) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (c *Client) raw(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "merged-attester")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gist file: http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return string(b), err
}

// SearchOwnerMerges lists every merged pull request authored by login, in any repository.
func (c *Client) SearchOwnerMerges(ctx context.Context, login string) ([]MergeSummary, error) {
	var out []MergeSummary
	q := "author:" + login + " is:pr is:merged"
	for page := 1; page <= 10; page++ {
		if err := c.waitSearch(ctx); err != nil {
			return nil, err
		}
		var res struct {
			Total int `json:"total_count"`
			Items []struct {
				Number        int    `json:"number"`
				RepositoryURL string `json:"repository_url"`
				User          User   `json:"user"`
				PullRequest   struct {
					MergedAt *time.Time `json:"merged_at"`
				} `json:"pull_request"`
			} `json:"items"`
		}
		if err := c.get(ctx, "/search/issues?per_page=100&page="+strconv.Itoa(page)+"&q="+url.QueryEscape(q), &res); err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			if it.PullRequest.MergedAt == nil {
				continue
			}
			_, repo, _ := strings.Cut(it.RepositoryURL, "/repos/")
			out = append(out, MergeSummary{Repo: repo, Number: it.Number, MergedAt: it.PullRequest.MergedAt.UTC(), Author: it.User})
		}
		if len(res.Items) < 100 || page*100 >= res.Total {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MergedAt.Before(out[j].MergedAt) })
	return out, nil
}
