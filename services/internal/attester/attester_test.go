package attester

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mergoshi/internal/claim"
	"mergoshi/internal/fakegh"
	"mergoshi/internal/github"
	"mergoshi/internal/near"
	"mergoshi/internal/token"
)

type call struct {
	method string
	args   map[string]json.RawMessage
}

type fakeChain struct {
	params  token.Params
	repos   []token.RepoInfo
	rounds  []token.Round
	links   []token.Request
	calls   []call
	results map[string][]string
}

func (f *fakeChain) Params(context.Context) (token.Params, error)      { return f.params, nil }
func (f *fakeChain) Repos(context.Context) ([]token.RepoInfo, error)   { return f.repos, nil }
func (f *fakeChain) OpenRounds(context.Context) ([]token.Round, error) { return f.rounds, nil }
func (f *fakeChain) PendingLinks(context.Context) ([]token.Request, error) {
	return f.links, nil
}
func (f *fakeChain) PendingRepos(context.Context) ([]token.Request, error) { return nil, nil }
func (f *fakeChain) IsDone(context.Context, string) (bool, error)          { return false, nil }
func (f *fakeChain) Attesters(context.Context) ([]token.Attester, error) {
	return []token.Attester{{ID: "a1", Active: true}, {ID: "a2", Active: true}, {ID: "a3", Active: true}}, nil
}
func (f *fakeChain) Stats(context.Context) (token.Stats, error) {
	return token.Stats{LastFeastAt: time.Now().Unix()}, nil
}
func (f *fakeChain) Escrows(context.Context) ([]token.Escrow, error) { return nil, nil }
func (f *fakeChain) Call(_ context.Context, fc near.FunctionCall) ([]byte, string, error) {
	var args map[string]json.RawMessage
	_ = json.Unmarshal(fc.Args, &args)
	f.calls = append(f.calls, call{fc.Method, args})
	var items []json.RawMessage
	_ = json.Unmarshal(args["items"], &items)
	st := make([]string, len(items))
	for i := range st {
		st[i] = "ok"
	}
	out, _ := json.Marshal(st)
	return out, "tx" + fc.Method, nil
}

func setup(t *testing.T) (*fakegh.Store, *fakeChain, *Attester) {
	t.Helper()
	gh := fakegh.New("")
	srv := httptest.NewServer(gh.API())
	t.Cleanup(srv.Close)
	gh.SetBase(srv.URL)
	old := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	gh.AddUser(fakegh.User{ID: 101, Login: "ada", CreatedAt: old})
	gh.AddUser(fakegh.User{ID: 7, Login: "lin", CreatedAt: old})
	gh.AddRepo(fakegh.Repo{ID: 1, FullName: "acme/rocket", Stars: 900, CreatedAt: old})
	fc := &fakeChain{
		params: token.Params{MaxMergeAgeSecs: 7 * 86400, FeastPeriodSecs: 86400, EscrowTTLSecs: 86400},
		repos:  []token.RepoInfo{{Name: "acme/rocket"}},
	}
	client := github.New(srv.URL, "")
	client.SearchGap = 0
	a := New(Config{ID: "a1", Secret: []byte("s"), Logf: t.Logf}, fc, client)
	return gh, fc, a
}

func items(t *testing.T, c call) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(c.args["items"], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCommitsThenRevealsAMerge(t *testing.T) {
	gh, fc, a := setup(t)
	pr, err := gh.Merge("acme/rocket", 101, []int64{7})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) != 1 || fc.calls[0].method != "commit" {
		t.Fatalf("calls %+v", fc.calls)
	}
	key := claim.MergeKey("acme/rocket", pr.Number)
	want := claim.Merge{Repo: "acme/rocket", Number: pr.Number, MergeSHA: pr.SHA, MergedAt: pr.MergedAt.Unix(),
		AuthorID: 101, AuthorLogin: "ada", AuthorCreatedAt: time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
		Approvers: []claim.Approver{{ID: 7, Login: "lin"}}}.Text()
	h := claim.Hash(&want)
	cm := claim.Hex(claim.Commitment("a1", key, h, claim.Salt([]byte("s"), key)))
	got := items(t, fc.calls[0])
	if got[0]["key"] != key || got[0]["commitment"] != cm {
		t.Fatalf("commit item %v, want %s %s", got[0], key, cm)
	}

	// Everyone committed: the attester reveals the same text at once.
	now := time.Now().Unix()
	fc.rounds = []token.Round{{Key: key, CommitDeadline: now + 60, RevealDeadline: now + 120, Committed: []string{"a1", "a2", "a3"}}}
	fc.calls = nil
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) == 0 || fc.calls[0].method != "reveal" {
		t.Fatalf("calls %+v", fc.calls)
	}
	rev := items(t, fc.calls[0])
	if rev[0]["claim"] != want || !strings.HasPrefix(rev[0]["claim"].(string), "merge\nacme/rocket\n") {
		t.Fatalf("reveal %v", rev[0])
	}
}

func TestVotesNoneOnAMergeThatDidNotHappen(t *testing.T) {
	_, fc, a := setup(t)
	now := time.Now().Unix()
	fc.rounds = []token.Round{{Key: "merge:acme/rocket#424242", CommitDeadline: now + 60, RevealDeadline: now + 120, Committed: []string{"a2"}}}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) != 1 {
		t.Fatalf("calls %+v", fc.calls)
	}
	key := "merge:acme/rocket#424242"
	none := claim.Hex(claim.Commitment("a1", key, claim.Hash(nil), claim.Salt([]byte("s"), key)))
	if items(t, fc.calls[0])[0]["commitment"] != none {
		t.Fatal("expected a NONE commitment")
	}
}

func TestLinksOnlyWithAGistProof(t *testing.T) {
	gh, fc, a := setup(t)
	fc.links = []token.Request{{Key: "link:101:ada.near", By: "ada.near"}}
	ctx := context.Background()
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) != 0 {
		t.Fatalf("committed without proof: %+v", fc.calls)
	}
	gh.PublishGist(101, "hello\n"+claim.LinkProof("ada.near")+"\n")
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) != 1 || items(t, fc.calls[0])[0]["key"] != "link:101:ada.near" {
		t.Fatalf("calls %+v", fc.calls)
	}
}

func TestReadyAndSweepable(t *testing.T) {
	r := token.Round{CommitDeadline: 100, RevealDeadline: 200, Committed: []string{"a1"}}
	if RevealReady(r, "a1", 3, 50) || !RevealReady(r, "a1", 3, 101) || RevealReady(r, "a2", 3, 150) || RevealReady(r, "a1", 3, 201) {
		t.Fatal("reveal readiness")
	}
	r.Committed = []string{"a1", "a2", "a3"}
	if !RevealReady(r, "a1", 3, 50) {
		t.Fatal("everyone committed: reveal early")
	}
	out := "x"
	if Sweepable(r, 150) || !Sweepable(r, 201) {
		t.Fatal("unsettled sweep")
	}
	r.Outcome = &out
	r.Revealed = [][2]string{{"a1", ""}, {"a2", ""}, {"a3", ""}}
	if !Sweepable(r, 150) {
		t.Fatal("settled and fully revealed is sweepable")
	}
}
