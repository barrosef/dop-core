package demand

import (
	"context"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// Service concentra as regras da demanda. Recebe apenas PORTAS.
type Service struct {
	repo    Repository
	flows   FlowResolver
	watcher Watcher
	clock   ports.Clock
}

// NewService recusa dependência nula.
//
// Panic aqui é deliberado, e pela mesma razão de identity.NewService: isto é
// erro de MONTAGEM, e erro de montagem tem que aparecer no boot, não às três da
// manhã na primeira demanda que alguém tentar iniciar. Aceitar nil e cair num
// fallback por dentro é o que transforma porta em enfeite.
func NewService(repo Repository, flows FlowResolver, watcher Watcher, clock ports.Clock) *Service {
	switch {
	case repo == nil:
		panic("demand.NewService: repositório obrigatório")
	case flows == nil:
		panic("demand.NewService: resolvedor de fluxo obrigatório — sem ele não há o que congelar")
	case watcher == nil:
		panic("demand.NewService: assinatura de eventos obrigatória — WatchDemand depende dela")
	case clock == nil:
		panic("demand.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	return &Service{repo: repo, flows: flows, watcher: watcher, clock: clock}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// ── leitura ──────────────────────────────────────────────────────────────────

// List devolve as demandas do projeto, paginadas pelo id da última lida.
func (s *Service) List(ctx context.Context, projectID string, size int, token string) ([]Demand, string, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, "", err
	}
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	// Pede um a mais para saber se há próxima página sem uma contagem extra.
	list, err := s.repo.List(ctx, accountID, projectID, size+1, token)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(list) > size {
		list = list[:size]
		next = list[len(list)-1].ID
	}
	return list, next, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Demand, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, accountID, id)
}

func (s *Service) load(ctx context.Context, accountID, id string) (*Demand, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("demanda não informada")
	}
	d, err := s.repo.ByID(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errs.NotFound("demanda")
	}
	return d, nil
}

// ── início: onde o fluxo é resolvido e CONGELADO ─────────────────────────────

// Start resolve o fluxo efetivo do projeto e o congela dentro da demanda.
//
// O congelamento é o ponto inteiro desta operação (ADR-0014 §4). Um fluxo é
// editável, promovível e apagável; uma demanda em andamento não pode descobrir,
// no meio do caminho, que a etapa que ela estava executando deixou de existir.
// Depois daqui, a máquina de etapas obedece ao SNAPSHOT — o fluxo vivo não tem
// mais poder sobre esta demanda.
//
// Repetir o Start da mesma chave externa devolve a demanda como está, sem
// re-resolver nada: reiniciar seria justamente reescrever o passado que o
// congelamento protege.
func (s *Service) Start(ctx context.Context, projectID, externalKey, idemKey string) (*Demand, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	projectID = strings.TrimSpace(projectID)
	externalKey = strings.TrimSpace(externalKey)
	if projectID == "" {
		return nil, errs.Invalid("projeto é obrigatório para iniciar uma demanda")
	}
	if externalKey == "" {
		return nil, errs.Invalid("chave externa é obrigatória (ex.: SUOPT-1315)")
	}

	if existing, err := s.repo.ByExternalKey(ctx, accountID, projectID, externalKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	flow, err := s.flows.Resolve(ctx, accountID, "project", projectID)
	if err != nil {
		return nil, err
	}
	if err := validateFlow(flow); err != nil {
		return nil, err
	}

	now := s.now()
	snap := freeze(flow, now)
	d := &Demand{
		AccountID:   accountID,
		ProjectID:   projectID,
		ExternalKey: externalKey,
		// Título e tipo de card vêm do provedor (Jira, ClickUp) pela
		// integração do projeto; até a sincronização acontecer, a chave
		// externa é o melhor rótulo honesto que temos.
		Title:     externalKey,
		Status:    StatusNew,
		Flow:      snap,
		Stages:    instantiate(snap),
		CreatedBy: call.ActorID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	return s.repo.Create(ctx, d, Emission{
		Type: EventStarted,
		Payload: map[string]any{
			"project_id":    projectID,
			"external_key":  externalKey,
			"flow_id":       snap.FlowID,
			"flow_version":  snap.Version,
			"resolved_from": snap.ResolvedFrom,
			"stages":        stageKeys(snap),
		},
	}, idemKey)
}

// instantiate transforma o molde congelado nas etapas da demanda. Todas nascem
// pendentes: progresso é evento, não estado inicial.
func instantiate(snap Snapshot) []Stage {
	out := make([]Stage, 0, len(snap.Stages))
	for i, spec := range snap.Stages {
		gate := spec.Gate
		if gate == "" {
			gate = GateNone
		}
		out = append(out, Stage{
			Key: spec.Key, Name: spec.Name, Type: spec.Type,
			Gate: gate, Status: StagePending, Position: i,
		})
	}
	return out
}

// validateFlow recusa congelar um fluxo quebrado.
//
// A validação é aqui, e não só no domínio de fluxo, porque este é o instante em
// que o molde vira passado imutável: um fluxo sem etapas ou com chave repetida
// congelado numa demanda é um defeito que nenhuma correção posterior do fluxo
// desfaz.
func validateFlow(f Flow) error {
	if len(f.Stages) == 0 {
		return errs.Precondition(
			"o fluxo efetivo %q não tem etapas: nada a executar", f.Name)
	}
	seen := make(map[string]bool, len(f.Stages))
	for _, st := range f.Stages {
		if strings.TrimSpace(st.Key) == "" {
			return errs.Precondition("o fluxo efetivo %q tem etapa sem chave", f.Name)
		}
		if seen[st.Key] {
			return errs.Precondition(
				"o fluxo efetivo %q repete a chave de etapa %q", f.Name, st.Key)
		}
		seen[st.Key] = true
	}
	return nil
}

func stageKeys(snap Snapshot) []string {
	keys := make([]string, 0, len(snap.Stages))
	for _, st := range snap.Stages {
		keys = append(keys, st.Key)
	}
	return keys
}

// ── máquina de etapas ────────────────────────────────────────────────────────

// AdvanceStage move uma etapa dentro da máquina dirigida pelo TIPO (ADR-0014).
func (s *Service) AdvanceStage(ctx context.Context, demandID, stageKey string, to StageStatus, idemKey string) (*Stage, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	st, noop, err := d.CheckAdvance(stageKey, to)
	if err != nil {
		return nil, err
	}
	if noop {
		return st, nil
	}

	now := s.now()
	from := st.Status
	st.Status = to
	switch to {
	case StageRunning:
		if st.StartedAt == nil {
			st.StartedAt = &now
		}
	case StageDone:
		st.FinishedAt = &now
	}

	return s.repo.SaveStage(ctx, accountID, d.ID, *st, d.ProjectStatus(), Emission{
		Type: EventStageAdvanced,
		Payload: map[string]any{
			"stage_key": st.Key, "stage_type": string(st.Type),
			"from": string(from), "to": string(to),
			"dop_status": string(d.ProjectStatus()),
		},
	}, idemKey)
}

// DecideGate registra a decisão humana do portão.
//
// Reprovar NÃO devolve a etapa para pendente: ela vai para bloqueada, com o
// comentário. Zerar a etapa apagaria da tela o fato de que houve uma reprovação
// — e esse fato é metade do valor da validação humana.
func (s *Service) DecideGate(ctx context.Context, demandID, stageKey string, approved bool, comment, idemKey string) (*Stage, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorKind == ctxutil.ActorAgent || call.ActorKind == ctxutil.ActorSubagent {
		return nil, errs.Permission(
			"portão humano é decidido por gente: agente não aprova a própria etapa")
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	st, err := d.CheckDecideGate(stageKey)
	if err != nil {
		return nil, err
	}

	now := s.now()
	st.GateApproved = &approved
	st.GateComment = comment
	if approved {
		st.Status = StageDone
		st.FinishedAt = &now
	} else {
		st.Status = StageBlocked
	}

	return s.repo.SaveStage(ctx, accountID, d.ID, *st, d.ProjectStatus(), Emission{
		Type: EventGateDecided,
		Payload: map[string]any{
			"stage_key": st.Key, "stage_type": string(st.Type),
			"approved": approved, "comment": comment,
			"to": string(st.Status), "dop_status": string(d.ProjectStatus()),
		},
	}, idemKey)
}

// ── threads (ADR-0010) ───────────────────────────────────────────────────────

func (s *Service) ListThreads(ctx context.Context, demandID string) ([]Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.load(ctx, accountID, demandID); err != nil {
		return nil, err
	}
	return s.repo.ThreadsOf(ctx, accountID, demandID)
}

// CreateThread lança um subagente: a thread nasce junto com a FICHA dele
// (ADR-0010 §2) e aparece de imediato para o dev acompanhar ou intervir.
func (s *Service) CreateThread(ctx context.Context, demandID, key string, card AgentCard, idemKey string) (*Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if err := ValidateThreadKey(key); err != nil {
		return nil, err
	}
	if err := validateCard(card); err != nil {
		return nil, err
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}

	now := s.now()
	t := &Thread{
		AccountID: accountID, DemandID: d.ID, Key: strings.TrimSpace(key),
		Card: card, State: ThreadOpen, CreatedBy: call.ActorID,
		CreatedAt: now, UpdatedAt: now,
	}
	return s.repo.CreateThread(ctx, t, Emission{
		Type: EventThreadCreated,
		Payload: map[string]any{
			"thread_key": t.Key, "purpose": card.Purpose,
			"model": card.Model, "effort": card.Effort,
			"tools": card.Tools, "budget_micros": card.BudgetMicros,
		},
	}, idemKey)
}

// validateCard: subagente sem ficha é caixa-preta, que é exatamente o que a
// ADR-0010 recusa. Propósito e modelo são o mínimo para o dev saber com quem
// está falando e para o roteador saber quanto aquilo custa.
func validateCard(c AgentCard) error {
	if strings.TrimSpace(c.Purpose) == "" {
		return errs.Invalid("a ficha do agente exige propósito")
	}
	if c.BudgetMicros < 0 {
		return errs.Invalid("orçamento do agente não pode ser negativo")
	}
	switch c.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return errs.Invalid("esforço desconhecido: %q", c.Effort)
	}
	return nil
}

func (s *Service) loadThread(ctx context.Context, accountID, id string) (*Thread, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("thread não informada")
	}
	t, err := s.repo.ThreadByID(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errs.NotFound("thread")
	}
	return t, nil
}

// PostMessage acrescenta a mensagem ao log da demanda (ADR-0006): toda mensagem
// é evento. A thread sai de `aberta` e vira `ativa` na mesma transação.
func (s *Service) PostMessage(ctx context.Context, threadID, text, idemKey string) (*Message, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if strings.TrimSpace(text) == "" {
		return nil, errs.Invalid("mensagem vazia")
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	if err := t.CheckPost(); err != nil {
		return nil, err
	}

	m := &Message{
		AccountID: accountID, DemandID: t.DemandID, ThreadID: t.ID,
		AuthorID: call.ActorID, AuthorKind: string(call.ActorKind),
		AuthorName: call.ActorName, Text: text, At: s.now(),
	}
	return s.repo.AppendMessage(ctx, m, Emission{
		Type: EventMessagePosted,
		Payload: map[string]any{
			"thread_id": t.ID, "thread_key": t.Key, "text": text,
			"author_kind": m.AuthorKind, "author_id": m.AuthorID,
		},
	}, idemKey)
}

// SetThreadBlocked marca (ou desfaz) a pergunta pendente.
//
// É o que alimenta a caixa de atenção: thread bloqueada é item de fila, e sem
// esse estado o modelo multi-agente afoga o dev (spec §3, risco R-2). Não tem
// RPC própria ainda; quem a chama hoje é o runtime do agente pela borda.
func (s *Service) SetThreadBlocked(ctx context.Context, threadID string, blocked bool, reason, idemKey string) (*Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	if t.State == ThreadConcluded {
		return nil, errs.Precondition("a thread %q está concluída", t.Key)
	}
	state, evType := ThreadBlocked, EventThreadBlocked
	if !blocked {
		state, evType = ThreadActive, EventThreadResumed
	}
	if t.State == state {
		return t, nil
	}
	return s.repo.SaveThreadState(ctx, accountID, t.ID, state, Emission{
		Type: evType,
		Payload: map[string]any{
			"thread_id": t.ID, "thread_key": t.Key, "reason": reason,
		},
	}, idemKey)
}

// PublishFinding publica a conclusão estruturada da investigação.
//
// É o que entra no contexto dos irmãos, no dossiê e na memória do projeto
// (ADR-0010 §4) — e é o que DESTRAVA a conclusão da thread: sem achado
// publicado, ConcludeThread recusa.
func (s *Service) PublishFinding(ctx context.Context, demandID, threadID, title string, payload map[string]any, idemKey string) (*Finding, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if strings.TrimSpace(title) == "" {
		return nil, errs.Invalid("o achado precisa de um título")
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	// Thread de outra demanda publicando no quadro desta seria vazamento de
	// contexto entre demandas — e a demanda é a fronteira de segurança
	// (ADR-0010 §6).
	if t.DemandID != d.ID {
		return nil, errs.Invalid("a thread %q não pertence a esta demanda", t.Key)
	}
	if payload == nil {
		payload = map[string]any{}
	}

	f := &Finding{
		AccountID: accountID, DemandID: d.ID, ThreadID: t.ID,
		Title: title, Payload: payload, CreatedBy: call.ActorID,
		CreatedAt: s.now(),
	}
	return s.repo.CreateFinding(ctx, f, Emission{
		Type: EventFindingPublished,
		Payload: map[string]any{
			"thread_id": t.ID, "thread_key": t.Key,
			"title": title, "finding": payload,
		},
	}, idemKey)
}

// ConcludeThread encerra a conversa — e só aceita se o achado já foi publicado.
//
// É a regra literal da spec: "concluir exige publicar o achado; a thread não
// morre em silêncio". Investigação que termina sem achado some junto com o
// transcript, e o próximo agente refaz o mesmo trabalho.
func (s *Service) ConcludeThread(ctx context.Context, threadID, idemKey string) (*Thread, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.loadThread(ctx, accountID, threadID)
	if err != nil {
		return nil, err
	}
	has, err := s.repo.HasFinding(ctx, accountID, t.ID)
	if err != nil {
		return nil, err
	}
	if err := t.CheckConclude(has); err != nil {
		return nil, err
	}
	if t.State == ThreadConcluded {
		return t, nil
	}
	return s.repo.SaveThreadState(ctx, accountID, t.ID, ThreadConcluded, Emission{
		Type:    EventThreadConcluded,
		Payload: map[string]any{"thread_id": t.ID, "thread_key": t.Key},
	}, idemKey)
}

// ── streaming ────────────────────────────────────────────────────────────────

// Watch entrega ao vivo os eventos DESTA demanda.
//
// O fan-out, o replay e o isolamento por conta são do serviço de eventos — aqui
// só se recorta. O recorte por demanda é feito neste lado porque o filtro do
// barramento é por agregado e tipo, não por id: assinar "demand" e descartar o
// que é de outra demanda custa uma comparação de string por evento e evita
// duplicar a máquina de assinatura.
//
// A demanda é carregada ANTES de abrir o fluxo: cliente que pede uma demanda
// inexistente ou de outra conta recebe 404 na hora, e não um stream mudo que
// ele vai interpretar como "ainda não aconteceu nada".
func (s *Service) Watch(ctx context.Context, demandID string, emit func(ports.Event) error) error {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return err
	}
	d, err := s.load(ctx, accountID, demandID)
	if err != nil {
		return err
	}
	return s.watcher.Watch(ctx, "", []string{Aggregate}, nil, func(e ports.Event) error {
		if e.AggregateID != d.ID {
			return nil
		}
		return emit(e)
	})
}
