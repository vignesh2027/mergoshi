// Package web serves the Mergoshi dashboard: live contract state and the full
// ledger of mints and burns, rendered on the server.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mergoshi/internal/token"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server renders pages from the contract.
type Server struct {
	Token *token.Client
	// Network is "testnet" or "local"; it decides explorer links.
	Network string
	// SimDir holds the simulation charts (sim/out).
	SimDir string
	// Extra adds links to the navigation (the fake GitHub in local mode).
	Extra []NavLink

	tpl  map[string]*template.Template
	mu   sync.Mutex
	snap *Snapshot
	at   time.Time
}

// NavLink is one extra navigation entry.
type NavLink struct{ Href, Label string }

// RepoRow is one enrolled repository with today's cap.
type RepoRow struct {
	Name     string
	Genesis  bool
	Stars    int64
	Today    int
	Cap      int
	CapFull  bool
	Enrolled time.Time
}

// Snapshot is everything the home page shows, read in one pass.
type Snapshot struct {
	Params     token.Params
	Stats      token.Stats
	Supply     token.U128
	GenesisAt  time.Time
	Attesters  []token.Attester
	Repos      []RepoRow
	Rounds     []token.Round
	Recent     []token.LogEntry
	LogLen     int64
	Escrows    []token.Escrow
	NextFeast  time.Time
	FeastReady bool
	Read       time.Time
}

func (s *Server) templates() error {
	funcs := template.FuncMap{
		"tok":     func(u token.U128) string { return u.Tokens(2) },
		"near":    func(u token.U128) string { return token.FormatUnits(u.Big(), 24, 3) },
		"when":    func(t int64) string { return time.Unix(t, 0).UTC().Format("2 Jan 15:04 UTC") },
		"ago":     ago,
		"until":   until,
		"short":   shortHash,
		"account": s.accountLink,
		"keylink": keyLink,
		"pct":     func(bps int) string { return strconv.FormatFloat(float64(bps)/100, 'f', -1, 64) + "%" },
		"days":    func(secs int64) string { return humanDuration(time.Duration(secs) * time.Second) },
		"kindclass": func(k string) string {
			switch k {
			case "merge", "link", "enroll":
				return "k-mint"
			case "feast", "escrow_burned", "offering", "exile", "exile_burn", "unregister":
				return "k-burn"
			case "slash", "rejected":
				return "k-bad"
			}
			return "k-quiet"
		},
		"isburn": func(e token.LogEntry) bool { return e.Burned.Big().Sign() > 0 },
		"ismint": func(e token.LogEntry) bool { return e.Minted.Big().Sign() > 0 },
		"add":    func(a, b int64) int64 { return a + b },
		"sub":    func(a, b int64) int64 { return a - b },
	}
	s.tpl = map[string]*template.Template{}
	for _, page := range []string{"home", "ledger", "params", "escrow", "sim", "join", "fake"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return err
		}
		s.tpl[page] = t
	}
	return nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	if err := s.templates(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /ledger", s.ledger)
	mux.HandleFunc("GET /params", s.params)
	mux.HandleFunc("GET /escrow", s.escrow)
	mux.HandleFunc("GET /sim", s.sim)
	mux.HandleFunc("GET /join", s.join)
	mux.HandleFunc("GET /sim/{file}", s.simFile)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	return mux, nil
}

// Load reads the contract, caching for a few seconds.
func (s *Server) Load(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap != nil && time.Since(s.at) < 6*time.Second {
		return s.snap, nil
	}
	c := s.Token
	sn := &Snapshot{Read: time.Now()}
	var err error
	if sn.Params, err = c.Params(ctx); err != nil {
		return nil, err
	}
	if sn.Stats, err = c.Stats(ctx); err != nil {
		return nil, err
	}
	if sn.Supply, err = c.TotalSupply(ctx); err != nil {
		return nil, err
	}
	g, err := c.GenesisAt(ctx)
	if err != nil {
		return nil, err
	}
	sn.GenesisAt = time.Unix(g, 0)
	if sn.Attesters, err = c.Attesters(ctx); err != nil {
		return nil, err
	}
	repos, err := c.Repos(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	for _, r := range repos {
		today, _ := c.RepoDayCount(ctx, r.Name, now/86400)
		cp, _ := c.RepoCap(ctx, r.Name, now)
		sn.Repos = append(sn.Repos, RepoRow{Name: r.Name, Genesis: r.Genesis, Stars: r.Stars, Today: today, Cap: cp, CapFull: today >= cp, Enrolled: time.Unix(r.EnrolledAt, 0)})
	}
	sort.Slice(sn.Repos, func(i, j int) bool { return sn.Repos[i].Name < sn.Repos[j].Name })
	if sn.Rounds, err = c.OpenRounds(ctx); err != nil {
		return nil, err
	}
	if sn.LogLen, err = c.LogLen(ctx); err != nil {
		return nil, err
	}
	from := max(sn.LogLen-12, 0)
	recent, err := c.Log(ctx, from, 12)
	if err != nil {
		return nil, err
	}
	for i := len(recent) - 1; i >= 0; i-- {
		sn.Recent = append(sn.Recent, recent[i])
	}
	if sn.Escrows, err = c.Escrows(ctx); err != nil {
		return nil, err
	}
	sort.Slice(sn.Escrows, func(i, j int) bool { return sn.Escrows[i].Amount.Big().Cmp(sn.Escrows[j].Amount.Big()) > 0 })
	sn.NextFeast = time.Unix(sn.Stats.LastFeastAt+sn.Params.FeastPeriodSecs, 0)
	sn.FeastReady = !time.Now().Before(sn.NextFeast)
	s.snap, s.at = sn, time.Now()
	return sn, nil
}

type page struct {
	Title   string
	Active  string
	Network string
	Token   string
	Extra   []NavLink
	S       *Snapshot
	Data    any
}

func (s *Server) render(w http.ResponseWriter, name, title string, sn *Snapshot, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	p := page{Title: title, Active: name, Network: s.Network, Token: s.Token.Contract, Extra: s.Extra, S: sn, Data: data}
	if err := s.tpl[name].Execute(w, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// Render draws a named page with the current snapshot; used by local mode.
func (s *Server) Render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	s.mu.Lock()
	s.at = time.Time{} // always fresh after a demo action
	s.mu.Unlock()
	sn, err := s.Load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, name, title, sn, data)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("dashboard: %v", err)
	http.Error(w, "could not read the contract: "+err.Error(), http.StatusBadGateway)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	sn, err := s.Load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "home", "Mergoshi", sn, nil)
}

// LedgerPage is one page of the ledger, newest first.
type LedgerPage struct {
	Entries   []token.LogEntry
	From, To  int64
	Newer     int64
	Older     int64
	HasNewer  bool
	HasOlder  bool
	Kind      string
	TotalBurn string
	TotalMint string
}

func (s *Server) ledger(w http.ResponseWriter, r *http.Request) {
	sn, err := s.Load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	const per = 50
	n := sn.LogLen
	end := n
	if v, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil && v >= 0 && v < n {
		end = v
	}
	from := max(end-per, 0)
	var entries []token.LogEntry
	for start := from; start < end; start += 200 {
		got, err := s.Token.Log(r.Context(), start, min(200, end-start))
		if err != nil {
			s.fail(w, err)
			return
		}
		entries = append(entries, got...)
	}
	kind := r.URL.Query().Get("kind")
	lp := LedgerPage{From: from, To: end, Kind: kind}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if kind == "mint" && e.Minted.Big().Sign() == 0 || kind == "burn" && e.Burned.Big().Sign() == 0 {
			continue
		}
		lp.Entries = append(lp.Entries, e)
	}
	lp.HasNewer, lp.Newer = end < n, min(end+per, n)
	lp.HasOlder, lp.Older = from > 0, from
	s.render(w, "ledger", "Ledger", sn, lp)
}

func (s *Server) params(w http.ResponseWriter, r *http.Request) {
	sn, err := s.Load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "params", "Rules", sn, nil)
}

func (s *Server) escrow(w http.ResponseWriter, r *http.Request) {
	sn, err := s.Load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "escrow", "Escrow", sn, nil)
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	sn, err := s.Load(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "join", "Claim your coins", sn, nil)
}

// SimRow is one scenario in the simulation table.
type SimRow struct {
	Name                                 string
	Supply365, SupplyEnd, Minted, Burned string
	Coverage, Farmer                     string
}

func (s *Server) sim(w http.ResponseWriter, r *http.Request) {
	s.render(w, "sim", "Simulation", nil, nil)
}

func (s *Server) simFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("file"))
	if !strings.HasSuffix(name, ".svg") || s.SimDir == "" {
		http.NotFound(w, r)
		return
	}
	raw, err := os.ReadFile(filepath.Join(s.SimDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "max-age=300")
	_, _ = w.Write(raw)
}

func (s *Server) accountLink(id string) template.HTML {
	label := template.HTMLEscapeString(id)
	if strings.HasPrefix(id, "github:") {
		return template.HTML(`<span class="gh" title="held in escrow until this GitHub account links">` + label + `</span>`)
	}
	if s.Network == "testnet" {
		return template.HTML(fmt.Sprintf(`<a href="https://testnet.nearblocks.io/address/%s" target="_blank" rel="noopener">%s</a>`, label, label))
	}
	return template.HTML(label)
}

func keyLink(key string) template.HTML {
	if rest, ok := strings.CutPrefix(key, "merge:"); ok {
		if i := strings.LastIndex(rest, "#"); i > 0 {
			u := fmt.Sprintf("https://github.com/%s/pull/%s", rest[:i], rest[i+1:])
			return template.HTML(fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener">%s</a>`, template.HTMLEscapeString(u), template.HTMLEscapeString(rest)))
		}
	}
	return template.HTML(template.HTMLEscapeString(key))
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:6] + ".." + h[len(h)-4:]
	}
	return h
}

func ago(t int64) string {
	d := time.Since(time.Unix(t, 0))
	if d < 0 {
		return "just now"
	}
	return humanDuration(d) + " ago"
}

func until(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	return "in " + humanDuration(d)
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// Percent of a/b as text, for template use.
func Percent(a, b *big.Int) string {
	if b.Sign() == 0 {
		return "0%"
	}
	f, _ := new(big.Rat).SetFrac(new(big.Int).Mul(a, big.NewInt(100)), b).Float64()
	return strconv.FormatFloat(f, 'f', 1, 64) + "%"
}
