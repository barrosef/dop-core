package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/eventbus"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// EventBusSuite verifica as nove garantias documentadas na porta.
//
// A garantia 1 é a razão de esta suíte existir: o formato de fio é o ENVELOPE, e
// um adaptador que reserialize ports.Event manda Payload []byte em base64. O
// consumidor não reconhece, descarta tudo e NÃO RECLAMA — o sistema fica mudo e
// verde ao mesmo tempo. Só um teste que compara os BYTES pega isso.
//
// Todo assunto e todo ID de evento são únicos por subtestе: contra um broker de
// verdade a suíte compartilha stream com as execuções anteriores, e o JetStream
// desduplica por ID.
func EventBusSuite(t *testing.T, name string, newBus func(t *testing.T) ports.EventBus) {
	t.Run(name, func(t *testing.T) {
		t.Run("1_payload_chega_byte_a_byte_e_campos_vem_do_envelope", func(t *testing.T) {
			bus := newBus(t)
			assunto := assuntoUnico("intacto")
			c := novoColetor()
			assinar(t, bus, assunto, c.handler)

			dados := envelopeJSON(eventoUnico(), assunto, `{"quantidade":42,"texto":"acentuação"}`)
			publicar(t, bus, assunto, dados)

			c.esperar(t, 1, "o evento publicado nunca chegou")
			got := c.eventos()[0]

			if !bytes.Equal(got.Payload, dados) {
				t.Fatalf("os bytes do envelope não sobreviveram ao transporte.\n"+
					"publicado: %s\nrecebido:  %s\n"+
					"(payload em base64 do outro lado = alguém reserializou ports.Event)",
					dados, got.Payload)
			}
			var publicado, recebido eventbus.Envelope
			_ = json.Unmarshal(dados, &publicado)
			if err := json.Unmarshal(got.Payload, &recebido); err != nil {
				t.Fatalf("o payload entregue não é o envelope JSON: %v", err)
			}
			if string(recebido.Payload) != string(publicado.Payload) {
				t.Fatalf("o dado de negócio dentro do envelope mudou: %s != %s",
					recebido.Payload, publicado.Payload)
			}
			// Os campos do evento entregue vêm do envelope, não do struct publicado.
			if got.ID != publicado.ID || got.AccountID != publicado.AccountID ||
				got.Aggregate != publicado.Aggregate || got.AggregateID != publicado.AggregateID ||
				got.Type != publicado.Type {
				t.Fatalf("campos desembrulhados divergem do envelope:\nrecebido: %+v\nenvelope: %+v", got, publicado)
			}
			if !got.OccurredAt.Equal(publicado.OccurredAt) {
				t.Errorf("OccurredAt divergente: %v != %v", got.OccurredAt, publicado.OccurredAt)
			}
		})

		t.Run("2_publish_sem_payload_preserva_identificacao", func(t *testing.T) {
			bus := newBus(t)
			ctx := context.Background()
			assunto := assuntoUnico("sem-payload")
			c := novoColetor()
			assinar(t, bus, assunto, c.handler)

			e := ports.Event{
				ID: eventoUnico(), AccountID: "acct-1", Aggregate: "conta",
				AggregateID: "ag-1", Type: assunto, OccurredAt: agoraJSON(),
			}
			if err := bus.Publish(ctx, e); err != nil {
				t.Fatalf("Publish: %v", err)
			}

			c.esperar(t, 1, "evento sem payload nunca chegou")
			got := c.eventos()[0]
			// É aqui que json.Marshal(ports.Event) se denuncia: os nomes de
			// campo do struct não batem com os do envelope e tudo chega vazio.
			if got.ID != e.ID || got.AccountID != e.AccountID ||
				got.Aggregate != e.Aggregate || got.AggregateID != e.AggregateID || got.Type != e.Type {
				t.Fatalf("identificação perdida no caminho:\nrecebido: %+v\npublicado: %+v", got, e)
			}
			var env eventbus.Envelope
			if err := json.Unmarshal(got.Payload, &env); err != nil {
				t.Fatalf("o fallback deveria montar um envelope JSON, veio: %s", got.Payload)
			}
		})

		t.Run("3_filtra_por_assunto", func(t *testing.T) {
			bus := newBus(t)
			base := assuntoUnico("filtro")
			alvo := base + ".alvo"
			outro := base + ".outro"
			c := novoColetor()
			assinar(t, bus, base+".alvo.>", c.handler)

			publicar(t, bus, outro+".um", envelopeJSON(eventoUnico(), outro+".um", `{}`))
			publicar(t, bus, alvo+".um", envelopeJSON(eventoUnico(), alvo+".um", `{}`))

			c.esperar(t, 1, "o evento do assunto assinado nunca chegou")
			// Margem para o intruso aparecer, se o filtro estiver furado.
			time.Sleep(500 * time.Millisecond)
			for _, e := range c.eventos() {
				if e.Type != alvo+".um" {
					t.Fatalf("chegou evento de assunto não assinado: %q", e.Type)
				}
			}
		})

		t.Run("4_entrega_o_que_foi_publicado_antes_da_assinatura", func(t *testing.T) {
			bus := newBus(t)
			assunto := assuntoUnico("retido")
			id := eventoUnico()

			// Publica ANTES de existir assinante: é a ordem real quando o relay
			// sobe antes do worker de projeção.
			publicar(t, bus, assunto, envelopeJSON(id, assunto, `{}`))

			c := novoColetor()
			assinar(t, bus, assunto, c.handler)
			c.esperar(t, 1, "o evento publicado antes da assinatura se perdeu — "+
				"a espinha de eventos dependeria da ordem de boot")
			if got := c.eventos()[0].ID; got != id {
				t.Fatalf("chegou outro evento: %q != %q", got, id)
			}
		})

		t.Run("5_erro_no_handler_provoca_reentrega", func(t *testing.T) {
			bus := newBus(t)
			assunto := assuntoUnico("reentrega")
			var tentativas atomic.Int64
			assinar(t, bus, assunto, func(_ context.Context, e ports.Event) error {
				if tentativas.Add(1) == 1 {
					return fmt.Errorf("falha proposital na primeira entrega")
				}
				return nil
			})

			publicar(t, bus, assunto, envelopeJSON(eventoUnico(), assunto, `{}`))
			esperarAte(t, 30*time.Second, func() bool { return tentativas.Load() >= 2 },
				"o handler falhou e o evento NÃO foi reentregue — entrega ao-menos-uma-vez quebrada")
		})

		t.Run("6_sucesso_nao_provoca_reentrega_imediata", func(t *testing.T) {
			bus := newBus(t)
			assunto := assuntoUnico("ack")
			c := novoColetor()
			assinar(t, bus, assunto, c.handler)

			publicar(t, bus, assunto, envelopeJSON(eventoUnico(), assunto, `{}`))
			c.esperar(t, 1, "o evento nunca chegou")
			// Janela curta: pega o adaptador que reentrega em laço quente.
			// Reentrega tardia (AckWait do JetStream) está fora do alcance de um
			// teste rápido, e a porta permite duplicata de qualquer forma.
			time.Sleep(1 * time.Second)
			if n := len(c.eventos()); n > 1 {
				t.Fatalf("entrega bem-sucedida foi repetida %d vezes em 1s — o ack não está acontecendo", n)
			}
		})

		t.Run("7_mensagem_ilegivel_nao_trava_a_fila", func(t *testing.T) {
			bus := newBus(t)
			assunto := assuntoUnico("veneno")
			c := novoColetor()
			assinar(t, bus, assunto, c.handler)

			// Bytes que não são JSON: nenhum número de retentativas conserta.
			publicar(t, bus, assunto, []byte("{isto não é json"))
			bom := eventoUnico()
			publicar(t, bus, assunto, envelopeJSON(bom, assunto, `{}`))

			c.esperar(t, 1, "a mensagem ilegível travou a fila: o evento seguinte não chegou")
			if got := c.eventos()[0].ID; got != bom {
				t.Fatalf("o handler recebeu algo inesperado: %q", got)
			}
		})

		t.Run("8_publish_concorrente", func(t *testing.T) {
			bus := newBus(t)
			ctx := context.Background()
			assunto := assuntoUnico("concorrente")
			c := novoColetor()
			assinar(t, bus, assunto, c.handler)

			const total = 50
			ids := make(map[string]bool, total)
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := 0; i < total; i++ {
				id := eventoUnico()
				mu.Lock()
				ids[id] = true
				mu.Unlock()
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					if err := bus.Publish(ctx, ports.Event{
						ID: id, Type: assunto, Payload: envelopeJSON(id, assunto, `{}`),
					}); err != nil {
						t.Errorf("Publish concorrente: %v", err)
					}
				}(id)
			}
			wg.Wait()

			esperarAte(t, 30*time.Second, func() bool { return len(c.distintos()) >= total },
				fmt.Sprintf("dos %d eventos publicados em paralelo, chegaram %d", total, len(c.distintos())))
			// Duplicata é permitida (ao-menos-uma-vez); perda não é.
			for id := range ids {
				if !c.distintos()[id] {
					t.Fatalf("evento %s se perdeu", id)
				}
			}
		})

		t.Run("9_evento_sem_tipo_e_recusado", func(t *testing.T) {
			bus := newBus(t)
			if err := bus.Publish(context.Background(), ports.Event{ID: eventoUnico()}); err == nil {
				t.Fatal("evento sem Type não tem assunto para onde ir e deveria ser recusado")
			}
		})

		t.Run("10_close_encerra_sem_erro", func(t *testing.T) {
			bus := newBus(t)
			if err := bus.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// ── auxiliares da suíte ──────────────────────────────────────────────────────

var eventoSeq atomic.Int64

// assuntoUnico devolve um assunto sob "dop." (o stream do JetStream só aceita
// dop.>), único por execução: o stream é persistente e sobrevive ao teste.
func assuntoUnico(rotulo string) string {
	return fmt.Sprintf("dop.contrato.%s.%d-%d", rotulo, time.Now().UnixNano(), eventoSeq.Add(1))
}

// eventoUnico devolve um ID novo — o JetStream desduplica por ID dentro de uma
// janela, então reaproveitar ID faria o segundo evento sumir "sozinho".
func eventoUnico() string {
	return fmt.Sprintf("evt-%d-%d", time.Now().UnixNano(), eventoSeq.Add(1))
}

// agoraJSON trunca para milissegundos: o instante precisa sobreviver ao
// RFC3339 do JSON sem perder igualdade na volta.
func agoraJSON() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func envelopeJSON(id, assunto, payload string) []byte {
	b, err := json.Marshal(eventbus.Envelope{
		ID: id, AccountID: "acct-contrato", Aggregate: "contrato", AggregateID: "ag-" + id,
		Type: assunto, Payload: json.RawMessage(payload), OccurredAt: agoraJSON(),
	})
	if err != nil {
		panic(err)
	}
	return b
}

func assinar(t *testing.T, bus ports.EventBus, assunto string, h ports.Handler) {
	t.Helper()
	durable := fmt.Sprintf("contrato-%d-%d", time.Now().UnixNano(), eventoSeq.Add(1))
	if err := bus.Subscribe(context.Background(), "", durable, []string{assunto}, h); err != nil {
		t.Fatalf("Subscribe(%s): %v", assunto, err)
	}
}

func publicar(t *testing.T, bus ports.EventBus, assunto string, dados []byte) {
	t.Helper()
	var env eventbus.Envelope
	_ = json.Unmarshal(dados, &env)
	id := env.ID
	if id == "" {
		id = eventoUnico()
	}
	if err := bus.Publish(context.Background(), ports.Event{ID: id, Type: assunto, Payload: dados}); err != nil {
		t.Fatalf("Publish(%s): %v", assunto, err)
	}
}

type coletor struct {
	mu   sync.Mutex
	vist []ports.Event
}

func novoColetor() *coletor { return &coletor{} }

func (c *coletor) handler(_ context.Context, e ports.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vist = append(c.vist, e)
	return nil
}

func (c *coletor) eventos() []ports.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.Event(nil), c.vist...)
}

func (c *coletor) distintos() map[string]bool {
	out := map[string]bool{}
	for _, e := range c.eventos() {
		out[e.ID] = true
	}
	return out
}

func (c *coletor) esperar(t *testing.T, n int, msg string) {
	t.Helper()
	esperarAte(t, 30*time.Second, func() bool { return len(c.eventos()) >= n }, msg)
}

// esperarAte substitui sleep fixo: a entrega é assíncrona nos dois adaptadores,
// e prazo generoso com verificação frequente é rápido quando passa e claro
// quando falha.
func esperarAte(t *testing.T, prazo time.Duration, ok func() bool, msg string) {
	t.Helper()
	limite := time.Now().Add(prazo)
	for {
		if ok() {
			return
		}
		if time.Now().After(limite) {
			t.Fatalf("%s (prazo de %s esgotado)", msg, prazo)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
