package workflow

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/idem"
)

// Service concentra as regras do fluxo de trabalho. Recebe apenas PORTAS.
type Service struct {
	repo   Repository
	tree   Ancestry
	access Access
	clock  ports.Clock
}

// NewService exige um relógio. Aceitar nil era o que mantinha a porta de
// enfeite: o serviço caía em time.Now() por dentro e nenhum teste de
// versionamento era determinístico. Panic aqui é deliberado — é erro de
// montagem, detectado no boot.
func NewService(repo Repository, tree Ancestry, access Access, clock ports.Clock) *Service {
	if clock == nil {
		panic("workflow.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	return &Service{repo: repo, tree: tree, access: access, clock: clock}
}

func (s *Service) now() time.Time { return s.clock.Now() }

// ── leitura ──────────────────────────────────────────────────────────────────

// List lista os fluxos visíveis da conta. Conteúdo (fluxo, skill, git_flow) em
// conta PJ é ABERTO dentro da conta por default (ADR-0014 §6): quem está na
// conta vê o que a conta escreveu. Credencial é risco, fluxo é conhecimento.
func (s *Service) List(ctx context.Context, scope Scope, ownerID string) ([]Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if scope != "" && !ValidScope(scope) {
		return nil, errs.Invalid("nível desconhecido: %q", scope)
	}
	return s.repo.List(ctx, accountID, scope, strings.TrimSpace(ownerID))
}

// Get devolve a versão CORRENTE do fluxo.
func (s *Service) Get(ctx context.Context, id string) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("identificador do fluxo não informado")
	}
	return s.repo.ByID(ctx, accountID, id)
}

// GetVersion devolve uma versão congelada, exatamente como foi gravada.
//
// É o que a demanda em andamento consome: ela guardou (id, versão) ao iniciar
// e precisa continuar enxergando aquele documento mesmo depois de o fluxo ter
// avançado três versões (ADR-0014 §4).
func (s *Service) GetVersion(ctx context.Context, id string, version int32) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("identificador do fluxo não informado")
	}
	if version <= 0 {
		return nil, errs.Invalid("versão do fluxo precisa ser positiva")
	}
	return s.repo.VersionOf(ctx, accountID, id, version)
}

// Validate é o ensaio: devolve o relatório sem gravar nada.
//
// Devolve Report e NÃO erro quando o fluxo é inválido — o cliente pediu uma
// avaliação, e recebê-la como falha de RPC obrigaria a tela a ler mensagem de
// erro para montar a lista de problemas. A recusa com errs.Invalid acontece em
// Create e Update, onde o fluxo inválido de fato impede alguma coisa.
func (s *Service) Validate(ctx context.Context, in Flow) (Report, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return Report{}, err
	}
	in.Normalize()
	return Validate(in), nil
}

// ── escrita ──────────────────────────────────────────────────────────────────

// Create grava o fluxo de um nível e a sua versão 1.
//
// O nível é verificado contra a ÁRVORE da conta, não contra o que o cliente
// afirma: um id de workspace é adivinhável e viaja no corpo da requisição, e
// sem essa checagem bastaria mandar o id de outra conta para pendurar um fluxo
// lá dentro.
func (s *Service) Create(ctx context.Context, in Flow, idempotencyKey string) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "ator não identificado")
	}

	ref, err := s.resolveOwner(ctx, accountID, ScopeRef{Scope: in.OwnerScope, ID: in.OwnerID})
	if err != nil {
		return nil, err
	}
	if ref.Scope == ScopePlatform {
		return nil, errs.Permission("o catálogo da plataforma é semeado pela migração, não escrito por RPC: um fluxo de nível 0 vale para todas as contas")
	}

	in.Normalize()
	if rep := Validate(in); !rep.Valid() {
		return nil, rep.Err()
	}

	now := s.now()
	f := Flow{
		AccountID:   accountID,
		OwnerScope:  ref.Scope,
		OwnerID:     ref.ID,
		Name:        in.Name,
		Description: in.Description,
		Version:     1,
		Stages:      in.Stages,
		CreatedBy:   call.ActorID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return s.repo.Create(ctx, &f, s.writeKey(idempotencyKey, "create", f, 0))
}

// Update GERA VERSÃO NOVA. Nunca altera a existente.
//
// Duas defesas contra reescrever o passado de quem está em execução:
//
//   - a versão anterior fica intacta no banco (a porta nem oferece caminho para
//     alterá-la, e o banco recusa por trigger);
//   - a versão que o autor editou é comparada com a corrente ANTES de gravar.
//     Sem isso, duas pessoas editando o mesmo fluxo produziriam a versão 4
//     duas vezes e a segunda apagaria o trabalho da primeira do documento vivo.
//
// Reenvio idêntico NÃO versiona: um cliente com retry automático versionaria o
// fluxo para sempre, e a demanda passaria a apontar para uma versão que ninguém
// escreveu.
func (s *Service) Update(ctx context.Context, in Flow) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "ator não identificado")
	}
	if strings.TrimSpace(in.ID) == "" {
		return nil, errs.Invalid("identificador do fluxo não informado")
	}

	current, err := s.repo.ByID(ctx, accountID, in.ID)
	if err != nil {
		return nil, err
	}
	if current.OwnerScope == ScopePlatform {
		return nil, errs.Permission("o catálogo da plataforma não se edita por RPC: derive um fluxo no seu nível e a cadeia sobrepõe o que você declarar")
	}

	in.Normalize()
	if rep := Validate(in); !rep.Valid() {
		return nil, rep.Err()
	}
	if in.Version != 0 && in.Version != current.Version {
		return nil, errs.Conflict(
			"o fluxo %q já está na versão %d e esta alteração foi escrita sobre a versão %d: recarregue antes de gravar",
			current.Name, current.Version, in.Version)
	}
	if in.SameStages(*current) {
		return current, nil
	}

	next := Flow{
		ID:          current.ID,
		AccountID:   accountID,
		OwnerScope:  current.OwnerScope,
		OwnerID:     current.OwnerID,
		Name:        in.Name,
		Description: in.Description,
		Version:     current.Version + 1,
		Stages:      in.Stages,
		CreatedBy:   call.ActorID,
		CreatedAt:   current.CreatedAt,
		UpdatedAt:   s.now(),
	}
	// UpdateFlowRequest não carrega idempotency_key (ver relatório): a chave é
	// DERIVADA do conteúdo e da versão base, o que dá a mesma garantia — o
	// mesmo reenvio colide na chave e devolve a versão já gravada, em vez de
	// empilhar versões idênticas.
	return s.repo.AppendVersion(ctx, accountID, &next, current.Version,
		s.writeKey("", "update", next, current.Version))
}

// ── resolução ────────────────────────────────────────────────────────────────

// Resolve percorre a cadeia plataforma ◁ conta ◁ workspace ◁ projeto ◁ demanda
// e devolve o fluxo efetivo COM a procedência.
//
// A ordem importa duas vezes: a linhagem vem da árvore da conta (é ela que diz
// em qual projeto a demanda vive), e a sobreposição respeita essa ordem. Um
// nível que não declara nada herda por omissão e nem aparece no rastro.
func (s *Service) Resolve(ctx context.Context, scope Scope, scopeID string) (*EffectiveFlow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := s.resolveOwner(ctx, accountID, ScopeRef{Scope: scope, ID: scopeID})
	if err != nil {
		return nil, err
	}

	chain, err := s.tree.ChainOf(ctx, accountID, ref)
	if err != nil {
		return nil, err
	}
	declared, err := s.repo.ByOwners(ctx, accountID, chain)
	if err != nil {
		return nil, err
	}

	// Reordena o que o repositório devolveu segundo a CADEIA — a sobreposição
	// depende da ordem, e ordem vinda de ORDER BY seria ordem por acaso.
	byRef := make(map[ScopeRef]Flow, len(declared))
	for _, f := range declared {
		byRef[f.Ref()] = f
	}
	levels := make([]Flow, 0, len(chain))
	for _, c := range chain {
		if f, ok := byRef[c]; ok {
			levels = append(levels, f)
		}
	}

	eff := MergeChain(levels)
	if len(eff.Flow.Stages) == 0 {
		return nil, errs.NotFound(
			"fluxo aplicável a %s: nenhum nível da cadeia declara etapas, nem o catálogo da plataforma", ref)
	}
	return &eff, nil
}

// ── promoção ─────────────────────────────────────────────────────────────────

// Promote publica o fluxo para um nível ACIMA na cadeia (ADR-0014 §5).
//
// Três recusas, e cada uma fecha um buraco diferente:
//
//   - alvo fora da linhagem do próprio fluxo: promover é subir na SUA cadeia,
//     não aterrissar num projeto vizinho que por acaso é da mesma conta;
//   - alvo no catálogo da plataforma: dentro de uma conta PJ, fluxo de nível
//     inferior é público DENTRO da conta — nunca fora dela. Compartilhamento
//     externo está explicitamente fora da v1 (ADR-0014 §7);
//   - ator sem `manage`: publicar para o nível acima muda o fluxo de quem não
//     pediu nada, e isso não é decisão de qualquer membro.
func (s *Service) Promote(ctx context.Context, flowID string, target Scope, targetID string) (*Flow, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return nil, err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return nil, errs.New(errs.KindUnauthorized, "ator não identificado")
	}
	if strings.TrimSpace(flowID) == "" {
		return nil, errs.Invalid("identificador do fluxo não informado")
	}
	if !ValidScope(target) {
		return nil, errs.Invalid("nível de destino desconhecido: %q", target)
	}
	if target == ScopePlatform {
		return nil, errs.Permission("o catálogo da plataforma não recebe fluxo de conta: dentro de uma conta PJ um fluxo de nível inferior é público DENTRO da conta, nunca fora dela (ADR-0014 §6 e §7)")
	}

	src, err := s.repo.ByID(ctx, accountID, flowID)
	if err != nil {
		return nil, err
	}
	if src.OwnerScope == ScopePlatform {
		return nil, errs.Precondition("o fluxo já é do catálogo da plataforma: não há nível acima")
	}
	if Rank(target) >= Rank(src.OwnerScope) {
		return nil, errs.Invalid(
			"promoção sobe na cadeia: %s não está acima de %s", target.Label(), src.OwnerScope.Label())
	}

	want := ScopeRef{Scope: target, ID: strings.TrimSpace(targetID)}
	if want.Scope == ScopeAccount && want.ID == "" {
		want.ID = accountID
	}
	chain, err := s.tree.ChainOf(ctx, accountID, src.Ref())
	if err != nil {
		return nil, err
	}
	if !containsRef(chain, want) {
		return nil, errs.Invalid(
			"o %s %q não está na linhagem do fluxo: promover é subir na própria cadeia, não publicar num ramo vizinho",
			target.Label(), want.ID)
	}

	role, err := s.access.RoleOf(ctx, call.ActorID, accountID)
	if err != nil {
		return nil, err
	}
	if !canManage(role) {
		return nil, errs.Permission("promover exige manage sobre o conteúdo da conta: apenas owner ou admin")
	}

	src.CreatedBy = call.ActorID
	src.UpdatedAt = s.now()
	return s.repo.Promote(ctx, accountID, src, want,
		s.writeKey("", "promote:"+string(want.Scope)+":"+want.ID, *src, src.Version))
}

// ── auxiliares ───────────────────────────────────────────────────────────────

// resolveOwner normaliza e CONFIRMA o nível endereçado.
//
// O nível conta é o único que dispensa id — ele é sempre a conta ativa, e
// aceitar outro id no corpo seria deixar o chamador escolher o tenant. Os
// demais são confirmados contra a árvore: id que não existe, ou que é de outra
// conta, volta como NotFound pela própria porta.
func (s *Service) resolveOwner(ctx context.Context, accountID string, ref ScopeRef) (ScopeRef, error) {
	ref.ID = strings.TrimSpace(ref.ID)
	if !ValidScope(ref.Scope) {
		return ScopeRef{}, errs.Invalid("nível desconhecido: %q", ref.Scope)
	}
	switch ref.Scope {
	case ScopePlatform:
		return ScopeRef{Scope: ScopePlatform}, nil
	case ScopeAccount:
		if ref.ID != "" && ref.ID != accountID {
			return ScopeRef{}, errs.Permission("nível de conta é sempre a conta ativa")
		}
		return ScopeRef{Scope: ScopeAccount, ID: accountID}, nil
	}
	if ref.ID == "" {
		return ScopeRef{}, errs.Invalid("nível %s exige o identificador do dono", ref.Scope.Label())
	}
	if _, err := s.tree.ChainOf(ctx, accountID, ref); err != nil {
		return ScopeRef{}, err
	}
	return ref, nil
}

// writeKey devolve a chave de idempotência da escrita.
//
// Toda escrita carrega uma (ADR-0017). Quando o contrato não traz a chave do
// cliente, ela é DERIVADA do que está sendo gravado: o mesmo reenvio produz a
// mesma chave, colide no índice único e devolve o que já foi gravado — em vez
// de criar um fluxo gêmeo ou uma versão duplicada.
func (s *Service) writeKey(given, op string, f Flow, base int32) string {
	if k := strings.TrimSpace(given); k != "" {
		return k
	}
	parts := []string{op, f.AccountID, string(f.OwnerScope), f.OwnerID, f.ID,
		strconv.Itoa(int(base)), f.Name, f.Description}
	for _, st := range f.Stages {
		parts = append(parts, st.Key, st.Name, string(st.Type), string(st.Gate))
		for _, a := range st.Artifacts {
			parts = append(parts, string(a))
		}
		parts = append(parts, st.Subtypes...)
	}
	return "wf:" + idem.Hash(parts...)
}

func containsRef(refs []ScopeRef, want ScopeRef) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}
