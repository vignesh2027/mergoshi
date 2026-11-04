package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"mergoshi/internal/attester"
	"mergoshi/internal/claim"
	"mergoshi/internal/fakegh"
	"mergoshi/internal/near"
	"mergoshi/internal/token"
	"mergoshi/internal/web"
)

// ownerID is the real GitHub id of vignesh2027; the other people in the local
// sandbox are made up.
const ownerID = 207180873

func seedFake(store *fakegh.Store, snapshotPath string) {
	old := time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)
	store.AddUser(fakegh.User{ID: ownerID, Login: "vignesh2027", CreatedAt: time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)})
	for i, l := range []string{"ada-lovecode", "lin-reviews", "omar-ships", "priya-patches", "sam-maintains"} {
		store.AddUser(fakegh.User{ID: int64(1001 + i), Login: l, CreatedAt: old.AddDate(0, i, 0)})
	}
	store.AddUser(fakegh.User{ID: 1099, Login: "fresh-face", CreatedAt: time.Now().AddDate(0, 0, -20)})
	added := false
	if raw, err := os.ReadFile(snapshotPath); err == nil {
		var snap struct {
			Repos []struct {
				Repo      string    `json:"repo"`
				ID        int64     `json:"id"`
				Stars     int       `json:"stars"`
				CreatedAt time.Time `json:"created_at"`
			} `json:"repos"`
		}
		if json.Unmarshal(raw, &snap) == nil {
			for _, r := range snap.Repos {
				store.AddRepo(fakegh.Repo{ID: r.ID, FullName: strings.ToLower(r.Repo), Stars: r.Stars, CreatedAt: r.CreatedAt})
				added = true
			}
		}
	}
	if !added {
		for i, r := range DefaultRepos {
			store.AddRepo(fakegh.Repo{ID: int64(i + 1), FullName: strings.ToLower(r), Stars: 1000, CreatedAt: old})
		}
	}
	store.AddRepo(fakegh.Repo{ID: 777, FullName: "community/fresh-lib", Stars: 450, CreatedAt: time.Now().AddDate(-2, 0, 0)})
}

type fakePage struct {
	Repos       []string
	Users       []fakegh.User
	Recent      []fakegh.PR
	Contributor string
	Flash       string
	store       *fakegh.Store
}

func (f fakePage) Young(u fakegh.User) bool { return time.Since(u.CreatedAt) < 90*24*time.Hour }
func (f fakePage) Login(id int64) string    { return f.store.Login(id) }

// demoRoutes adds the fake GitHub API under /gh and the demo page under /fake.
func (a *app) demoRoutes(ctx context.Context, store *fakegh.Store, contributor *near.Account) func(*http.ServeMux, *web.Server) []web.NavLink {
	return func(mux *http.ServeMux, s *web.Server) []web.NavLink {
		mux.Handle("/gh/", http.StripPrefix("/gh", store.API()))
		back := func(w http.ResponseWriter, r *http.Request, msg string) {
			http.Redirect(w, r, "/fake?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		}
		mux.HandleFunc("GET /fake", func(w http.ResponseWriter, r *http.Request) {
			var repos []string
			for _, rp := range store.Repos() {
				if rp != "community/fresh-lib" {
					repos = append(repos, rp)
				}
			}
			s.Render(w, r, "fake", "Fake GitHub", fakePage{
				Repos: repos, Users: store.Users(), Recent: store.Recent(15),
				Contributor: contributor.ID, Flash: r.URL.Query().Get("msg"), store: store,
			})
		})
		mux.HandleFunc("POST /fake/merge", func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			author, _ := strconv.ParseInt(r.Form.Get("author"), 10, 64)
			var apps []int64
			for _, v := range r.Form["approver"] {
				if id, _ := strconv.ParseInt(v, 10, 64); id != 0 {
					apps = append(apps, id)
				}
			}
			pr, err := store.Merge(r.Form.Get("repo"), author, apps)
			if err != nil {
				back(w, r, err.Error())
				return
			}
			back(w, r, fmt.Sprintf("Merged %s#%d. The attesters will pick it up within a few seconds.", pr.Repo, pr.Number))
		})
		mux.HandleFunc("POST /fake/link", func(w http.ResponseWriter, r *http.Request) {
			tc, _ := a.client()
			p, err := tc.Params(r.Context())
			if err != nil {
				back(w, r, err.Error())
				return
			}
			store.PublishGist(ownerID, "Linking my GitHub to NEAR\n"+claim.LinkProof(contributor.ID)+"\n")
			if _, err := contributor.Send(r.Context(), tc.Contract, token.RequestLink(ownerID, "vignesh2027", p.RequestDeposit.Big())); err != nil {
				back(w, r, "request_link failed: "+err.Error())
				return
			}
			back(w, r, "Gist published and link requested. The attesters will confirm it in a few seconds.")
		})
		mux.HandleFunc("POST /fake/feast", func(w http.ResponseWriter, r *http.Request) {
			tc, _ := a.client()
			if _, err := contributor.Send(r.Context(), tc.Contract, token.Feast()); err != nil {
				back(w, r, "feast: "+err.Error())
				return
			}
			back(w, r, "The feast was served. See the ledger.")
		})
		mux.HandleFunc("POST /fake/propose", func(w http.ResponseWriter, r *http.Request) {
			tc, _ := a.client()
			p, _ := tc.Params(r.Context())
			if _, err := contributor.Send(r.Context(), tc.Contract, token.ProposeRepo("community/fresh-lib", p.RequestDeposit.Big())); err != nil {
				back(w, r, "propose: "+err.Error())
				return
			}
			back(w, r, "Offering burned and community/fresh-lib proposed.")
		})
		mux.HandleFunc("POST /fake/exile", func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			repo := strings.ToLower(r.Form.Get("repo"))
			tc, _ := a.client()
			cost, err := tc.ExileCost(r.Context(), repo)
			if err != nil {
				back(w, r, err.Error())
				return
			}
			if _, err := contributor.Send(r.Context(), tc.Contract, token.Exile(repo, cost.Big())); err != nil {
				back(w, r, "exile: "+err.Error())
				return
			}
			back(w, r, "Burned "+cost.Tokens(2)+" MRGO. "+repo+" is exiled.")
		})
		mux.HandleFunc("POST /fake/lie", func(w http.ResponseWriter, r *http.Request) {
			if err := a.lie(ctx); err != nil {
				back(w, r, "lie: "+err.Error())
				return
			}
			back(w, r, "Attester 3 committed a claim for a pull request that does not exist. Watch the open rounds.")
		})
		return []web.NavLink{{Href: "/fake", Label: "Fake GitHub"}}
	}
}

// lie makes attester 3 commit and later reveal a made-up merge.
func (a *app) lie(ctx context.Context) error {
	idKey, keyKey := attesterEnv(3)
	liar, err := a.account(idKey, keyKey)
	if err != nil {
		return err
	}
	tc, err := a.client()
	if err != nil {
		return err
	}
	p, err := tc.Params(ctx)
	if err != nil {
		return err
	}
	repo := strings.ToLower(DefaultRepos[1])
	n := 900000 + int(time.Now().Unix()%99999)
	m := claim.Merge{Repo: repo, Number: n, MergeSHA: strings.Repeat("ab", 20), MergedAt: time.Now().Unix() - 60,
		AuthorID: 1003, AuthorLogin: "omar-ships", AuthorCreatedAt: time.Date(2019, 5, 1, 0, 0, 0, 0, time.UTC).Unix()}
	text := m.Text()
	var salt [32]byte
	_, _ = rand.Read(salt[:])
	key := m.Key()
	c := claim.Commitment(liar.ID, key, claim.Hash(&text), salt)
	if _, err := liar.Send(ctx, tc.Contract, token.Commit([]token.CommitItem{{Key: key, Commitment: claim.Hex(c)}})); err != nil {
		return err
	}
	go func() {
		for i := 0; i < 60; i++ {
			time.Sleep(2 * time.Second)
			rounds, err := tc.OpenRounds(ctx)
			if err != nil {
				continue
			}
			atts, _ := tc.Attesters(ctx)
			active := 0
			for _, x := range atts {
				if x.Active {
					active++
				}
			}
			for _, r := range rounds {
				if r.Key == key && attester.RevealReady(r, liar.ID, active, time.Now().Unix()) {
					out, err := liar.Send(ctx, tc.Contract, token.Reveal([]token.RevealItem{{Key: key, Claim: &text, Salt: claim.Hex(salt)}}))
					if err != nil {
						log.Printf("liar reveal: %v", err)
						return
					}
					log.Printf("liar revealed %s: %s", key, strings.Join(token.Statuses(out.Value), ","))
					// Top the bond back up so the demo can continue with three attesters.
					time.Sleep(4 * time.Second)
					if _, err := liar.Send(ctx, tc.Contract, token.Bond(p.Bond.Big())); err != nil {
						log.Printf("liar re-bond: %v", err)
					}
					return
				}
			}
		}
	}()
	return nil
}
