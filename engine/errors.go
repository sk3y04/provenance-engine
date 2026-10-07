package engine

import (
	"context"
	"errors"
)

// ErrorKind is a stable failure category understood by all engine consumers.
// Callers branch on it with ErrorKindOf or IsErrorKind instead of matching
// error strings.
type ErrorKind string

const (
	// ErrorValidation means the request or its inputs were invalid.
	ErrorValidation ErrorKind = "validation"
	// ErrorAuthRequired means the source needs credentials that were missing,
	// expired, or rejected.
	ErrorAuthRequired ErrorKind = "authentication_required"
	// ErrorUnsupportedSource means no extractor can handle the source.
	ErrorUnsupportedSource ErrorKind = "unsupported_source"
	// ErrorRateLimited means the remote service throttled the request.
	ErrorRateLimited ErrorKind = "rate_limited"
	// ErrorTemporary means a transient failure that may succeed on retry.
	ErrorTemporary ErrorKind = "temporary"
	// ErrorCanceled means the operation was canceled or timed out.
	ErrorCanceled ErrorKind = "canceled"
	// ErrorExternalTool means a yt-dlp/ffmpeg/ffprobe/Chrome subprocess failed.
	ErrorExternalTool ErrorKind = "external_tool"
	// ErrorPermanent means a non-retryable failure.
	ErrorPermanent ErrorKind = "permanent"
)

// Error is a categorized engine failure. It wraps the underlying cause so
// errors.Is and errors.As continue to work through the chain. The message is
// the wrapped cause's message, preserving existing CLI text.
type Error struct {
	Kind ErrorKind
	Op   string
	URL  string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Kind)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ErrorKindOf returns the category of err. Context cancellation and deadline
// errors always map to ErrorCanceled. It returns "" for uncategorized errors
// and nil.
func ErrorKindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorCanceled
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// IsErrorKind reports whether err carries the given category.
func IsErrorKind(err error, kind ErrorKind) bool { return ErrorKindOf(err) == kind }
