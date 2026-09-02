package app

import (
	"context"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// UnaryLogging records the entry, the exit, the duration and the error of every
// call — in JSON, with the BFF's same fields.
func UnaryLogging() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()
		log := logging.From(ctx).With("rpc", info.FullMethod)
		if c, ok := ctxutil.From(ctx); ok {
			log = log.With(
				logging.FieldRequestID, c.RequestID,
				logging.FieldAccountID, c.AccountID,
				logging.FieldActorID, c.ActorID,
				logging.FieldActorKind, string(c.ActorKind),
			)
		}
		ctx = logging.Into(ctx, log)

		resp, err := h(ctx, req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			log.Error("rpc falhou", logging.FieldError, err.Error(), logging.FieldDurationMs, ms)
			return nil, toStatus(err)
		}
		log.Info("rpc finished", logging.FieldDurationMs, ms)
		return resp, nil
	}
}

func StreamLogging() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		start := time.Now()
		log := logging.From(ss.Context()).With("rpc", info.FullMethod, "stream", true)
		err := h(srv, ss)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			log.Error("stream falhou", logging.FieldError, err.Error(), logging.FieldDurationMs, ms)
			return toStatus(err)
		}
		log.Info("stream encerrado", logging.FieldDurationMs, ms)
		return nil
	}
}

// UnaryCallContext extracts the call context from the metadata and puts it into
// the context.Context. The EDGE fills it in; the domain trusts it (ADR-0016).
func UnaryCallContext() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(callFromMD(ctx), req)
	}
}

// StreamCallContext does for streaming what UnaryCallContext does for unary
// calls.
//
// Without it, a streaming RPC sees no x-account-id at all, and the stream's
// multi-tenant isolation would come to depend on the CallContext declared in the
// request's body — a field the server does NOT read (ADR-0017, conv. 5). That
// is: the hole would not be "a stream with no context", it would be "a stream
// with the context the client chooses". Context is a cross-cutting concern in
// both kinds of RPC.
//
// grpc.ServerStream does not let you swap the Context, so we wrap the stream.
func StreamCallContext() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		return h(srv, &streamWithContext{ServerStream: ss, ctx: callFromMD(ss.Context())})
	}
}

// streamWithContext exists only to override Context(): it is the only extension
// point the interface offers.
type streamWithContext struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *streamWithContext) Context() context.Context { return s.ctx }

// callFromMD is the metadata read, shared by both interceptors — duplicating it
// is how the two ends diverge without anyone noticing.
func callFromMD(ctx context.Context) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	call := ctxutil.Call{
		RequestID: first(md, "x-request-id"),
		AccountID: first(md, "x-account-id"),
		ActorID:   first(md, "x-actor-id"),
		ActorName: first(md, "x-actor-name"),
		ActorKind: ctxutil.ActorKind(first(md, "x-actor-kind")),
	}
	if call.ActorKind == "" {
		call.ActorKind = ctxutil.ActorUser
	}
	return ctxutil.Into(ctx, call)
}

// UnaryRecover stops a panic from bringing the whole process down.
func UnaryRecover() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logging.From(ctx).Error("panic in the RPC",
					"rpc", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return h(ctx, req)
	}
}

func first(md metadata.MD, k string) string {
	if v := md.Get(k); len(v) > 0 {
		return v[0]
	}
	return ""
}

// toStatus translates a domain error into a gRPC status. The translation lives
// HERE, at the edge — the domain never imports google.golang.org/grpc.
func toStatus(err error) error {
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	var code codes.Code
	switch errs.KindOf(err) {
	case errs.KindNotFound:
		code = codes.NotFound
	case errs.KindAlreadyExists:
		code = codes.AlreadyExists
	case errs.KindInvalid:
		code = codes.InvalidArgument
	case errs.KindPermission:
		code = codes.PermissionDenied
	case errs.KindUnauthorized:
		code = codes.Unauthenticated
	case errs.KindConflict:
		code = codes.Aborted
	case errs.KindPrecondition:
		code = codes.FailedPrecondition
	case errs.KindUnavailable:
		code = codes.Unavailable
	default:
		code = codes.Internal
	}
	return status.Error(code, err.Error())
}
