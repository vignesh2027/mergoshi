package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestApproversTrustRule(t *testing.T) {
	merged := time.Date(2026, 11, 2, 21, 32, 19, 0, time.UTC)
	before := merged.Add(-time.Minute)
	type rv struct {
		id    int64
		login string
		state string
		assoc string
		at    time.Time
	}
	reviews := []rv{
		{1, "owner", "APPROVED", "OWNER", before},
		{2, "merger", "APPROVED", "CONTRIBUTOR", before},     // merged the PR: counts without a search
		{3, "regular", "APPROVED", "CONTRIBUTOR", before},    // 3 prior merges: counts
		{4, "typo-fixer", "APPROVED", "CONTRIBUTOR", before}, // 1 prior merge: does not count
		{5, "newcomer", "APPROVED", "FIRST_TIME_CONTRIBUTOR", before},
		{6, "drive-by", "APPROVED", "NONE", before},
		{7, "late", "APPROVED", "MEMBER", merged.Add(time.Minute)}, // after the merge
		{8, "author", "APPROVED", "MEMBER", before},                // the author
		{9, "ci[bot]", "APPROVED", "MEMBER", before},
		{10, "changed-mind", "APPROVED", "MEMBER", before.Add(-time.Hour)},
		{10, "changed-mind", "CHANGES_REQUESTED", "MEMBER", before},
	}
	prior := map[string]int{"regular": 3, "typo-fixer": 1}
	var searched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			var out []map[string]any
			for _, x := range reviews {
				out = append(out, map[string]any{
					"user": map[string]any{"id": x.id, "login": x.login, "type": "User"}, "state": x.state,
					"author_association": x.assoc, "submitted_at": x.at,
				})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			searched = append(searched, q)
			if !strings.Contains(q, "repo:o/r ") || !strings.Contains(q, "merged:<2026-11-02T21:32:19Z") {
				t.Errorf("unexpected query %q", q)
			}
			n := 0
			for login, c := range prior {
				if strings.Contains(q, "author:"+login+" ") {
					n = c
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": n, "items": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	c.SearchGap = 0

	got, err := c.Approvers(context.Background(), "o/r", 1, 8, 2, merged)
	if err != nil {
		t.Fatal(err)
	}
	var logins []string
	for _, u := range got {
		logins = append(logins, u.Login)
	}
	if want := "owner,merger,regular"; strings.Join(logins, ",") != want {
		t.Fatalf("approvers = %v, want %s", logins, want)
	}
	// Only the two CONTRIBUTORs that did not merge the PR cost a search.
	if len(searched) != 2 {
		t.Fatalf("searched %d times: %q", len(searched), searched)
	}
}
