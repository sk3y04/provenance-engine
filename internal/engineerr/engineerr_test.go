package engineerr

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestNewAndKindOf(t *testing.T) {
	cause := errors.New("boom")
	err := New(ExternalTool, "yt-dlp", "https://example.com/v", cause)

	if got := KindOf(err); got != ExternalTool {
		t.Fatalf("KindOf = %q, want %q", got, ExternalTool)
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped cause is not reachable via errors.Is")
	}
	if !Is(err, ExternalTool) {
		t.Fatal("Is(ExternalTool) = false")
	}
	e, ok := As(err)
	if !ok || e.Op != "yt-dlp" || e.URL != "https://example.com/v" {
		t.Fatalf("As() = %+v, %v", e, ok)
	}
}

func TestNilCause(t *testing.T) {
	err := New(Validation, "request", "", nil)
	if err == nil {
		t.Fatal("New returned nil")
	}
	if KindOf(err) != Validation {
		t.Fatalf("KindOf = %q, want validation", KindOf(err))
	}
}

func TestCancellationNeverRecategorized(t *testing.T) {
	cases := []error{context.Canceled, context.DeadlineExceeded,
		fmt.Errorf("wrapped: %w", context.Canceled)}
	for _, cause := range cases {
		for _, requested := range []Kind{Permanent, Temporary, ExternalTool, RateLimited} {
			err := New(requested, "download", "u", cause)
			if KindOf(err) != Canceled {
				t.Fatalf("requested=%q cause=%v: KindOf = %q, want canceled",
					requested, cause, KindOf(err))
			}
			if !errors.Is(err, cause) {
				t.Fatalf("requested=%q: errors.Is does not reach context error", requested)
			}
		}
	}
}

func TestKindOfPlainError(t *testing.T) {
	if got := KindOf(errors.New("plain")); got != "" {
		t.Fatalf("KindOf(plain) = %q, want empty", got)
	}
	if got := KindOf(nil); got != "" {
		t.Fatalf("KindOf(nil) = %q, want empty", got)
	}
}

func TestKindOfContextDirect(t *testing.T) {
	if got := KindOf(context.DeadlineExceeded); got != Canceled {
		t.Fatalf("KindOf(deadline) = %q, want canceled", got)
	}
}

func TestErrorFormatting(t *testing.T) {
	// Error() preserves the wrapped cause message so CLI text is unchanged.
	if got := New(Permanent, "op", "u", errors.New("x")).Error(); got != "x" {
		t.Fatalf("Error() = %q", got)
	}
	if got := New(Permanent, "op", "u", nil).Error(); got != "permanent" {
		t.Fatalf("Error() = %q", got)
	}
	var nilErr *Error
	if got := nilErr.Error(); got != "<nil>" {
		t.Fatalf("nil Error() = %q", got)
	}
}
