package delivery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Service concentra as regras de entrega. Recebe apenas PORTAS.
type Service struct {
	repo    Repository
	demands Demands
	clock   ports.Clock
}

// NewService exige as três portas.
//
// O relógio é obrigatório pelo mesmo motivo que em identity: aceitar nil
// deixaria o serviço cair em time.Now() por dentro, e nenhum teste de
// evidência ("a execução é deste commit e terminou quando?") seria
// determinístico. Panic aqui é deliberado — erro de montagem se detecta no
// boot, não em produção.
func NewService(repo Repository, demands Demands, clock ports.Clock) *Service {
	if repo == nil {
		panic("delivery.NewService: repositório obrigatório")
	}
	if demands == nil {
		panic("delivery.NewService: porta de demandas obrigatória")
	}
	if clock == nil {
		panic("delivery.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	return &Service{repo: repo, demands: demands, clock: clock}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// ─────────────────────────── evidência ───────────────────────────

// RecordVerification registra UMA execução de verificação.
//
// É por aqui que a evidência entra no sistema, e é por isso que não existe
// nenhuma RPC que diga "este PR está verde": o verde é derivado destas linhas.
// Quem quiser burlar precisa forjar uma execução com commit, suíte, resultado
// e rastro — que é exatamente o registro que se quer auditável.
func (s *Service) RecordVerification(ctx context.Context, run VerificationRun, idemKey string) (*VerificationRun, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := run.Validate(); err != nil {
		return nil, err
	}
	// A demanda precisa existir NA CONTA: sem isso, evidência de uma conta
	// provaria o verde de outra.
	if _, err := s.demands.Demand(ctx, accountID, run.DemandID); err != nil {
		return nil, err
	}
	run.AccountID = accountID
	if run.EndedAt.IsZero() {
		run.EndedAt = s.now()
	}
	if run.Attempts <= 0 {
		run.Attempts = 1
	}
	return s.repo.RecordVerification(ctx, &run, idemKey)
}

// Evidence responde "o que se sabe sobre o verde deste commit" — a consulta que
// o crítico, o cockpit e a recusa da fila compartilham.
func (s *Service) Evidence(ctx context.Context, demandID, repoID, commit string) (Evidence, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return Evidence{}, err
	}
	if demandID == "" || repoID == "" || commit == "" {
		return Evidence{}, errs.Invalid("evidência é sempre de uma demanda, um repositório e um commit")
	}
	return s.repo.EvidenceFor(ctx, accountID, demandID, repoID, commit)
}

// ─────────────────────────── pull requests ───────────────────────────

// OpenSpec é o pedido de abertura de PR.
type OpenSpec struct {
	DemandID     string
	RepoID       string
	Repo         string
	SourceBranch string
	TargetBranch string
	HeadCommit   string
	URL          string
	ExternalID   string
	Reviewers    []Reviewer
}

// OpenPullRequest é a ADR-0007 no ponto exato onde ela vale: não existe PR sem
// evidência de verde do commit que ele carrega.
//
// A recusa diz o que falta, item por item. "Falha de precondição" sem dizer
// qual precondição faz o agente tentar de novo às cegas — e tentar de novo às
// cegas é como um PR quebrado acaba chegando ao humano por outro caminho.
func (s *Service) OpenPullRequest(ctx context.Context, spec OpenSpec, idemKey string) (*PullRequest, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.DemandID) == "" || strings.TrimSpace(spec.RepoID) == "" {
		return nil, errs.Invalid("PR precisa de demanda e repositório")
	}
	if strings.TrimSpace(spec.HeadCommit) == "" {
		return nil, errs.Invalid("PR sem commit de topo: não há o que verificar")
	}
	if strings.TrimSpace(spec.SourceBranch) == "" {
		return nil, errs.Invalid("PR sem branch de origem")
	}
	if _, err := s.demands.Demand(ctx, accountID, spec.DemandID); err != nil {
		return nil, err
	}

	ev, err := s.repo.EvidenceFor(ctx, accountID, spec.DemandID, spec.RepoID, spec.HeadCommit)
	if err != nil {
		return nil, err
	}
	if falta := ev.Missing(); len(falta) > 0 {
		return nil, errs.Precondition(
			"sem verde, sem PR (ADR-0007): %s", strings.Join(falta, "; "))
	}

	target := spec.TargetBranch
	if target == "" {
		target = "main"
	}
	pr := &PullRequest{
		AccountID:    accountID,
		DemandID:     spec.DemandID,
		RepoID:       spec.RepoID,
		Repo:         spec.Repo,
		SourceBranch: spec.SourceBranch,
		TargetBranch: target,
		HeadCommit:   spec.HeadCommit,
		URL:          spec.URL,
		ExternalID:   spec.ExternalID,
		Reviewers:    spec.Reviewers,
		CreatedAt:    s.now(),
		UpdatedAt:    s.now(),
	}
	return s.repo.OpenPullRequest(ctx, pr, idemKey)
}

func (s *Service) ListPullRequests(ctx context.Context, f PRFilter) ([]PullRequest, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListPullRequests(ctx, accountID, f)
}

// ─────────────────────────── fila de merge ───────────────────────────

// GetMergeQueue devolve a fila de UM repositório, em ordem determinística e
// com as posições numeradas (ADR-0008 §1).
//
// A fila é por repositório porque é o repositório que serializa: dois PRs em
// repositórios diferentes não invalidam um ao outro.
func (s *Service) GetMergeQueue(ctx context.Context, repoID string) ([]MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repoID) == "" {
		return nil, errs.Invalid("a fila de merge é sempre de um repositório (ADR-0008)")
	}
	entries, err := s.repo.QueueOfRepo(ctx, accountID, repoID, false)
	if err != nil {
		return nil, err
	}
	return SortQueue(entries), nil
}

// EnqueueMerge é a porta da fila — e é onde a ADR-0007 é cobrada pela segunda
// vez, agora contra o commit ATUAL do PR.
//
// Cobrar de novo não é redundância: entre a abertura do PR e a entrada na fila
// o branch pode ter avançado, e o verde do commit antigo não é o verde do
// commit novo. Este é o mesmo raciocínio que faz a fila re-verificar a cada
// posição (ADR-0008 §1) — o verde é sempre sobre um estado do código, nunca
// sobre uma intenção.
func (s *Service) EnqueueMerge(ctx context.Context, repoID, demandID, idemKey string) (*MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repoID) == "" || strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("entrar na fila exige repositório e demanda")
	}
	if _, err := s.demands.Demand(ctx, accountID, demandID); err != nil {
		return nil, err
	}

	pr, err := s.repo.PullRequestOf(ctx, accountID, demandID, repoID)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, errs.Precondition(
			"a demanda %s ainda não tem PR aberto no repositório %s — e não há PR sem evidência de verde (ADR-0007)",
			demandID, repoID)
	}
	if pr.Merged {
		return nil, errs.Precondition("o PR da demanda %s já foi mergeado", demandID)
	}

	ev, err := s.repo.EvidenceFor(ctx, accountID, demandID, repoID, pr.HeadCommit)
	if err != nil {
		return nil, err
	}
	if falta := ev.Missing(); len(falta) > 0 {
		// FailedPrecondition, e não Invalid: o pedido está bem formado; o que
		// falta é um estado do mundo que o chamador pode providenciar (rodar a
		// aceitação, chamar o crítico) e tentar de novo.
		return nil, errs.Precondition(
			"a fila de merge recusa entrada sem evidência de verde do commit %s (ADR-0007): %s",
			curto(pr.HeadCommit), strings.Join(falta, "; "))
	}

	entry := &MergeQueueEntry{
		AccountID:     accountID,
		RepoID:        repoID,
		DemandID:      demandID,
		PullRequestID: pr.ID,
		Priority:      DefaultPriority,
		State:         StateQueued,
		EnqueuedAt:    s.now(),
		UpdatedAt:     s.now(),
	}
	return s.repo.Enqueue(ctx, entry, idemKey)
}

// AdvanceQueue move a entrada pelo fluxo `na fila → rebase → re-verificação →
// merge`. Transição fora da máquina de estados é recusada — é o que impede
// "mergeado" sem passar pela re-verificação.
func (s *Service) AdvanceQueue(ctx context.Context, entryID string, to QueueState, idemKey string) (*MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidQueueState(to) {
		return nil, errs.Invalid("estado de fila desconhecido: %q", to)
	}
	if to == StateConflict {
		// Conflito carrega relato; tem caminho próprio, com evento próprio.
		return nil, errs.Invalid("conflito entra por ReportConflict, com o relato que a caixa de atenção precisa")
	}
	entry, err := s.queueEntry(ctx, accountID, entryID)
	if err != nil {
		return nil, err
	}
	if entry.State == to {
		return entry, nil // repetição é inócua
	}
	if !entry.State.CanTransitionTo(to) {
		return nil, errs.Precondition(
			"a fila não vai de %s para %s (ADR-0008: na fila → rebase → re-verificação → merge)",
			entry.State, to)
	}
	return s.repo.SetQueueState(ctx, accountID, entryID, to, nil, idemKey)
}

// ReportConflict transforma o conflito em ITEM DE DECISÃO HUMANA.
//
// A ADR-0008 §2 é explícita: rebase e resolução são tarefa do agente da
// demanda; falha ESCALA ao humano pela caixa de atenção, com o contexto do
// conflito. Escalar é gravar estado e emitir evento na mesma transação — quem
// alimenta a caixa é o evento. Devolver erro aqui seria a versão silenciosa do
// mesmo fato: o agente veria uma falha, o humano não veria nada.
func (s *Service) ReportConflict(ctx context.Context, entryID string, c ConflictReport, idemKey string) (*MergeQueueEntry, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	entry, err := s.queueEntry(ctx, accountID, entryID)
	if err != nil {
		return nil, err
	}
	if entry.State.IsTerminal() {
		return nil, errs.Precondition("entrada já mergeada não entra em conflito")
	}
	if c.ReportedAt.IsZero() {
		c.ReportedAt = s.now()
	}
	return s.repo.SetQueueState(ctx, accountID, entryID, StateConflict, &c, idemKey)
}

func (s *Service) queueEntry(ctx context.Context, accountID, entryID string) (*MergeQueueEntry, error) {
	if strings.TrimSpace(entryID) == "" {
		return nil, errs.Invalid("entrada de fila não informada")
	}
	entry, err := s.repo.QueueEntryByID(ctx, accountID, entryID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, errs.NotFound("entrada da fila de merge")
	}
	return entry, nil
}

// ─────────────────────────── diretrizes ───────────────────────────

// ProposeDirective é o techlead acionando a caixa de atenção com uma provocação
// de decisão (ADR-0015 §3): opções prontas e uma recomendação.
//
// Não existe caminho para propor "pausar a demanda X": Validate percorre as
// instruções de cada opção e só deixa passar o vocabulário de coordenação.
func (s *Service) ProposeDirective(ctx context.Context, d Directive, idemKey string) (*Directive, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	// Toda demanda instruída precisa existir na conta e pertencer ao projeto
	// da diretriz — coordenação entre projetos diferentes não é coordenação, é
	// engano.
	for _, id := range instructedDemands(d) {
		info, err := s.demands.Demand(ctx, accountID, id)
		if err != nil {
			return nil, err
		}
		if info.ProjectID != d.ProjectID {
			return nil, errs.Invalid(
				"a demanda %s não pertence ao projeto da diretriz", id)
		}
	}
	d.AccountID = accountID
	d.Status = DirectiveProposed
	d.CreatedAt = s.now()
	d.UpdatedAt = d.CreatedAt
	return s.repo.CreateDirective(ctx, &d, idemKey)
}

func (s *Service) ListDirectives(ctx context.Context, projectID string) ([]Directive, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, errs.Invalid("diretrizes são sempre de um projeto (ADR-0015)")
	}
	return s.repo.ListDirectives(ctx, accountID, projectID)
}

// Chaves aceitas no Struct de decisão do contrato. São duas porque a ADR-0015
// pede as duas: a escolha e o motivo dela.
const (
	DecisionKeyOption    = "option"
	DecisionKeyRationale = "rationale"
)

// DecideDirective registra a escolha do dev: QUEM decidiu, QUAL opção e POR QUÊ.
//
// E aqui está a regra de ouro, dita de novo porque é a que mais tenta escapar:
// decidir uma diretriz NÃO interrompe demanda nenhuma. Não há como: a única
// porta deste domínio para o domínio de demanda é de leitura, e o que a decisão
// produz são instruções do vocabulário de coordenação — trabalho a fazer,
// condicionado ao que já está acontecendo. A demanda 1 segue até onde der; quando
// a condição se cumprir, aplica a coordenação e continua.
func (s *Service) DecideDirective(ctx context.Context, directiveID string, decision map[string]any, idemKey string) (*Directive, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "decisão de diretriz exige ator identificado")
	}
	// Quem decide é o dev (ADR-0015 §4): o techlead detecta, planeja e propõe;
	// a escolha é humana. Deixar um agente decidir a própria proposta fecharia
	// o laço sem o único participante que a diretriz existe para consultar.
	if call.ActorKind == ctxutil.ActorAgent || call.ActorKind == ctxutil.ActorSubagent {
		return nil, errs.Permission("agentes propõem diretrizes; quem decide é o dev (ADR-0015 §4)")
	}
	if strings.TrimSpace(directiveID) == "" {
		return nil, errs.Invalid("diretriz não informada")
	}

	option := texto(decision[DecisionKeyOption])
	rationale := texto(decision[DecisionKeyRationale])
	if option == "" {
		return nil, errs.Invalid("a decisão precisa dizer qual opção (%q)", DecisionKeyOption)
	}
	// Motivo é obrigatório. Coordenação entre demandas paralelas é decisão de
	// engenharia: sem o porquê registrado, ninguém entende três semanas depois
	// por que a demanda 2 esperou a 1 — e a diretriz vira mágica invisível.
	if rationale == "" {
		return nil, errs.Invalid("a decisão precisa registrar o motivo (%q)", DecisionKeyRationale)
	}

	d, err := s.repo.DirectiveByID(ctx, accountID, directiveID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errs.NotFound("diretriz")
	}
	if d.Status == DirectiveDecided && d.Decision != nil {
		if d.Decision.Option == option {
			return d, nil // repetição da mesma decisão é inócua
		}
		return nil, errs.Conflict(
			"a diretriz já foi decidida (%s) por %s — proponha uma nova em vez de reescrever a decisão",
			d.Decision.Option, d.Decision.DecidedBy)
	}
	if d.Status == DirectiveSuperseded {
		return nil, errs.Precondition("diretriz superada por outra")
	}

	opt, ok := d.Option(option)
	if !ok {
		return nil, errs.Invalid("a opção %q não está entre as oferecidas: %s",
			option, strings.Join(chaves(d.Options), ", "))
	}
	for _, ins := range opt.Instructions {
		if err := ins.Validate(); err != nil {
			return nil, err
		}
	}

	dec := Decision{
		Option:    option,
		Rationale: rationale,
		DecidedBy: call.ActorID,
		ActorKind: string(call.ActorKind),
		DecidedAt: s.now(),
	}
	return s.repo.DecideDirective(ctx, accountID, directiveID, dec, opt.Instructions, idemKey)
}

// ─────────────────────────── auxiliares ───────────────────────────

// instructedDemands junta, sem repetir, toda demanda citada pela diretriz.
func instructedDemands(d Directive) []string {
	visto := make(map[string]bool)
	var out []string
	add := func(id string) {
		if id != "" && !visto[id] {
			visto[id] = true
			out = append(out, id)
		}
	}
	for _, id := range d.AffectedDemands {
		add(id)
	}
	for _, o := range d.Options {
		for _, ins := range o.Instructions {
			add(ins.DemandID)
		}
	}
	return out
}

func chaves(opts []DirectiveOption) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Key)
	}
	return out
}

// texto extrai string do Struct do contrato sem explodir com tipo inesperado —
// o corpo vem de fora, e valor de tipo errado é erro de cliente, não pânico.
func texto(v any) string {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return strings.TrimSpace(s)
}
