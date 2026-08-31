package execution

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// Repository é a PORTA de persistência do substrato.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
//
// Toda operação recebe accountID explicitamente e DEVE filtrá-lo no WHERE:
// isolamento multi-tenant é parâmetro obrigatório da porta, não algo que o
// adaptador possa esquecer.
//
// Cada método de escrita grava estado e evento na MESMA transação (ADR-0019) —
// e é por isso que não existe um "SaveSandbox" genérico aqui: uma escrita
// genérica não sabe qual evento emitir, e o evento acabaria sendo publicado
// fora da transação por quem chamou.
type Repository interface {
	// ByID devolve (nil, nil) quando não há linha: se a ausência é erro, quem
	// decide é o domínio.
	ByID(ctx context.Context, accountID, id string) (*Sandbox, error)
	// ByIdempotencyKey resolve a REPETIÇÃO de um provisionamento: mesma chave,
	// mesmo sandbox. Sem isso, um retry de rede duplicaria microVM.
	ByIdempotencyKey(ctx context.Context, accountID, key string) (*Sandbox, error)
	// LiveByDemand devolve o sandbox NÃO destruído da demanda, se houver.
	// Uma demanda ativa tem um sandbox (spec §1) — a unicidade é garantida por
	// índice parcial no banco, e esta consulta existe para responder antes de
	// tentar violar a restrição.
	LiveByDemand(ctx context.Context, accountID, demandID string) (*Sandbox, error)

	// Create grava o sandbox em provisioning e emite o evento de início.
	Create(ctx context.Context, s *Sandbox) (*Sandbox, error)
	// MarkProvisioned confirma o que o substrato ENTREGOU: tier e endpoints
	// reais. O tier vem de volta porque ele é declarado, nunca presumido — e a
	// linha precisa registrar o que o cliente de fato recebeu.
	MarkProvisioned(ctx context.Context, accountID, id string, tier ports.IsolationTier, endpoints []Endpoint) (*Sandbox, error)
	// Transition aplica uma mudança de estado e emite o evento correspondente.
	// Recebe a Transition inteira, não só o estado destino, para que o evento
	// carregue se o trabalho foi preservado ou perdido — é a pergunta que a
	// auditoria vai fazer depois.
	Transition(ctx context.Context, accountID, id string, t Transition) (*Sandbox, error)
	// TouchActivity registra uso e adia a suspensão por ociosidade. Não emite
	// evento: batimento de atividade em log de eventos é ruído que afogaria o
	// dossiê da demanda.
	TouchActivity(ctx context.Context, accountID, id string) error

	// ListIdle alimenta o varredor de economia: ativos parados desde antes do
	// corte. Filtra por conta como todo o resto.
	ListIdle(ctx context.Context, accountID string, olderThanSeconds int) ([]Sandbox, error)

	// AccountsWithIdle é a ÚNICA consulta deste domínio que atravessa contas, e
	// existe por um motivo estrutural: o varredor de economia roda no
	// scheduler, que é ator de SISTEMA e não tem conta ativa — enquanto todo o
	// resto do domínio exige uma.
	//
	// A saída dela não é dado de conta nenhuma: é a lista de contas que TÊM o
	// que varrer. Cada varredura continua acontecendo dentro de UMA conta, com
	// ela no contexto, então o isolamento não é afrouxado — o que muda é só
	// quem decide a ordem de visita.
	//
	// Sem isso, ou o scheduler ganharia acesso irrestrito, ou sandbox ocioso
	// nunca suspenderia. A spec do substrato é explícita sobre o custo do
	// segundo caso: "sandbox ocioso é o que separa paralelismo real de máquina
	// afogada".
	AccountsWithIdle(ctx context.Context, olderThanSeconds int) ([]string, error)
}

// Access é a porta ESTREITA para o domínio de identidade: o substrato precisa
// de UMA coisa sobre quem chama — o papel na conta ativa, porque provisionar
// custa dinheiro e viewer não gasta o dinheiro da conta.
//
// A superfície foi escolhida para que *identity.Service a satisfaça como está:
// o composition root apenas liga, sem adaptador de cola.
type Access interface {
	Authorize(ctx context.Context, userID, accountID string) (*identity.Membership, error)
}

// Demands é a porta ESTREITA para o domínio de demanda.
//
// O substrato precisa saber DUAS coisas antes de gastar uma microVM: a demanda
// existe, e ela é da conta ativa. Nada além — nem estágio, nem thread, nem
// card. Declarada aqui, e não importada do pacote de demanda, porque a
// dependência é do substrato para a demanda e não o contrário: quem executa
// conhece o que executa, e inverter isso amarraria os dois domínios num ciclo.
type Demands interface {
	// DemandAccount devolve a conta dona da demanda.
	//
	// Demanda inexistente e demanda de OUTRA conta devolvem o mesmo erro
	// (KindNotFound) de propósito: distinguir os dois casos vazaria a
	// existência de ids de outras contas para quem ficar tentando.
	DemandAccount(ctx context.Context, demandID string) (string, error)
}
