package redact

import (
	"errors"
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

func TestErrorRedactsMessageAndPreservesCause(t *testing.T) {
	cause := errors.New("failed https://x.com/a?token=CANARYTOKEN auth_token=CANARYSECRET")
	got := Error(cause)
	if strings.Contains(got.Error(), "CANARYTOKEN") || strings.Contains(got.Error(), "CANARYSECRET") {
		t.Fatalf("secret leaked in error: %q", got.Error())
	}
	if !errors.Is(got, cause) {
		t.Fatal("redacted error must still unwrap to the original cause")
	}
	if Error(nil) != nil {
		t.Fatal("Error(nil) must be nil")
	}
}
