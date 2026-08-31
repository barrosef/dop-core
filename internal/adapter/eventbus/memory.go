// Adaptador de EventBus em memória — o par do NATS pela porta.
//
// Existe por dois motivos, nesta ordem: (1) provar a porta, que com um
// adaptador só é palpite (ADR-0001); (2) deixar o núcleo rodar num processo
// único — teste de caso de uso e modo demonstração — sem broker no caminho.
//
// O que ele NÃO é: um mock. Ele entrega de verdade, em goroutine, com retentativa
// com backoff, teto de tentativas e descarte de mensagem ilegível, porque é
// exatamente isso que o JetStream faz. Um duplo que entregasse síncrono e
// perfeito esconderia os bugs que só aparecem com entrega assíncrona.
//
// A armadilha que este arquivo repete de propósito: o que trafega é o ENVELOPE
// (ver nats.go). Publish carrega os BYTES de e.Payload sem recodificar e o
// assinante recebe esses mesmos bytes. Reserializar ports.Event aqui traria de
// volta o bug que custou caro — Payload []byte vira base64 no JSON e o outro
// lado descarta tudo, em silêncio.
package eventbus

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// retencaoMemoria limita o histórico guardado para assinantes que chegam depois.
//
// O stream do JetStream retém 30 dias; aqui reter tudo seria vazamento de
// memória em processo longo. O número é generoso para o uso real (worker que
// sobe depois do relay) e finito para o processo sobreviver.
const retencaoMemoria = 1024

// backoffMemoria é a escada de reentrega. Mais curta que a do NATS de
// propósito: sem rede no caminho, esperar um segundo só faria teste lento.
// A porta não promete tempo de reentrega — promete QUE reentrega.
var backoffMemoria = []time.Duration{
	5 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond,
}

type Memory struct {
	mu         sync.Mutex
	fechado    bool
	historico  []entrega
	assinantes []*assinaturaMem
	wg         sync.WaitGroup
}

func NewMemory() *Memory { return &Memory{} }

// entrega é uma mensagem no fio: assunto + bytes crus, mais o contador de
// tentativas. Nada de ports.Event aqui — o que trafega é byte.
type entrega struct {
	assunto   string
	dados     []byte
	tentativa int
}

type assinaturaMem struct {
	durable  string
	assuntos []string
	handler  ports.Handler
	fila     *filaMem
	log      *slog.Logger
	ctx      context.Context
}

func (m *Memory) Publish(_ context.Context, e ports.Event) error {
	if strings.TrimSpace(e.Type) == "" {
		return errs.Invalid("evento sem tipo: não há assunto para publicar")
	}
	dados := e.Payload
	if len(dados) == 0 {
		// Mesmo fallback do nats.go, e pela mesma razão: quem publica sem
		// envelope pronto recebe um envelope montado aqui — nunca um
		// json.Marshal(ports.Event), cujo Payload []byte sairia em base64 e com
		// nomes de campo que o consumidor não sabe ler.
		dados = envelopeDe(e)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fechado {
		return errs.New(errs.KindUnavailable, "barramento em memória já encerrado")
	}
	msg := entrega{assunto: e.Type, dados: dados}
	m.historico = append(m.historico, msg)
	if len(m.historico) > retencaoMemoria {
		m.historico = m.historico[len(m.historico)-retencaoMemoria:]
	}
	for _, a := range m.assinantes {
		if casaAssuntos(a.assuntos, msg.assunto) {
			a.fila.enfileira(msg)
		}
	}
	return nil
}

// Subscribe entrega o retido ANTES do novo, como o consumidor durável do
// JetStream faz. Sem isso, o worker que sobe depois do relay perderia
// silenciosamente tudo o que já estava publicado — e a espinha de eventos só
// funcionaria com a ordem de boot certa, que é a definição de frágil.
func (m *Memory) Subscribe(ctx context.Context, stream, durable string, subjects []string, h ports.Handler) error {
	if stream != "" && stream != StreamName {
		return errs.NotFound("stream %q", stream)
	}
	if h == nil {
		return errs.Invalid("assinatura sem handler")
	}

	m.mu.Lock()
	if m.fechado {
		m.mu.Unlock()
		return errs.New(errs.KindUnavailable, "barramento em memória já encerrado")
	}
	a := &assinaturaMem{
		durable:  durable,
		assuntos: append([]string(nil), subjects...),
		handler:  h,
		fila:     novaFila(),
		log:      logging.From(ctx).With("consumer", durable),
		ctx:      ctx,
	}
	// Snapshot do histórico sob o MESMO lock do Publish: é o que impede uma
	// publicação concorrente de ser entregue duas vezes ou nenhuma.
	for _, msg := range m.historico {
		if casaAssuntos(a.assuntos, msg.assunto) {
			a.fila.enfileira(msg)
		}
	}
	m.assinantes = append(m.assinantes, a)
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		a.consome()
	}()
	return nil
}

func (m *Memory) Close() error {
	m.mu.Lock()
	if m.fechado {
		m.mu.Unlock()
		return nil
	}
	m.fechado = true
	assinantes := m.assinantes
	m.mu.Unlock()

	for _, a := range assinantes {
		a.fila.fecha()
	}
	m.wg.Wait()
	return nil
}

func (a *assinaturaMem) consome() {
	for {
		msg, ok := a.fila.retira()
		if !ok {
			return
		}
		if a.ctx.Err() != nil {
			return
		}
		var env Envelope
		if err := json.Unmarshal(msg.dados, &env); err != nil {
			// Ilegível nunca melhora com retentativa: descarta com registro,
			// como o Term() do NATS. Uma mensagem venenosa não pode travar a
			// fila para sempre.
			a.log.Error("evento ilegível, descartado", "error", err, "subject", msg.assunto)
			continue
		}
		e := ports.Event{
			ID:          env.ID,
			AccountID:   env.AccountID,
			Aggregate:   env.Aggregate,
			AggregateID: env.AggregateID,
			Type:        env.Type,
			Payload:     msg.dados, // bytes IDÊNTICOS aos publicados
			OccurredAt:  env.OccurredAt,
		}
		if err := a.handler(a.ctx, e); err != nil {
			msg.tentativa++
			if msg.tentativa >= MaxDeliver {
				a.log.Error("evento esgotou as tentativas, indo para a DLQ",
					"error", err, "type", e.Type, "event_id", e.ID, "attempts", msg.tentativa)
				continue
			}
			a.log.Warn("falha ao processar evento, será reentregue",
				"error", err, "type", e.Type, "event_id", e.ID)
			// Reagenda fora da fila: enquanto esta mensagem espera o backoff,
			// as outras continuam andando (é o efeito do Nak no JetStream).
			espera := backoffMemoria[min(msg.tentativa-1, len(backoffMemoria)-1)]
			reagendada := msg
			time.AfterFunc(espera, func() { a.fila.enfileira(reagendada) })
			continue
		}
	}
}

// envelopeDe monta o formato de fio a partir do evento — usado só quando o
// publicador não trouxe envelope pronto.
func envelopeDe(e ports.Event) []byte {
	ocorrido := e.OccurredAt
	if ocorrido.IsZero() {
		ocorrido = time.Now().UTC()
	}
	b, _ := json.Marshal(Envelope{
		ID: e.ID, AccountID: e.AccountID, Aggregate: e.Aggregate,
		AggregateID: e.AggregateID, Type: e.Type, OccurredAt: ocorrido,
	})
	return b
}

// casaAssuntos aplica a semântica de wildcard do NATS: "*" casa UM token,
// ">" casa a cauda (ao menos um token). Lista vazia casa tudo, como
// FilterSubjects vazio no JetStream.
func casaAssuntos(padroes []string, assunto string) bool {
	if len(padroes) == 0 {
		return true
	}
	for _, p := range padroes {
		if casaAssunto(p, assunto) {
			return true
		}
	}
	return false
}

func casaAssunto(padrao, assunto string) bool {
	p := strings.Split(padrao, ".")
	a := strings.Split(assunto, ".")
	for i, tok := range p {
		if tok == ">" {
			return len(a) > i
		}
		if i >= len(a) {
			return false
		}
		if tok != "*" && tok != a[i] {
			return false
		}
	}
	return len(p) == len(a)
}

// filaMem é uma fila ilimitada com sinal.
//
// Canal com buffer fixo não serve: Publish enfileira segurando o lock do
// barramento, e um buffer cheio travaria o publicador — no NATS, publicar
// nunca espera o consumidor.
type filaMem struct {
	mu      sync.Mutex
	cond    *sync.Cond
	itens   []entrega
	fechada bool
}

func novaFila() *filaMem {
	q := &filaMem{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *filaMem) enfileira(e entrega) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fechada {
		return
	}
	q.itens = append(q.itens, e)
	q.cond.Signal()
}

func (q *filaMem) retira() (entrega, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.itens) == 0 && !q.fechada {
		q.cond.Wait()
	}
	if len(q.itens) == 0 {
		return entrega{}, false
	}
	e := q.itens[0]
	q.itens = q.itens[1:]
	return e, true
}

func (q *filaMem) fecha() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fechada = true
	q.cond.Broadcast()
}

var _ ports.EventBus = (*Memory)(nil)
