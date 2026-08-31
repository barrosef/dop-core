package event

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

const (
	// watcherBuffer é a POLÍTICA DE CONSUMIDOR LENTO, em um número.
	//
	// Cada assinante tem uma fila própria e limitada. Se ela encher, o servidor
	// NÃO espera: derruba aquele assinante com Unavailable e segue. A
	// alternativa — bloquear na entrega — faria um cockpit num wifi ruim travar
	// o fan-out de todo mundo, inclusive do broker (o handler roda na goroutine
	// de consumo do JetStream). Perder um cliente lento é barato; ele reconecta
	// mandando since_event_id e não perde nada. Travar o servidor não é.
	watcherBuffer = 256

	// replayPage / maxReplay limitam o custo do replay.
	//
	// Cursor muito antigo NÃO é caso deste serviço: histórico profundo se lê da
	// projeção de timeline, paginada, que existe exatamente para isso. Aqui o
	// replay serve para emendar uma reconexão, não para reconstruir o mundo.
	replayPage = 500
	maxReplay  = 5000
)

// Service entrega o log de eventos ao vivo. Recebe apenas PORTAS.
//
// Há UMA assinatura no barramento por processo, não uma por cliente: o fan-out
// para os assinantes é feito em memória. Assinar por cliente criaria um
// consumidor no broker a cada aba aberta do cockpit — e a porta EventBus nem
// oferece como desfazer isso.
type Service struct {
	repo  Repository
	bus   ports.EventBus
	clock ports.Clock

	startOnce sync.Once
	startErr  error
	started   atomic.Bool
	// startedAt corta o passado do fluxo ao vivo — ver fanout.
	startedAt time.Time

	mu       sync.Mutex
	watchers map[*watcher]struct{}
}

func NewService(repo Repository, bus ports.EventBus, clock ports.Clock) *Service {
	return &Service{repo: repo, bus: bus, clock: clock, watchers: map[*watcher]struct{}{}}
}

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now().UTC()
}

// Start abre a assinatura única no barramento. Idempotente.
//
// O ctx é o do PROCESSO, não o de um cliente: a assinatura precisa sobreviver a
// todo Watch que entra e sai. Quem chama é o composition root.
//
// `consumer` precisa ser único POR PROCESSO — não por serviço. Duas réplicas de
// `serve` compartilhando o mesmo nome viram um consumer group: o evento cai em
// uma das duas e os clientes da outra nunca o veem. Vazio = nome sorteado.
//
// `subjects` é decidido pelo composition root, como já acontece em
// RegisterProjections; o domínio não escreve sintaxe de assunto do broker.
func (s *Service) Start(ctx context.Context, consumer string, subjects []string) error {
	s.startOnce.Do(func() {
		if consumer == "" {
			consumer = "event-stream-" + randomSuffix(8)
		}
		s.startedAt = s.now()
		s.startErr = s.bus.Subscribe(ctx, "", consumer, subjects, s.fanout)
		s.started.Store(s.startErr == nil)
	})
	return s.startErr
}

// Watch entrega os eventos da conta ativa conforme acontecem.
//
// Se sinceEventID vier preenchido, primeiro drena do log o que veio depois dele
// e só então emenda no fluxo ao vivo. A ordem das operações abaixo é a parte
// que importa — ver o comentário da emenda.
func (s *Service) Watch(ctx context.Context, sinceEventID string, f Filter, emit Emitter) error {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	if !s.started.Load() {
		return errs.New(errs.KindUnavailable, "fluxo de eventos não iniciado")
	}

	// ── A EMENDA ─────────────────────────────────────────────────────────────
	// Assina ANTES de ler o banco. É o que fecha o buraco.
	//
	// Todo evento é publicado DEPOIS de commitado (o relay lê o outbox já
	// gravado). Logo, para um evento qualquer: ou ele foi publicado antes de nós
	// assinarmos — e então commitou antes disso, e portanto antes da leitura do
	// banco, que vem depois: está no replay — ou foi publicado depois de
	// assinarmos, e está na fila deste watcher. Não existe terceira hipótese:
	// nada se perde.
	//
	// O preço é o inverso: o que commitou antes da leitura e só foi publicado
	// depois da assinatura aparece nos DOIS. Por isso guardamos os ids do replay
	// e descartamos a repetição quando ela chegar pela fila. O conjunto é
	// limitado por maxReplay, então cabe na memória e vale para o fluxo inteiro
	// — inclusive para uma republicação tardia do relay.
	w := s.register(accountID, f)
	defer s.unregister(w)

	seen := map[string]struct{}{}
	if sinceEventID != "" {
		if err := s.replay(ctx, accountID, sinceEventID, f, emit, seen); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Cliente desconectou: o defer acima tira o watcher do fan-out e a
			// fila é coletada com ele. Nada fica rodando.
			return nil
		case <-w.overflow:
			return errs.New(errs.KindUnavailable,
				"assinante lento demais: reconecte com since_event_id do último evento recebido")
		case e := <-w.ch:
			if _, dup := seen[e.ID]; dup {
				continue // já saiu no replay
			}
			if err := emit(e); err != nil {
				return err
			}
		}
	}
}

// replay drena do log tudo o que veio depois do cursor, paginado.
func (s *Service) replay(ctx context.Context, accountID, sinceEventID string, f Filter, emit Emitter, seen map[string]struct{}) error {
	cur, err := s.repo.Locate(ctx, accountID, sinceEventID)
	if err != nil {
		return err
	}
	for {
		batch, err := s.repo.EventsAfter(ctx, accountID, cur, f, replayPage)
		if err != nil {
			return err
		}
		for i := range batch {
			e := batch[i]
			if err := emit(e); err != nil {
				return err
			}
			seen[e.ID] = struct{}{}
			cur = Cursor{OccurredAt: e.OccurredAt, ID: e.ID}
		}
		if len(batch) < replayPage {
			return nil // alcançou o fim do log
		}
		if len(seen) >= maxReplay {
			// Recusar é mais honesto do que emendar em silêncio no meio do
			// histórico: o cliente ficaria com um buraco sem saber.
			return errs.Precondition(
				"cursor antigo demais para replay ao vivo (limite de %d eventos); "+
					"leia o histórico pela timeline e reconecte com um cursor recente", maxReplay)
		}
	}
}

// ── fan-out ──────────────────────────────────────────────────────────────────

type watcher struct {
	accountID string
	filter    Filter
	ch        chan ports.Event
	overflow  chan struct{}
	once      sync.Once
}

// offer nunca bloqueia: roda na goroutine de consumo do barramento, que é
// compartilhada por TODOS os assinantes.
func (w *watcher) offer(e ports.Event) {
	select {
	case w.ch <- e:
	default:
		w.once.Do(func() { close(w.overflow) })
	}
}

func (s *Service) register(accountID string, f Filter) *watcher {
	w := &watcher{
		accountID: accountID,
		filter:    f,
		ch:        make(chan ports.Event, watcherBuffer),
		overflow:  make(chan struct{}),
	}
	s.mu.Lock()
	s.watchers[w] = struct{}{}
	s.mu.Unlock()
	return w
}

func (s *Service) unregister(w *watcher) {
	s.mu.Lock()
	delete(s.watchers, w)
	s.mu.Unlock()
	// A fila NÃO é fechada aqui: o fan-out pode estar no meio de um offer. Sem
	// referência, ela é coletada — fechar só criaria corrida por nada.
}

// fanout é o handler da assinatura única. Devolve sempre nil: entregar ao
// cockpit é best-effort e um cliente lento não pode fazer o barramento
// reentregar o evento para todo mundo.
func (s *Service) fanout(_ context.Context, e ports.Event) error {
	// A porta EventBus GARANTE (garantia 6) que o que foi publicado antes da
	// assinatura é entregue quando o durável aparece — retenção, para que a
	// espinha de eventos não dependa da ordem de boot. Para uma projeção isso é
	// exatamente o certo; para um tail ao vivo é exatamente o errado: todo start
	// do processo despejaria o histórico retido no broker em cima de quem
	// estivesse ouvindo, estourando a fila de todos.
	//
	// Então o corte é feito AQUI, onde se sabe o que o tail significa: ao vivo é
	// o que aconteceu depois de ele existir; o passado é assunto do replay pelo
	// Postgres, que sabe filtrar por conta e paginar. Nada se perde na emenda
	// porque todo evento commitado depois da leitura do banco necessariamente
	// ocorreu depois do Start.
	if e.OccurredAt.Before(s.startedAt) {
		return nil
	}
	e = unwrapEnvelope(e)

	s.mu.Lock()
	targets := make([]*watcher, 0, len(s.watchers))
	for w := range s.watchers {
		if belongsTo(e, w.accountID) && w.filter.Matches(e) {
			targets = append(targets, w)
		}
	}
	s.mu.Unlock()

	for _, w := range targets {
		w.offer(e)
	}
	return nil
}

// unwrapEnvelope reduz Payload ao payload de NEGÓCIO.
//
// O evento que chega pelo barramento traz, em Payload, o envelope inteiro (id,
// account_id, type, payload...); os demais campos de ports.Event já vêm
// desembrulhados pelo adaptador. O evento que vem do Postgres traz o payload
// puro. Sem normalizar aqui, o MESMO evento chegaria ao cliente com duas formas
// diferentes conforme tivesse passado pelo replay ou pelo tail — e o cliente
// não tem como saber por onde ele veio.
//
// A checagem é estrita (id do envelope igual ao id do evento) para não confundir
// com um payload de negócio que por acaso tenha um campo "payload".
func unwrapEnvelope(e ports.Event) ports.Event {
	var env struct {
		ID      string          `json:"id"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(e.Payload, &env); err != nil {
		return e
	}
	if env.ID != "" && env.ID == e.ID && len(env.Payload) > 0 {
		e.Payload = env.Payload
	}
	return e
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
