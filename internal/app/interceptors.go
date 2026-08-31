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

// UnaryLogging registra entrada, saída, duração e erro de toda chamada — em
// JSON, com os mesmos campos do BFF.
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
		log.Info("rpc concluída", logging.FieldDurationMs, ms)
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

// UnaryCallContext extrai o contexto de chamada dos metadados e o coloca no
// context.Context. A BORDA preenche; o domínio confia (ADR-0016).
func UnaryCallContext() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
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
		return h(ctxutil.Into(ctx, call), req)
	}
}

// UnaryRecover impede que um pânico derrube o processo inteiro.
func UnaryRecover() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logging.From(ctx).Error("pânico na RPC",
					"rpc", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "erro interno")
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

// toStatus traduz erro de domínio para status gRPC. A tradução vive AQUI, na
// borda — o domínio nunca importa google.golang.org/grpc.
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
