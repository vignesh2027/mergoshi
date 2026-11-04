// Package claim builds the canonical claim texts that attesters commit to and
// reveal. The bytes must match contract/src/claim.rs exactly; the shared test
// vectors in claim_test.go and in the contract's claim tests keep them in step.
package claim

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Approver is a GitHub account that approved a pull request.
type Approver struct {
	ID    int64
	Login string
}

// Merge describes one merged pull request.
type Merge struct {
	Repo            string // owner/name, any case
	Number          int
	MergeSHA        string
	MergedAt        int64
	AuthorID        int64
	AuthorLogin     string
	AuthorCreatedAt int64
	Approvers       []Approver
}

// Key is the round key for the merge.
func (m Merge) Key() string { return MergeKey(m.Repo, m.Number) }

// Text is the canonical claim.
func (m Merge) Text() string {
	apps := append([]Approver(nil), m.Approvers...)
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	var pairs []string
	var last int64 = -1
	for _, a := range apps {
		if a.ID == m.AuthorID || a.ID == last {
			continue
		}
		last = a.ID
		pairs = append(pairs, fmt.Sprintf("%d:%s", a.ID, a.Login))
	}
	return strings.Join([]string{
		"merge", strings.ToLower(m.Repo), strconv.Itoa(m.Number), strings.ToLower(m.MergeSHA),
		strconv.FormatInt(m.MergedAt, 10), strconv.FormatInt(m.AuthorID, 10), m.AuthorLogin,
		strconv.FormatInt(m.AuthorCreatedAt, 10), strings.Join(pairs, ","),
	}, "\n")
}

// Link ties a GitHub account to a NEAR account.
type Link struct {
	GitHubID        int64
	Login           string
	Account         string
	GitHubCreatedAt int64
}

func (l Link) Key() string { return LinkKey(l.GitHubID, l.Account) }

func (l Link) Text() string {
	return strings.Join([]string{"link", strconv.FormatInt(l.GitHubID, 10), l.Login, l.Account, strconv.FormatInt(l.GitHubCreatedAt, 10)}, "\n")
}

// Repo is a repository proposed for enrollment.
type Repo struct {
	Repo      string
	RepoID    int64
	StarsOK   bool // at least min_repo_stars; a flag so attesters agree despite new stars
	CreatedAt int64
	Fork      bool
}

func (r Repo) Key() string { return RepoKey(r.Repo) }

func (r Repo) Text() string {
	fork := "0"
	if r.Fork {
		fork = "1"
	}
	stars := "0"
	if r.StarsOK {
		stars = "1"
	}
	return strings.Join([]string{"repo", strings.ToLower(r.Repo), strconv.FormatInt(r.RepoID, 10), stars, strconv.FormatInt(r.CreatedAt, 10), fork}, "\n")
}

func MergeKey(repo string, number int) string {
	return fmt.Sprintf("merge:%s#%d", strings.ToLower(repo), number)
}
func LinkKey(id int64, account string) string { return fmt.Sprintf("link:%d:%s", id, account) }
func RepoKey(repo string) string              { return "repo:" + strings.ToLower(repo) }

// LinkProof is the text a contributor puts in a public gist to link accounts.
func LinkProof(account string) string { return "mergoshi-link:" + account }

// Hash is sha256 of the claim text; a nil text is the NONE vote (32 zero bytes).
func Hash(text *string) [32]byte {
	if text == nil {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(*text))
}

// Commitment is sha256(attester || "\n" || key || "\n" || hash || salt).
func Commitment(attester, key string, hash, salt [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(attester))
	h.Write([]byte{'\n'})
	h.Write([]byte(key))
	h.Write([]byte{'\n'})
	h.Write(hash[:])
	h.Write(salt[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Salt derives a per-key salt from an attester secret, so an attester can
// restart and still reveal without storing salts.
func Salt(secret []byte, key string) [32]byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(key))
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

func Hex(b [32]byte) string { return hex.EncodeToString(b[:]) }
