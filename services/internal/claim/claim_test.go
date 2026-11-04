package claim

import "testing"

// The same vector is asserted in contract/src/claim.rs (tests::vectors).
func TestSharedVector(t *testing.T) {
	m := Merge{
		Repo: "prometheus/client_golang", Number: 2182,
		MergeSHA: "0123456789ABCDEF0123456789abcdef01234567", MergedAt: 1793010367,
		AuthorID: 101, AuthorLogin: "vignesh2027", AuthorCreatedAt: 1500000000,
		Approvers: []Approver{{99, "kakkoyun"}, {7, "beorn7"}, {101, "vignesh2027"}, {7, "beorn7"}},
	}
	want := "merge\nprometheus/client_golang\n2182\n0123456789abcdef0123456789abcdef01234567\n1793010367\n101\nvignesh2027\n1500000000\n7:beorn7,99:kakkoyun"
	text := m.Text()
	if text != want {
		t.Fatalf("text:\n%q\nwant\n%q", text, want)
	}
	h := Hash(&text)
	if got := Hex(h); got != "e5b2e5ad992365a0f71895aac591225acf747ded72ecdcdc1961c6e02551e79f" {
		t.Fatalf("hash %s", got)
	}
	var salt [32]byte
	for i := range salt {
		salt[i] = 7
	}
	if got := Hex(Commitment("attester-1.testnet", m.Key(), h, salt)); got != "79da1e975b4e0a0c7f4b4a79ac927dd1858d6d870c4a02a657d1f90b6458a68d" {
		t.Fatalf("commitment %s", got)
	}
	if m.Key() != "merge:prometheus/client_golang#2182" {
		t.Fatal(m.Key())
	}
}

func TestOtherClaims(t *testing.T) {
	l := Link{GitHubID: 42, Login: "alice", Account: "alice.testnet", GitHubCreatedAt: 1500000000}
	if l.Text() != "link\n42\nalice\nalice.testnet\n1500000000" || l.Key() != "link:42:alice.testnet" {
		t.Fatal(l.Text(), l.Key())
	}
	r := Repo{Repo: "Helm/Chart-Testing", RepoID: 12345, Stars: 1500, CreatedAt: 1500000000}
	if r.Text() != "repo\nhelm/chart-testing\n12345\n1500\n1500000000\n0" || r.Key() != "repo:helm/chart-testing" {
		t.Fatal(r.Text(), r.Key())
	}
	if Hash(nil) != [32]byte{} {
		t.Fatal("none hash")
	}
	if Salt([]byte("a"), "k") == Salt([]byte("b"), "k") || Salt([]byte("a"), "k") != Salt([]byte("a"), "k") {
		t.Fatal("salt")
	}
}
