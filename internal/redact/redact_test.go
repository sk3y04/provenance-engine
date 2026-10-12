package redact

import (
	"strings"
	"testing"
)

func TestDetailRedactsSecretsAndBoundsLength(t *testing.T) {
	canary := "CANARYSECRET"
	got := Detail("response auth_token=" + canary + "; cookie=" + canary)
	if strings.Contains(got, canary) {
		t.Fatalf("secret leaked: %q", got)
	}
	if len(got) > MaxDetailLen+3 {
		t.Fatalf("detail not bounded: %q", got)
	}
}

func TestURLDropsQueryAndFragment(t *testing.T) {
	got := URL("https://x.com/a?token=CANARY#frag")
	if got != "https://x.com/a" {
		t.Fatalf("url = %q", got)
	}
}

func TestDetailRedactsEmbeddedURLs(t *testing.T) {
	got := Detail("signed url https://cdn.example.com/f?token=CANARYTOKEN failed")
	if strings.Contains(got, "CANARYTOKEN") {
		t.Fatalf("query token leaked: %q", got)
	}
}
