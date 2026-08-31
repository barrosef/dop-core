package event

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// Repository é a PORTA de leitura do log de eventos.
//
// Superfície mínima de propósito: o replay precisa de exatamente duas coisas —
// achar onde o cliente parou e ler o que veio depois. Tudo mais (agregação,
// contagem, dossiê) é projeção e não entra aqui.
//
// Toda operação recebe accountID e DEVE filtrar por ele dentro do WHERE:
// isolamento multi-tenant é constraint, não convenção.
type Repository interface {
	// Locate devolve a posição do evento no log.
	//
	// Recebe accountID porque o cursor também é superfície de ataque: id de
	// evento de outra conta tem que devolver "não encontrado", nunca a posição
	// — senão a existência do id vaza.
	Locate(ctx context.Context, accountID, eventID string) (Cursor, error)

	// EventsAfter devolve, em ordem de acontecimento, até `limit` eventos da
	// conta posteriores a `after` que casem com o filtro.
	EventsAfter(ctx context.Context, accountID string, after Cursor, f Filter, limit int) ([]ports.Event, error)
}
