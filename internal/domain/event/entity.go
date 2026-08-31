// Package event é o domínio da LEITURA ao vivo do log de eventos.
//
// A verdade é o log (ADR-0006); este pacote não a produz, apenas a entrega
// enquanto ela acontece. Quem grava evento é o caso de uso, na mesma transação
// do estado (ADR-0019) — aqui só se lê.
//
// Regra da casa: nada de Postgres, NATS ou gRPC. O que se precisa de fora vem
// como PORTA — Repository (repository.go) e ports.EventBus.
package event

import (
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// O evento em si NÃO ganha tipo novo: ports.Event já é o vocabulário da casa e
// espelha campo a campo a tabela `events`. Inventar um event.Event aqui só
// criaria uma tradução a mais para manter em dia.

// Cursor é a POSIÇÃO de um evento no log.
//
// Vem em par porque a chave primária de `events` é (id, occurred_at) e a tabela
// é particionada por occurred_at: comparar só o id não define ordem nenhuma, e
// comparar só o instante empata entre eventos do mesmo microssegundo. O par
// ordena de forma total e estável — que é o que um cursor precisa ser.
type Cursor struct {
	OccurredAt time.Time
	ID         string
}

// Filter recorta o que interessa ao assinante. Listas vazias = tudo.
//
// O MESMO filtro é aplicado no replay (virando WHERE no Postgres) e no fluxo ao
// vivo (em memória). Precisam concordar: se divergissem, o cliente veria um
// evento na emenda e não veria o irmão dele um segundo depois.
type Filter struct {
	Aggregates []string // demand, project, workspace...
	Types      []string // dop.hierarchy.project.created
}

func (f Filter) Matches(e ports.Event) bool {
	return matchesAny(f.Aggregates, e.Aggregate) && matchesAny(f.Types, e.Type)
}

func matchesAny(allowed []string, v string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == v {
			return true
		}
	}
	return false
}

// belongsTo é o isolamento por conta, em um lugar só.
//
// Evento sem conta (`identity.user.ensured` acontece antes de a conta pessoal
// existir — migração 0003) não pertence a NINGUÉM: não vaza para assinante
// nenhum. Por isso a comparação é por igualdade estrita e a string vazia é
// recusada dos dois lados, em vez de tratada como curinga.
func belongsTo(e ports.Event, accountID string) bool {
	return accountID != "" && e.AccountID == accountID
}

// Emitter entrega um evento ao assinante. Devolver erro encerra o fluxo — é
// assim que a camada gRPC propaga "o cliente foi embora".
type Emitter func(e ports.Event) error
