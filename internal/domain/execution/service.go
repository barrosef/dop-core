package execution

import (
	"context"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/ctxutil"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Config é a política de implantação do substrato. Não é infraestrutura: são
// decisões de produto que o composition root injeta.
type Config struct {
	// DevboxImage é a imagem do sandbox. Roda como usuário arbitrário NÃO-root
	// desde a primeira imagem — OKD/OpenShift recusam root por SCC, e isso é
	// requisito de imagem, não de implantação (spec §2).
	DevboxImage string
	// IngressDomain é o sufixo das URLs <serviço>--<demanda>.<domínio> (spec §5).
	IngressDomain string
}

// Service concentra as regras do substrato. Recebe apenas PORTAS.
//
// Repare no que NÃO existe aqui: nenhum método devolve credencial, kubeconfig
// ou socket. O sandbox recebe token derivado de curta duração como volume
// projetado (spec §5) — nada disso passa por uma RPC de volta.
type Service struct {
	repo     Repository
	launcher ports.SandboxLauncher
	access   Access
	demands  Demands
	clock    ports.Clock
	cfg      Config
}

// NewService exige as portas de que depende. Panic aqui é deliberado: é erro de
// montagem, detectado no boot, não em produção às três da manhã.
//
// O relógio segue a regra da porta Clock: sem fallback para time.Now(), porque
// o fallback desliga a porta sem ninguém perceber e devolve ao teste a
// dependência do relógio de parede que a porta existe para remover.
func NewService(repo Repository, launcher ports.SandboxLauncher, access Access, demands Demands, clock ports.Clock, cfg Config) *Service {
	if repo == nil || launcher == nil || access == nil || demands == nil {
		panic("execution.NewService: repositório, launcher, acesso e demandas são obrigatórios")
	}
	if clock == nil {
		panic("execution.NewService: relógio obrigatório — use clock.NewSystem()")
	}
	return &Service{repo: repo, launcher: launcher, access: access, demands: demands, clock: clock, cfg: cfg}
}

// caller resolve conta e ator. Requisição sem conta ativa é inválida por
// definição (SP-0).
func (s *Service) caller(ctx context.Context) (accountID, userID string, role identity.Role, err error) {
	accountID, err = ctxutil.MustAccount(ctx)
	if err != nil {
		return "", "", "", err
	}
	call, _ := ctxutil.From(ctx)
	if call.ActorID == "" {
		return "", "", "", errs.New(errs.KindUnauthorized, "ator não identificado")
	}
	m, err := s.access.Authorize(ctx, call.ActorID, accountID)
	if err != nil {
		return "", "", "", err
	}
	return accountID, call.ActorID, m.Role, nil
}

// load traz o sandbox da conta ativa. Sandbox de outra conta é "não encontrado",
// nunca "sem permissão": a segunda resposta confirmaria que o id existe.
func (s *Service) load(ctx context.Context, accountID, id string) (*Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errs.Invalid("sandbox não informado")
	}
	sb, err := s.repo.ByID(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if sb == nil {
		return nil, errs.NotFound("sandbox")
	}
	return sb, nil
}

// ── provisionamento ──────────────────────────────────────────────────────────

// Provision cria o sandbox da demanda.
//
// A ordem das verificações é a regra, e a primeira delas é a mais importante:
//
//  1. o tier tem de vir DECLARADO. Sem ele, recusa — nunca um default;
//  2. repetição da mesma chave de idempotência devolve o mesmo sandbox;
//  3. a demanda precisa existir e ser da conta ativa;
//  4. uma demanda ativa tem UM sandbox (spec §1);
//  5. o substrato precisa OFERECER o tier pedido. Se não oferecer, recusa
//     ANTES de gravar qualquer estado — recusa que provisiona metade é pior
//     que recusa nenhuma.
//
// Só depois disso o estado é gravado. São duas transações, cada uma atômica com
// seu evento (ADR-0019): a primeira registra a INTENÇÃO (provisioning), a
// segunda registra o que o substrato ENTREGOU. Uma queda entre elas deixa a
// linha em provisioning — visível, reconciliável e sem sandbox órfão invisível,
// que é exatamente o que uma transação só não conseguiria dar: gravar depois do
// Launch perderia o rastro do que já subiu.
func (s *Service) Provision(ctx context.Context, demandID string, tier ports.IsolationTier, idempotencyKey string) (*Sandbox, error) {
	// Antes de tudo: o nível de isolamento é declarado, nunca presumido.
	if err := RequireTier(tier); err != nil {
		return nil, err
	}
	accountID, userID, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demanda não informada")
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("viewer não provisiona sandbox")
	}

	if idempotencyKey != "" {
		existing, err := s.repo.ByIdempotencyKey(ctx, accountID, idempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	owner, err := s.demands.DemandAccount(ctx, demandID)
	if err != nil {
		return nil, err
	}
	if owner != accountID {
		return nil, errs.NotFound("demanda")
	}

	// Uma demanda ativa, um sandbox. Devolver o que existe é o comportamento
	// útil; trocar o tier por baixo dele NÃO é — seria degradar (ou promover)
	// em silêncio um isolamento que alguém já declarou.
	if live, err := s.repo.LiveByDemand(ctx, accountID, demandID); err != nil {
		return nil, err
	} else if live != nil {
		if live.Tier != tier {
			return nil, errs.Precondition(
				"a demanda já tem sandbox com isolamento %q; destrua-o antes de pedir %q",
				live.Tier, tier)
		}
		return live, nil
	}

	if err := s.requireTierSupported(ctx, tier); err != nil {
		return nil, err
	}

	ns := NamespaceFor(demandID)
	created, err := s.repo.Create(ctx, &Sandbox{
		AccountID:      accountID,
		DemandID:       demandID,
		State:          StateProvisioning,
		Tier:           tier,
		Namespace:      ns,
		IdempotencyKey: idempotencyKey,
		CreatedBy:      userID,
		LastActiveAt:   s.clock.Now(),
	})
	if err != nil {
		return nil, err
	}

	status, err := s.launcher.Launch(ctx, s.specFor(created))
	if err != nil {
		// A linha fica em provisioning de propósito: ela é o rastro de que
		// alguém tentou. Apagá-la aqui esconderia um sandbox meio subido.
		return nil, err
	}
	if status.Tier != tier {
		// O adaptador quebrou a garantia 1 da porta. Desfazer é obrigatório:
		// entregar isolamento diferente do declarado é pior que não entregar.
		_ = s.launcher.Destroy(ctx, created.Handle())
		_, _ = s.repo.Transition(ctx, accountID, created.ID, DestroyTransition)
		return nil, errs.Internal(
			"o substrato entregou isolamento %q para um pedido de %q — sandbox descartado",
			status.Tier, tier)
	}

	return s.repo.MarkProvisioned(ctx, accountID, created.ID, status.Tier,
		s.endpoints(created.DemandID, status.Endpoints))
}

// requireTierSupported traduz "esse cluster não tem Kata" em recusa com
// mensagem, que é o R-4 da spec: o adaptador detecta e aplica a política de
// tier, nunca degrada em silêncio.
func (s *Service) requireTierSupported(ctx context.Context, tier ports.IsolationTier) error {
	supported, err := s.launcher.SupportedTiers(ctx)
	if err != nil {
		return err
	}
	for _, t := range supported {
		if t == tier {
			return nil
		}
	}
	names := make([]string, 0, len(supported))
	for _, t := range supported {
		names = append(names, string(t))
	}
	return errs.Precondition(
		"este substrato não oferece isolamento %q; disponíveis: %s",
		tier, strings.Join(names, ", "))
}

func (s *Service) specFor(sb *Sandbox) ports.SandboxSpec {
	return ports.SandboxSpec{
		SandboxHandle: sb.Handle(),
		AccountID:     sb.AccountID,
		DemandID:      sb.DemandID,
		Tier:          sb.Tier,
		Image:         s.cfg.DevboxImage,
		Env: map[string]string{
			"DOP_SANDBOX_ID": sb.ID,
			"DOP_DEMAND_ID":  sb.DemandID,
			"DOP_WORKSPACE":  ports.SandboxWorkspacePath,
		},
	}
}

// endpoints compõe a URL pública de cada serviço. A regra de nomeação é do
// domínio, não do adaptador — ver EndpointURL.
func (s *Service) endpoints(demandID string, in []ports.SandboxEndpoint) []Endpoint {
	out := make([]Endpoint, 0, len(in))
	for _, e := range in {
		out = append(out, Endpoint{
			Name:  e.Name,
			URL:   EndpointURL(s.cfg.IngressDomain, demandID, e.Name),
			Port:  e.Port,
			State: e.State,
		})
	}
	return out
}

// ── ciclo de vida ────────────────────────────────────────────────────────────

// Suspend é a operação de ECONOMIA: a execução morre, o workspace fica.
//
// Repetir é inócuo: suspender o que já está suspenso devolve o sandbox como
// está, sem tocar no substrato e SEM emitir evento. Evento de mudança que não
// mudou nada envenena o dossiê da demanda e faz toda projeção contar duas vezes.
func (s *Service) Suspend(ctx context.Context, id string) (*Sandbox, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("viewer não altera o ciclo de vida do sandbox")
	}
	if sb.State == StateSuspended {
		return sb, nil
	}
	if !CanApply(sb.State, SuspendTransition) {
		return nil, errs.Precondition(
			"sandbox em %q não pode ser suspenso", sb.State)
	}
	if err := s.launcher.Suspend(ctx, sb.Handle()); err != nil {
		return nil, err
	}
	return s.repo.Transition(ctx, accountID, sb.ID, SuspendTransition)
}

// Resume recria a execução SOBRE o workspace existente (spec §3).
//
// Destruído não retoma. É a diferença entre as duas operações materializada no
// único lugar onde ela dói: quem chama aqui esperando "desfazer" precisa ouvir
// que não há o que desfazer.
func (s *Service) Resume(ctx context.Context, id string) (*Sandbox, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("viewer não altera o ciclo de vida do sandbox")
	}
	if sb.State.IsTerminal() {
		return nil, errs.Precondition(
			"sandbox destruído não retoma — a destruição leva o workspace junto; " +
				"provisione um novo para a demanda")
	}
	if sb.State == StateActive {
		if err := s.repo.TouchActivity(ctx, accountID, sb.ID); err != nil {
			return nil, err
		}
		return sb, nil
	}
	if !CanApply(sb.State, ResumeTransition) {
		return nil, errs.Precondition("sandbox em %q não pode ser retomado", sb.State)
	}

	status, err := s.launcher.Resume(ctx, s.specFor(sb))
	if err != nil {
		return nil, err
	}
	if status.Tier != sb.Tier {
		// Retomar com isolamento diferente do declarado é a degradação
		// silenciosa entrando pela porta dos fundos: o sandbox já existia,
		// ninguém reconferiria o tier.
		return nil, errs.Internal(
			"o substrato retomou o sandbox com isolamento %q, declarado como %q",
			status.Tier, sb.Tier)
	}
	resumed, err := s.repo.Transition(ctx, accountID, sb.ID, ResumeTransition)
	if err != nil {
		return nil, err
	}
	if len(status.Endpoints) > 0 {
		return s.repo.MarkProvisioned(ctx, accountID, sb.ID, sb.Tier,
			s.endpoints(sb.DemandID, status.Endpoints))
	}
	return resumed, nil
}

// Destroy é IRREVERSÍVEL: leva execução e workspace.
//
// Idempotente por estado: destruir o que já foi destruído devolve true sem
// tocar em nada. Erro nesse caso seria hostil — quem repete a chamada quer o
// mesmo resultado, e o resultado já está lá.
func (s *Service) Destroy(ctx context.Context, id string) (bool, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return false, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return false, err
	}
	if role == identity.RoleViewer {
		return false, errs.Permission("viewer não destrói sandbox")
	}
	if sb.State.IsTerminal() {
		return true, nil
	}
	// O substrato primeiro, a linha depois. Destroy é idempotente por contrato
	// (garantia 8), então falhar ao gravar deixa um retry limpo. A ordem
	// inversa deixaria a linha dizendo "destruído" com a microVM viva e
	// faturando, e ninguém mais a procuraria.
	if err := s.launcher.Destroy(ctx, sb.Handle()); err != nil {
		return false, err
	}
	if _, err := s.repo.Transition(ctx, accountID, sb.ID, DestroyTransition); err != nil {
		return false, err
	}
	return true, nil
}

// Describe devolve o sandbox como ele ESTÁ.
//
// Consulta o substrato quando o sandbox está vivo, e não só o banco, por um
// motivo específico: o estado de cada endpoint (running/stopped) é o compose
// interno da demanda, que muda sem passar por nenhuma RPC nossa. Ler só a linha
// devolveria uma foto antiga com cara de verdade corrente.
func (s *Service) Describe(ctx context.Context, id string) (*Sandbox, error) {
	accountID, _, _, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	sb, err := s.load(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if !sb.IsLive() {
		return sb, nil
	}

	status, err := s.launcher.Describe(ctx, sb.Handle())
	if err != nil {
		if errs.KindOf(err) == errs.KindNotFound {
			// Divergência real: a linha diz que existe, o substrato não o tem.
			// Alguém apagou o namespace por fora, ou o Launch nunca terminou.
			// Dizer "ativo" aqui seria mentir para o cockpit.
			return nil, errs.Precondition(
				"o sandbox %s não existe mais no substrato (estado registrado: %s); "+
					"destrua-o e provisione outro", sb.ID, sb.State)
		}
		return nil, err
	}
	sb.Endpoints = s.endpoints(sb.DemandID, status.Endpoints)
	// O tier vem do BANCO, não do substrato: é o que foi declarado e entregue no
	// provisionamento. Deixar o substrato redeclarar a cada leitura abriria a
	// porta para o valor mudar sem que ninguém tivesse pedido.
	return sb, nil
}

// ── logs ─────────────────────────────────────────────────────────────────────

// Emitter entrega uma linha ao cliente. Erro dele encerra o fluxo — é como o
// servidor descobre que o cliente sumiu.
type Emitter func(LogLine) error

// StreamLogs segue os logs do sandbox enquanto o cliente estiver ouvindo.
//
// Dev conectado é ATIVIDADE: o toque abaixo adia a suspensão por ociosidade.
// Sem ele, o varredor de economia derrubaria o sandbox debaixo de quem está
// justamente olhando para ele (spec §3).
//
// Sandbox suspenso NÃO tem logs, e isso é recusa explícita em vez de fluxo
// vazio: a suspensão apaga a execução, e o que cada substrato ainda guarda do
// que rodou antes é diferente em cada um — o k8s apaga o pod e perde tudo, o
// Docker mantém o arquivo de log do contêiner parado. Prometer "às vezes vem
// alguma coisa" seria expor essa divergência ao cliente.
func (s *Service) StreamLogs(ctx context.Context, sandboxID string, f LogFilter, emit Emitter) error {
	accountID, _, _, err := s.caller(ctx)
	if err != nil {
		return err
	}
	sb, err := s.load(ctx, accountID, sandboxID)
	if err != nil {
		return err
	}
	switch sb.State {
	case StateDestroyed:
		return errs.Precondition("sandbox destruído não tem logs")
	case StateSuspended:
		return errs.Precondition("sandbox suspenso não tem execução; retome-o para ver logs")
	}
	if err := s.repo.TouchActivity(ctx, accountID, sb.ID); err != nil {
		return err
	}

	return s.launcher.Tail(ctx, sb.Handle(),
		ports.LogQuery{Service: f.Service, Follow: true},
		func(raw ports.LogLine) error {
			src, tt, text := Classify(raw.Text)
			line := LogLine{
				Source:   src,
				Service:  raw.Service,
				TestType: tt,
				Text:     text,
				At:       raw.At,
			}
			if line.At.IsZero() {
				line.At = s.clock.Now()
			}
			if !f.Matches(line) {
				return nil
			}
			return emit(line)
		})
}

// ── economia ─────────────────────────────────────────────────────────────────

// SweepIdle suspende os sandboxes ociosos da conta e devolve quantos suspendeu.
//
// É a spec §3 virando código: demandas esperam humanos por horas, e sandbox
// ocioso é o que separa paralelismo real de máquina afogada. Falha em um não
// interrompe os outros — um pod teimoso não pode fazer a conta inteira parar de
// economizar.
func (s *Service) SweepIdle(ctx context.Context) (int, error) {
	accountID, err := ctxutil.MustAccount(ctx)
	if err != nil {
		return 0, err
	}
	idle, err := s.repo.ListIdle(ctx, accountID, int(IdleTimeout.Seconds()))
	if err != nil {
		return 0, err
	}
	now := s.clock.Now()
	suspended := 0
	for i := range idle {
		sb := idle[i]
		if !sb.ShouldSuspend(now) {
			continue
		}
		if err := s.launcher.Suspend(ctx, sb.Handle()); err != nil {
			continue
		}
		if _, err := s.repo.Transition(ctx, accountID, sb.ID, SuspendTransition); err != nil {
			continue
		}
		suspended++
	}
	return suspended, nil
}
