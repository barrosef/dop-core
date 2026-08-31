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
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//  1. leitura-após-escrita: Put seguido de Get devolve os mesmos bytes, imediatamente;
//  2. Put SUBSTITUI o objeto inteiro — sem merge, sem versão — e a troca é
//     atômica para quem lê: nunca se lê metade de um objeto;
//  3. ausência é ERRO, com KindNotFound, em Get e Stat. Diferente do SecretStore,
//     que devolve (nil, nil): lá a ausência é estado normal do fluxo de
//     credencial; aqui o artefato pedido não existir é falha do caso de uso;
//  4. Delete é idempotente: remover o que não existe devolve nil;
//  5. a chave é OPACA e PLANA. Pode conter "/", mas isso NÃO cria hierarquia:
//     "a/b" e "a/b/c" são dois objetos independentes, "a/b" não é prefixo
//     navegável, e chave nenhuma escapa do bucket ("../x" é nome literal);
//  6. o bucket isola: a mesma chave em buckets diferentes são objetos diferentes;
//  7. Stat.Size é o tamanho exato do conteúdo gravado (zero é válido);
//     ContentType devolve o que foi gravado, e "application/octet-stream" quando
//     o Put não informou; UpdatedAt nunca é zero e não regride entre escritas;
//  8. SignedPutURL/SignedGetURL ou devolvem URL não vazia sem tocar no objeto,
//     ou falham com KindUnavailable — NUNCA string vazia com erro nil. A
//     capacidade não existe em todo backend (armazenamento em arquivo não tem o
//     que assinar) e a alternativa é passar o binário pelo BFF; o chamador
//     precisa poder DESCOBRIR isso em vez de receber uma URL que não funciona;
//  9. seguro para uso concorrente.
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

// EventBus transporta BYTES, não structs.
//
// Payload é o envelope JSON do evento — o mesmo que o outbox grava. Essa frase
// é a garantia mais importante desta porta, porque a violação dela é silenciosa:
// um adaptador que reserialize ports.Event manda Payload []byte em base64, o
// outro lado não reconhece nada e TODA entrega é descartada sem erro nenhum.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//  1. os bytes de Payload chegam IDÊNTICOS aos publicados, sem recodificação;
//  2. os demais campos do evento entregue são lidos do envelope, não copiados
//     do struct publicado — o assinante enxerga o que trafegou no fio;
//  3. Publish sem Payload monta um envelope a partir do evento (nunca um
//     json.Marshal do ports.Event), preservando os campos de identificação;
//  4. Type é o assunto da publicação; assinatura filtra por assunto com a
//     semântica do NATS ("*" casa um token, ">" casa a cauda), e lista vazia
//     casa tudo;
//  5. entrega ao-menos-uma-vez, ordem NÃO garantida — o Handler precisa ser
//     idempotente;
//  6. o que foi publicado ANTES da assinatura é entregue quando o durável
//     aparece (retenção), senão a espinha de eventos só funcionaria com a ordem
//     de boot certa;
//  7. erro do Handler provoca reentrega; sucesso não;
//  8. mensagem ilegível é DESCARTADA com registro, não reentregue para sempre:
//     uma mensagem venenosa não pode travar a fila;
//  9. Publish é seguro para uso concorrente; evento sem Type é recusado.
//
// FORA da porta, de propósito: desduplicação. O JetStream desduplica por ID
// dentro de uma janela; o adaptador em memória não desduplica. Como a entrega é
// ao-menos-uma-vez em ambos, consumidor idempotente é obrigatório de qualquer
// jeito — prometer dedup seria prometer o que só um adaptador cumpre.
type EventBus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(ctx context.Context, stream, durable string, subjects []string, h Handler) error
	Close() error
}

// ───────────────────────── Clock e IDs ─────────────────────────
// Pequenas, mas reais: é o que torna o domínio determinístico em teste.

// Clock é a ÚNICA fonte de "agora" do domínio.
//
// Garantias verificadas pela suíte de contrato, em TODO adaptador:
//  1. Now nunca devolve o instante zero;
//  2. Now devolve sempre UTC — instante sem fuso definido é ambiguidade que
//     vira erro de comparação quando o processo e o banco discordam de TZ;
//  3. Now não regride entre chamadas sucessivas;
//  4. seguro para uso concorrente.
//
// O domínio recebe a porta e NUNCA chama time.Now() por dentro, nem como
// fallback para clock nulo: o fallback desliga a porta sem ninguém perceber e
// devolve ao teste a dependência do relógio de parede que a porta existe para
// remover. Serviço que precisa de tempo EXIGE o relógio no construtor.
type Clock interface{ Now() time.Time }

type IDGenerator interface{ NewID() string }
