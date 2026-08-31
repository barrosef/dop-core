package cost

import (
	"context"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentra as regras de custo. Recebe apenas PORTAS.
type Service struct {
	repo   Repository
	clock  ports.Clock
	router *Router
}

// NewService exige um relógio e aceita roteador nulo.
//
// A assimetria é deliberada. O relógio é PORTA: aceitar nil faz o serviço cair
// em time.Now() por dentro, nenhum teste de período fica determinístico e
// ninguém percebe que a abstração não está provada — panic aqui é erro de
// montagem, detectado no boot. O roteador é POLÍTICA deste pacote, com padrão
// escrito na ADR-0011; nil apenas escolhe esse padrão, sem desligar nada.
func NewService(repo Repository, clock ports.Clock, router *Router) *Service {
	if clock == nil {
		panic("cost.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	if router == nil {
		router = NewRouter(nil)
	}
	return &Service{repo: repo, clock: clock, router: router}
}

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

// RecordOutcome é o resultado de registrar consumo.
//
// BudgetExceeded é ESTADO, não transição: a repetição de uma chamada devolve o
// mesmo aviso que a original, porque quem pergunta "posso seguir?" precisa da
// resposta certa mesmo quando a escrita não aconteceu de novo.
type RecordOutcome struct {
	Usage          *UsageEvent
	Duplicate      bool
	BudgetExceeded bool
	// Exceeded são os escopos estourados — é o que a caixa de atenção mostra
	// para o humano decidir (aumentar o teto, cortar escopo, encerrar).
	Exceeded []Budget
}

// RecordUsage registra consumo de modelo.
//
// Duas coisas que este método NÃO faz, e as duas são a decisão:
//
//   - não recusa a escrita por orçamento estourado. Medição que falha quando o
//     orçamento acaba é medição que some justo quando mais importa, e o corte
//     da ADR-0011 §2 é SUAVE: a demanda pausa e pergunta, nunca morre no meio
//     nem é cortada em silêncio. Quem pausa é o consumidor de
//     `dop.cost.budget.exceeded`; o custo só avisa;
//   - não deduz nem inventa a chave de idempotência. Ela é obrigatória aqui —
//     em outras escritas o interceptador (ADR-0017) protege e a UNIQUE do
//     banco é o último anteparo, mas neste caso a duplicata não colide com
//     nada: entraria como consumo legítimo e o orçamento viraria ficção.
func (s *Service) RecordUsage(ctx context.Context, u UsageEvent, idempotencyKey string) (*RecordOutcome, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return nil, errs.Invalid("registro de uso exige chave de idempotência")
	}

	// A conta vem do contexto, não do corpo: aceitar a do corpo permitiria
	// lançar consumo na conta do vizinho.
	u.AccountID = accountID
	if u.Currency == "" {
		u.Currency = DefaultCurrency
	}
	if u.At.IsZero() {
		u.At = s.now()
	}
	u.At = u.At.UTC()
	if err := u.Validate(); err != nil {
		return nil, err
	}

	res, err := s.repo.RecordUsage(ctx, &u, idempotencyKey)
	if err != nil {
		return nil, err
	}

	out := &RecordOutcome{Usage: res.Usage, Duplicate: res.Duplicate}
	for _, st := range res.Budgets {
		if st.After.Exceeded() {
			out.BudgetExceeded = true
			out.Exceeded = append(out.Exceeded, st.After)
		}
	}
	return out, nil
}

// GetBudget devolve o orçamento do escopo. Escopo sem teto definido devolve
// limite zero com o gasto real — ausência de orçamento é resposta, não erro.
func (s *Service) GetBudget(ctx context.Context, scope Scope, scopeID string) (*Budget, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	scope, scopeID, err = s.normalizeScope(accountID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return s.repo.BudgetOf(ctx, accountID, scope, scopeID)
}

// SetBudget define o teto do escopo, preservando o acumulado.
//
// Rebaixar o teto abaixo do gasto corrente é permitido e é um estouro: o
// evento sai na mesma transação da escrita, e a demanda pausa pelo mesmo
// caminho de sempre. Impedir o rebaixamento seria pior — o operador que
// descobre que uma demanda está queimando dinheiro precisa poder fechar a
// torneira sem esperar o próximo turno.
func (s *Service) SetBudget(ctx context.Context, b Budget) (*Budget, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	scope, scopeID, err := s.normalizeScope(accountID, b.Scope, b.ScopeID)
	if err != nil {
		return nil, err
	}
	if b.LimitMicros < 0 {
		return nil, errs.Invalid("limite não pode ser negativo (zero = sem teto)")
	}
	b.AccountID = accountID
	b.Scope, b.ScopeID = scope, scopeID
	if b.Currency == "" {
		b.Currency = DefaultCurrency
	}
	b.UpdatedAt = s.now()

	st, err := s.repo.SetBudget(ctx, &b)
	if err != nil {
		return nil, err
	}
	return &st.After, nil
}

// RouteModel devolve a decisão tarefa→(modelo, effort) com a justificativa.
//
// É função PURA do tipo de trabalho, e continua sendo de propósito. demandID
// entra no contrato (e é aceito aqui) porque a calibração de P-7 vai querer
// correlacionar decisão e gasto por demanda — mas ele NÃO participa da
// decisão hoje, e fingir que participa esconderia que a política ainda é a
// tabela crua da ADR-0011.
//
// Em particular, orçamento apertado não rebaixa o modelo: rebaixar sob pressão
// de custo atropelaria a regra fixa de que não se economiza no crítico, e o
// mecanismo de corte da ADR-0011 §2 é pausar e perguntar, não degradar em
// silêncio.
func (s *Service) RouteModel(ctx context.Context, taskKind TaskKind, demandID string) (*Decision, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return nil, err
	}
	d, err := s.router.Route(taskKind)
	if err != nil {
		return nil, errs.Invalid("%v", err)
	}
	return &d, nil
}

// RoutingTable expõe a política inteira para auditoria e para a tela de
// calibração de P-7 — quem vai recalibrar precisa ver o que está valendo.
func (s *Service) RoutingTable() []Decision { return s.router.Table() }

// Summarize agrega por escopo e período. Período vazio = mês corrente, que é a
// janela do ciclo de cobrança e a que a tela pede em 9 de 10 aberturas.
func (s *Service) Summarize(ctx context.Context, scope Scope, scopeID string,
	since, until time.Time, recentLimit int) (*Summary, error) {

	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	scope, scopeID, err = s.normalizeScope(accountID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	if since.IsZero() || until.IsZero() {
		since, until = CurrentMonth(s.now())
	}
	if !until.After(since) {
		return nil, errs.Invalid("período inválido: o fim precisa ser posterior ao início")
	}
	if recentLimit < 0 {
		recentLimit = 0
	}
	if recentLimit > maxRecent {
		recentLimit = maxRecent
	}
	return s.repo.Summarize(ctx, accountID, scope, scopeID, since.UTC(), until.UTC(), recentLimit)
}

// maxRecent existe porque a tabela de uso é a que mais cresce na plataforma:
// um limite ausente vira varredura de milhões de linhas na primeira tela que
// esquecer de paginar.
const maxRecent = 200

// CurrentMonth é a janela padrão de agregação, em UTC — o mês do relógio do
// processo e o do banco precisam ser o mesmo mês.
func CurrentMonth(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return since, since.AddDate(0, 1, 0)
}

// normalizeScope resolve o escopo e recusa o que não é vocabulário.
//
// Escopo vazio vira 'account' com a conta ativa: é o caso comum e pedir que o
// chamador repita o id da conta que já está no contexto só cria oportunidade
// de ele repetir o id ERRADO.
func (s *Service) normalizeScope(accountID string, scope Scope, scopeID string) (Scope, string, error) {
	scope = Scope(strings.ToLower(strings.TrimSpace(string(scope))))
	scopeID = strings.TrimSpace(scopeID)
	if scope == "" {
		scope = ScopeAccount
	}
	if !ValidScope(scope) {
		return "", "", errs.Invalid("escopo de orçamento desconhecido: %q", scope)
	}
	if scope == ScopeAccount {
		// A conta do escopo é SEMPRE a ativa. Aceitar outra seria abrir
		// leitura de orçamento alheio por parâmetro.
		return scope, accountID, nil
	}
	if scopeID == "" {
		return "", "", errs.Invalid("escopo de demanda exige o identificador da demanda")
	}
	return scope, scopeID, nil
}
