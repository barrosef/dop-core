package attention

import "context"

// Repository é a leitura da caixa. Não há Create nem Resolve: item nasce e
// morre de EVENTO (ADR-0006), e uma porta que oferecesse escrita direta seria
// um convite a criar item à mão — e a caixa deixaria de ser projeção.
type Repository interface {
	// List devolve os itens da conta, mais urgentes primeiro. `demandID` vazio
	// traz a conta inteira, que é a visão da caixa; preenchido, a de uma
	// demanda só.
	List(ctx context.Context, accountID, demandID string, includeResolved bool, limit int) ([]Item, error)

	// OpenTotal é o número do badge. Existe separado do List porque contar a
	// PÁGINA daria um badge que muda conforme o dev pagina.
	OpenTotal(ctx context.Context, accountID string) (int, error)
}

// Watcher é a assinatura do log — a mesma máquina de fan-out do domínio de
// evento, recebida como porta para que a caixa não saiba de onde ela vem.
type Watcher interface {
	Watch(ctx context.Context, sinceEventID string, aggregates, types []string, emit func(EventNotice) error) error
}

// EventNotice é o evento cru chegando ao serviço, já com a conta resolvida.
type EventNotice = Event
