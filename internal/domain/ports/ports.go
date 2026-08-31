// Package ports declara as PORTAS de infraestrutura, em linguagem do domínio.
//
// Regra da ADR-0001: o domínio define a porta com a superfície mais estreita de
// que precisa; adaptadores de fornecedor ficam em internal/adapter e são
// escolhidos por configuração no composition root. Nenhum SDK cruza esta
// fronteira, e capacidade que não mapeia entre adaptadores fica FORA da porta.
package ports

import (
	"context"
	"time"
)

// ───────────────────────── SecretStore ─────────────────────────

// SecretRef é uma referência LÓGICA e opaca: só o adaptador sabe resolvê-la
// (caminho no Secret Manager, nome de Secret no k8s). O domínio nunca conhece
// caminho, namespace nem nome de segredo.
type SecretRef struct {
	AccountID string
	Kind      string // integration_credential
	OwnerID   string
}

// SecretValue é opaco por construção: sem String() útil, não serializa em log.
type SecretValue []byte

func (SecretValue) String() string { return "***" }

// SecretStore — quatro operações e nada além.
//
// Garantias verificadas pelo conjunto de testes de contrato, em TODO adaptador:
//  1. leitura-após-escrita: Put seguido de Get devolve o mesmo valor, imediatamente;
//  2. Get de referência inexistente devolve (nil, nil) — não erro;
//  3. Delete é idempotente;
//  4. Put sobre referência existente substitui;
//  5. isolamento: referência da conta A jamais resolve segredo da conta B;
//  6. o valor nunca aparece em log, erro ou stack trace.
//
// Versionamento fica FORA da porta: o Secret Manager tem versões, o Secret do
// k8s é plano. Capacidade que não mapeia não entra.
type SecretStore interface {
	Put(ctx context.Context, ref SecretRef, v SecretValue) error
	Get(ctx context.Context, ref SecretRef) (SecretValue, error)
	Delete(ctx context.Context, ref SecretRef) error
	Exists(ctx context.Context, ref SecretRef) (bool, error)
}

// ───────────────────────── ObjectStore ─────────────────────────

type ObjectRef struct {
	Bucket string
	Key    string
}

type ObjectMeta struct {
	Size        int64
	ContentType string
	UpdatedAt   time.Time
}

// ObjectStore guarda artefatos de conhecimento, uploads do chat e diagramas.
// SignedPutURL existe para o binário do upload NÃO passar pelo BFF.
type ObjectStore interface {
	Put(ctx context.Context, ref ObjectRef, content []byte, contentType string) error
	Get(ctx context.Context, ref ObjectRef) ([]byte, error)
	Delete(ctx context.Context, ref ObjectRef) error
	Stat(ctx context.Context, ref ObjectRef) (*ObjectMeta, error)
	SignedPutURL(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
	SignedGetURL(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
}

// ───────────────────────── IdentityProvider ─────────────────────────

// Principal é o resultado NORMALIZADO da verificação de token. Claims de
// Firebase não cruzam esta fronteira — é o que permite trocar por Keycloak,
// Zitadel ou Ory sem tocar no domínio.
type Principal struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	AvatarURL     string
	Providers     []string
}

type IdentityProvider interface {
	VerifyToken(ctx context.Context, raw string) (*Principal, error)
}

// ───────────────────────── EventBus ─────────────────────────

type Event struct {
	ID          string
	AccountID   string
	Aggregate   string
	AggregateID string
	Type        string
	Payload     []byte
	OccurredAt  time.Time
}

// Handler processa um evento. DEVE ser idempotente: a entrega é ao-menos-uma-vez
// (ADR-0019). Erro devolvido provoca retry com backoff; após o teto, DLQ.
type Handler func(ctx context.Context, e Event) error

type EventBus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(ctx context.Context, stream, durable string, subjects []string, h Handler) error
	Close() error
}

// ───────────────────────── Clock e IDs ─────────────────────────
// Pequenas, mas reais: é o que torna o domínio determinístico em teste.

type Clock interface{ Now() time.Time }

type IDGenerator interface{ NewID() string }
