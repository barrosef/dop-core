// Adaptador de EventBus sobre NATS JetStream (ADR-0019).
//
// Escolha: leve (um container), roda idêntico em k3s e GKE, persistente, com
// consumer groups, DLQ e replay. Kafka seria caminhão para a nossa carga;
// Pub/Sub amarraria ao GCP — fica como segundo adaptador quando alguém quiser
// gerenciado.
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

const (
	StreamName    = "DOP"
	StreamSubject = "dop.>"
	// Após este número de tentativas a mensagem vai para a DLQ em vez de
	// bloquear a fila para sempre.
	MaxDeliver = 5
)

// Envelope é o FORMATO DE FIO do evento: o que o relay publica e o que o
// consumidor recebe. Explícito de propósito — antes ele era implícito e o
// consumidor tentava desserializar em ports.Event, cujo Payload []byte espera
// base64; o envelope tem payload como OBJETO. Silenciava toda entrega.
type Envelope struct {
	ID          string          `json:"id"`
	AccountID   string          `json:"account_id"`
	Aggregate   string          `json:"aggregate"`
	AggregateID string          `json:"aggregate_id"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

type NATS struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	stream jetstream.Stream
}

func NewNATS(ctx context.Context, url string) (*NATS, error) {
	conn, err := nats.Connect(url,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao conectar no NATS")
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao abrir JetStream")
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      StreamName,
		Subjects:  []string{StreamSubject},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		MaxAge:    30 * 24 * time.Hour,
		Discard:   jetstream.DiscardOld,
	})
	if err != nil {
		conn.Close()
		return nil, errs.Wrap(errs.KindUnavailable, err, "falha ao criar o stream")
	}
	return &NATS{conn: conn, js: js, stream: stream}, nil
}

func (n *NATS) Publish(ctx context.Context, e ports.Event) error {
	payload := e.Payload
	if len(payload) == 0 {
		// Sem envelope pronto (quem publica direto, sem passar pelo outbox):
		// monta um. Aqui havia json.Marshal(e) — que é o MESMO bug que o
		// Envelope existe para matar, só do lado do publicador: ports.Event
		// serializa Payload []byte em base64 e com nomes de campo que o
		// consumidor não sabe ler, então AccountID, AggregateID e OccurredAt
		// chegavam vazios do outro lado, sem erro nenhum.
		payload = envelopeDe(e)
	}
	// MsgId dá desduplicação no lado do broker: o relay pode republicar sem
	// gerar entrega dupla dentro da janela de dedup do JetStream.
	_, err := n.js.PublishMsg(ctx, &nats.Msg{
		Subject: e.Type,
		Data:    payload,
		Header:  nats.Header{jetstream.MsgIDHeader: []string{e.ID}},
	})
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao publicar no NATS")
	}
	return nil
}

// Subscribe cria um consumidor durável. O handler DEVE ser idempotente: a
// entrega é ao-menos-uma-vez.
func (n *NATS) Subscribe(ctx context.Context, stream, durable string, subjects []string, h ports.Handler) error {
	if stream == "" {
		stream = StreamName
	}
	cons, err := n.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:        durable,
		FilterSubjects: subjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		MaxDeliver:     MaxDeliver,
		AckWait:        30 * time.Second,
		BackOff:        []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, time.Minute},
	})
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao criar consumidor %q", durable)
	}

	log := logging.From(ctx).With("consumer", durable)
	_, err = cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			// Mensagem ilegível nunca melhora com retry — descarta com registro.
			log.Error("evento ilegível, descartado", "error", err, "subject", msg.Subject())
			_ = msg.Term()
			return
		}
		// O handler recebe os campos já desembrulhados; Payload carrega o
		// envelope inteiro, para quem quiser o dado cru.
		e := ports.Event{
			ID:          env.ID,
			AccountID:   env.AccountID,
			Aggregate:   env.Aggregate,
			AggregateID: env.AggregateID,
			Type:        env.Type,
			Payload:     msg.Data(),
			OccurredAt:  env.OccurredAt,
		}
		if err := h(ctx, e); err != nil {
			md, _ := msg.Metadata()
			if md != nil && md.NumDelivered >= MaxDeliver {
				log.Error("evento esgotou as tentativas, indo para a DLQ",
					"error", err, "type", e.Type, "event_id", e.ID,
					"attempts", md.NumDelivered)
				_ = msg.Term()
				return
			}
			log.Warn("falha ao processar evento, será reentregue",
				"error", err, "type", e.Type, "event_id", e.ID)
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "falha ao consumir %q", durable)
	}
	return nil
}

func (n *NATS) Close() error {
	if n.conn != nil {
		return n.conn.Drain()
	}
	return nil
}

var _ ports.EventBus = (*NATS)(nil)

func StreamInfo(ctx context.Context, n *NATS) (string, error) {
	info, err := n.stream.Info(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("stream=%s msgs=%d bytes=%d consumidores=%d",
		info.Config.Name, info.State.Msgs, info.State.Bytes, info.State.Consumers), nil
}
