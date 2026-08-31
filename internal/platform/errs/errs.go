// Package errs define os erros de domínio e sua tradução para gRPC.
// O domínio nunca importa google.golang.org/grpc — a tradução vive na borda.
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

type Error struct {
	Kind    Kind
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}
func (e *Error) Unwrap() error { return e.Cause }

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

func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}
