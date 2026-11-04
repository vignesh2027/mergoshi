package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSetAndPrecedence(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("# comment\nA=1\nB=\"two\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Get("A") != "1" || e.Get("B") != "two" || e.Or("C", "x") != "x" {
		t.Fatal("values")
	}
	t.Setenv("A", "env")
	if e.Get("A") != "env" {
		t.Fatal("environment must win over the file")
	}
	if err := e.Set("C", "3"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatal("settings file must stay private")
	}
	e2, _ := Load(p)
	if e2.Get("C") != "3" {
		t.Fatal("set value not persisted")
	}
	if _, err := e2.Must("MISSING"); err == nil {
		t.Fatal("expected error")
	}
}
