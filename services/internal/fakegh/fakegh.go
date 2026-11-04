// Package fakegh is a small stand-in for the GitHub REST API used by the local
// sandbox mode and the tests. It serves the endpoints the attesters read and a
// page where a person can merge a pull request with one click.
package fakegh

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// User is a fake GitHub account.
type User struct {
	ID        int64     `json:"id"`
	Login     string    `json:"login"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

// Repo is a fake repository.
type Repo struct {
	ID        int64     `json:"id"`
	FullName  string    `json:"full_name"`
	Stars     int       `json:"stargazers_count"`
	CreatedAt time.Time `json:"created_at"`
	Fork      bool      `json:"fork"`
}

// PR is a merged pull request.
type PR struct {
	Repo      string
	Number    int
	Author    int64
	MergedAt  time.Time
	SHA       string
	Approvers []int64
}

type gist struct {
	ID      string
	Owner   int64
	Content string
}

// Store holds the fake GitHub's state. It is safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	users map[int64]*User
	repos map[string]*Repo
	prs   []*PR
	gists []*gist
	next  map[string]int
	base  string
}

// New returns an empty store. base is the public URL of the server, used in
// repository and raw gist links.
func New(base string) *Store {
	return &Store{users: map[int64]*User{}, repos: map[string]*Repo{}, next: map[string]int{}, base: strings.TrimRight(base, "/")}
}

// SetBase sets the server URL once it is known.
func (s *Store) SetBase(base string) {
	s.mu.Lock()
	s.base = strings.TrimRight(base, "/")
	s.mu.Unlock()
}

// AddUser adds an account.
func (s *Store) AddUser(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.Type == "" {
		u.Type = "User"
	}
	s.users[u.ID] = &u
}

// AddRepo adds a repository.
func (s *Store) AddRepo(r Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[strings.ToLower(r.FullName)] = &r
}

// Users returns accounts sorted by login.
func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []User
	for _, u := range s.users {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out
}

// Repos returns repository names sorted.
func (s *Store) Repos() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.repos {
		out = append(out, r.FullName)
	}
	sort.Strings(out)
	return out
}

// Merge records a pull request merged now and returns it.
func (s *Store) Merge(repo string, author int64, approvers []int64) (*PR, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[strings.ToLower(repo)]
	if !ok {
		return nil, fmt.Errorf("unknown repository %s", repo)
	}
	if _, ok := s.users[author]; !ok {
		return nil, fmt.Errorf("unknown author %d", author)
	}
	key := strings.ToLower(r.FullName)
	if s.next[key] == 0 {
		s.next[key] = 10_000
	}
	s.next[key]++
	var sha [20]byte
	_, _ = rand.Read(sha[:])
	var apps []int64
	for _, a := range approvers {
		if a != author {
			apps = append(apps, a)
		}
	}
	pr := &PR{Repo: r.FullName, Number: s.next[key], Author: author, MergedAt: time.Now().UTC().Truncate(time.Second), SHA: hex.EncodeToString(sha[:]), Approvers: apps}
	s.prs = append(s.prs, pr)
	return pr, nil
}

// Recent returns the newest merged pull requests first.
func (s *Store) Recent(n int) []PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PR
	for i := len(s.prs) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, *s.prs[i])
	}
	return out
}

// Login returns the login for an id.
func (s *Store) Login(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[id]; ok {
		return u.Login
	}
	return strconv.FormatInt(id, 10)
}

// PublishGist stores a public gist for the account.
func (s *Store) PublishGist(owner int64, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id [6]byte
	_, _ = rand.Read(id[:])
	s.gists = append(s.gists, &gist{ID: hex.EncodeToString(id[:]), Owner: owner, Content: content})
}

// API returns the REST API handler.
func (s *Store) API() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", s.search)
	mux.HandleFunc("GET /repos/{owner}/{name}", s.repo)
	mux.HandleFunc("GET /repos/{owner}/{name}/pulls/{n}", s.pull)
	mux.HandleFunc("GET /repos/{owner}/{name}/pulls/{n}/reviews", s.reviews)
	mux.HandleFunc("GET /user/{id}", s.userByID)
	mux.HandleFunc("GET /users/{login}", s.userByLogin)
	mux.HandleFunc("GET /users/{login}/gists", s.userGists)
	mux.HandleFunc("GET /raw/gist/{id}", s.rawGist)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"Not Found"}`))
}

// search supports the qualifiers the attesters use: repo:, is:pr, is:merged,
// merged:>=TIME and merged:FROM..TO.
func (s *Store) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	var repos []string
	var from, to time.Time
	to = time.Now().Add(time.Hour)
	for _, f := range strings.Fields(q) {
		switch {
		case strings.HasPrefix(f, "repo:"):
			repos = append(repos, strings.ToLower(strings.TrimPrefix(f, "repo:")))
		case strings.HasPrefix(f, "merged:>="):
			from, _ = time.Parse(time.RFC3339, strings.TrimPrefix(f, "merged:>="))
		case strings.HasPrefix(f, "merged:"):
			a, b, _ := strings.Cut(strings.TrimPrefix(f, "merged:"), "..")
			from, _ = time.Parse(time.RFC3339, a)
			to, _ = time.Parse(time.RFC3339, b)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type item struct {
		Number        int    `json:"number"`
		RepositoryURL string `json:"repository_url"`
		User          User   `json:"user"`
		PullRequest   struct {
			MergedAt time.Time `json:"merged_at"`
		} `json:"pull_request"`
	}
	var items []item
	for i := len(s.prs) - 1; i >= 0; i-- {
		p := s.prs[i]
		if p.MergedAt.Before(from) || p.MergedAt.After(to) {
			continue
		}
		match := len(repos) == 0
		for _, rp := range repos {
			if rp == strings.ToLower(p.Repo) {
				match = true
			}
		}
		if !match {
			continue
		}
		it := item{Number: p.Number, RepositoryURL: s.base + "/repos/" + p.Repo, User: *s.users[p.Author]}
		it.PullRequest.MergedAt = p.MergedAt
		items = append(items, it)
	}
	if items == nil {
		items = []item{}
	}
	writeJSON(w, map[string]any{"total_count": len(items), "items": items})
}

func (s *Store) find(owner, name, n string) *PR {
	num, _ := strconv.Atoi(n)
	full := strings.ToLower(owner + "/" + name)
	for _, p := range s.prs {
		if strings.ToLower(p.Repo) == full && p.Number == num {
			return p
		}
	}
	return nil
}

func (s *Store) repo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rp, ok := s.repos[strings.ToLower(r.PathValue("owner")+"/"+r.PathValue("name"))]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, rp)
}

func (s *Store) pull(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(r.PathValue("owner"), r.PathValue("name"), r.PathValue("n"))
	if p == nil {
		notFound(w)
		return
	}
	out := map[string]any{
		"number": p.Number, "merged": true, "merged_at": p.MergedAt, "merge_commit_sha": p.SHA,
		"user": s.users[p.Author],
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": p.Repo, "default_branch": "main"}},
	}
	writeJSON(w, out)
}

func (s *Store) reviews(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(r.PathValue("owner"), r.PathValue("name"), r.PathValue("n"))
	if p == nil {
		notFound(w)
		return
	}
	if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
		writeJSON(w, []any{})
		return
	}
	out := []map[string]any{}
	for i, a := range p.Approvers {
		out = append(out, map[string]any{"user": s.users[a], "state": "APPROVED", "author_association": "MEMBER", "submitted_at": p.MergedAt.Add(-time.Duration(10+i) * time.Minute)})
	}
	writeJSON(w, out)
}

func (s *Store) userByID(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, u)
}

func (s *Store) byLogin(login string) *User {
	for _, u := range s.users {
		if strings.EqualFold(u.Login, login) {
			return u
		}
	}
	return nil
}

func (s *Store) userByLogin(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byLogin(r.PathValue("login"))
	if u == nil {
		notFound(w)
		return
	}
	writeJSON(w, u)
}

func (s *Store) userGists(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byLogin(r.PathValue("login"))
	if u == nil {
		notFound(w)
		return
	}
	out := []map[string]any{}
	for i := len(s.gists) - 1; i >= 0; i-- {
		g := s.gists[i]
		if g.Owner != u.ID {
			continue
		}
		out = append(out, map[string]any{
			"id": g.ID, "owner": u,
			"files": map[string]any{"mergoshi.txt": map[string]any{"raw_url": s.base + "/raw/gist/" + url.PathEscape(g.ID), "size": len(g.Content)}},
		})
	}
	writeJSON(w, out)
}

func (s *Store) rawGist(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.gists {
		if g.ID == r.PathValue("id") {
			_, _ = w.Write([]byte(g.Content))
			return
		}
	}
	notFound(w)
}
