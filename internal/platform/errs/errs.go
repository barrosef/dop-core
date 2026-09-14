// Package errs defines the domain errors and their translation to gRPC.
// The domain never imports google.golang.org/grpc — that translation lives at
// the edge.
package errs

import (
	"errors"
	"fmt"
)

type Kind string

const (
	KindNotFound      Kind = "not_found"
	KindAlreadyExists Kind = "already_exists"
	KindInvalid       Kind = "invalid_argument"
	KindPermission    Kind = "permission_denied"
	KindUnauthorized  Kind = "unauthenticated"
	KindConflict      Kind = "conflict"
	KindPrecondition  Kind = "failed_precondition"
	KindUnavailable   Kind = "unavailable"
	KindInternal      Kind = "internal"
)

// Error carries two different things on purpose, for two different readers.
//
// Message is for the DEVELOPER: it lands in logs and in `go test` output, it
// is written in English, and nothing user-facing may be built from it. Wording
// here changes freely, because nothing depends on it.
//
// Code and Params are for the USER, through translation at the edge. Code is a
// stable identifier ("invite.wrong_recipient") and Params carries the values
// the sentence needs. They exist because a human-facing message cannot be
// hardcoded in any language: the cockpit resolves Code against its catalogue
// and fills in Params.
//
// Code is OPTIONAL, and its absence is meaningful: an error with no Code never
// reaches a person as prose — it is a fault, and the edge shows a generic
// message for its Kind. Only errors a user is meant to act on earn a Code.
type Error struct {
	Kind    Kind
	Message string
	Code    string
	Params  map[string]any
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}
func (e *Error) Unwrap() error { return e.Cause }

// WithCode attaches the translation key and its parameters.
//
// It is a separate call, and not a parameter of every constructor, so that
// adding a code to an existing error is a one-line change — and so that the
// call sites that legitimately have no code stay short.
func (e *Error) WithCode(code string, params map[string]any) *Error {
	e.Code = code
	e.Params = params
	return e
}

func New(k Kind, msg string, args ...any) *Error {
	return &Error{Kind: k, Message: fmt.Sprintf(msg, args...)}
}
func Wrap(k Kind, cause error, msg string, args ...any) *Error {
	return &Error{Kind: k, Message: fmt.Sprintf(msg, args...), Cause: cause}
}

func NotFound(msg string, a ...any) *Error     { return New(KindNotFound, msg, a...) }
func Invalid(msg string, a ...any) *Error      { return New(KindInvalid, msg, a...) }
func Permission(msg string, a ...any) *Error   { return New(KindPermission, msg, a...) }
func Conflict(msg string, a ...any) *Error     { return New(KindConflict, msg, a...) }
func Precondition(msg string, a ...any) *Error { return New(KindPrecondition, msg, a...) }
func Internal(msg string, a ...any) *Error     { return New(KindInternal, msg, a...) }

// classifiers lets other packages register the translation of their sentinel
// errors without this package having to import them (which would be a cycle).
var classifiers []func(error) (Kind, bool)

// RegisterClassifier maps a sentinel error to a Kind.
func RegisterClassifier(f func(error) (Kind, bool)) {
	classifiers = append(classifiers, f)
}

func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	for _, c := range classifiers {
		if k, ok := c(err); ok {
			return k
		}
	}
	return KindInternal
}

// CodeOf returns the translation key, or "" when the error carries none.
// The edge uses it to decide between a translated sentence and the generic
// message for the Kind.
func CodeOf(err error) (string, map[string]any) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, e.Params
	}
	return "", nil
}

// CodeOrKind returns the error's Code, falling back to its Kind when no Code
// was attached.
//
// A Code is opt-in — only errors meant to reach a person as prose carry one —
// so most internal failures have none. Whoever derives an identity FROM an
// error (a signature key keyed on (consumer, code), for instance) cannot use
// CodeOf alone: every uncoded failure would collapse to the same empty
// string, and one broken template would promote the whole consumer instead
// of just that one failure mode. The Kind is coarser than a Code but still
// tells failure modes apart, and every error has one.
func CodeOrKind(err error) string {
	if code, _ := CodeOf(err); code != "" {
		return code
	}
	return string(KindOf(err))
}
