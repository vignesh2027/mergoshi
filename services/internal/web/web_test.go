package web

import (
	"strings"
	"testing"
	"time"
)

func TestTemplatesParse(t *testing.T) {
	s := &Server{Network: "testnet"}
	if err := s.templates(); err != nil {
		t.Fatal(err)
	}
	if len(s.tpl) != 7 {
		t.Fatalf("pages %d", len(s.tpl))
	}
}

func TestHelpers(t *testing.T) {
	if got := string(keyLink("merge:helm/chart-testing#873")); !strings.Contains(got, `href="https://github.com/helm/chart-testing/pull/873"`) {
		t.Fatal(got)
	}
	if got := string(keyLink("link:1:<b>")); strings.Contains(got, "<b>") {
		t.Fatal("key not escaped")
	}
	s := &Server{Network: "testnet"}
	if got := string(s.accountLink("github:42")); !strings.Contains(got, "escrow") {
		t.Fatal(got)
	}
	if humanDuration(90*time.Second) != "1m" || humanDuration(3*24*time.Hour) != "3 days" {
		t.Fatal("durations")
	}
}
