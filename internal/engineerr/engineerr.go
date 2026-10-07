// Package engineerr defines transport-independent categorized errors for the
// reusable engine paths. Callers classify failures with errors.As/errors.Is
// instead of matching error strings.
package engineerr

import (
	"context"
	"errors"
	"fmt"
)

// Kind is a stable failure category understood by all engine consumers.
type Kind string

const (
	// Validation indicates the request or its inputs were invalid.
	Validation Kind = "validation"
	// AuthRequired indicates the source needs cookies or credentials that were
	// missing, expired, or rejected.
	AuthRequired Kind = "authentication_required"
	// UnsupportedSource indicates no extractor can handle the source.
	UnsupportedSource Kind = "unsupported_source"
	// RateLimited indicates the remote service throttled the request.
	RateLimited Kind = "rate_limited"
	// Temporary indicates a transient failure that may succeed on retry.
	Temporary Kind = "temporary"
	// Canceled indicates the operation was canceled or its deadline elapsed.
	Canceled Kind = "canceled"
	// ExternalTool indicates a yt-dlp/ffmpeg/ffprobe/Chrome subprocess failure.
	ExternalTool Kind = "external_tool"
	// Permanent indicates a non-retryable failure.
	Permanent Kind = "permanent"
)

// Error is a categorized engine failure. It wraps the underlying cause so
// errors.Is/errors.As continue to work through the chain.
type Error struct {
	Kind Kind
	Op   string
	URL  string
	Err  error
}

// Error returns the underlying cause's message unchanged so existing CLI text
// is preserved. Kind/Op/URL are structured fields, not part of the message.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Kind)
}

func (e *Error) Unwrap() error { return e.Err }

// New categorizes err. A nil err is replaced with a message derived from kind.
// Cancellation and deadline errors are always categorized as Canceled, never
// wrapped into an unrelated category.
func New(kind Kind, op, url string, err error) *Error {
	if err == nil {
		err = errors.New(string(kind))
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		kind = Canceled
	}
	return &Error{Kind: kind, Op: op, URL: url, Err: err}
}

// Newf is New with a formatted message; the cause is not wrapped.
func Newf(kind Kind, op, url, format string, args ...any) *Error {
	return New(kind, op, url, fmt.Errorf(format, args...))
}

// KindOf returns the category of err. Context cancellation maps to Canceled.
// It returns "" when err is not categorized.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Canceled
	}
	return ""
}

// Is reports whether err carries the given category.
func Is(err error, kind Kind) bool { return KindOf(err) == kind }

// As extracts the outermost categorized error from err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
