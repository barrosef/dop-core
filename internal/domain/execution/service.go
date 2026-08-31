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
		// Linha em provisioning é rastro de uma tentativa que não terminou —
		// uma queda entre as duas transações. Devolvê-la como está entregaria
		// ao cliente um sandbox pela metade que nunca mais seria consertado;
		// retomar dali é o que torna a segunda transação idempotente de fato.
		if live.State != StateProvisioning {
			return live, nil
		}
		return s.finishProvision(ctx, accountID, live, tier)
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

	return s.finishProvision(ctx, accountID, created, tier)
}

// finishProvision executa a SEGUNDA metade do provisionamento: sobe o sandbox e
// registra o que o substrato entregou. Vive separada porque é exatamente o
// trecho que precisa ser refeito quando a primeira tentativa morreu no meio.
func (s *Service) finishProvision(ctx context.Context, accountID string, sb *Sandbox, tier ports.IsolationTier) (*Sandbox, error) {
	status, err := s.launcher.Launch(ctx, s.specFor(sb))
	if err != nil {
		// A linha fica em provisioning de propósito: ela é o rastro de que
		// alguém tentou. Apagá-la aqui esconderia um sandbox meio subido.
		return nil, err
	}
	if status.Tier != tier {
		// O adaptador quebrou a garantia 1 da porta. Desfazer é obrigatório:
		// entregar isolamento diferente do declarado é pior que não entregar.
		_ = s.launcher.Destroy(ctx, sb.Handle())
		_, _ = s.repo.Transition(ctx, accountID, sb.ID, DestroyTransition)
		return nil, errs.Internal(
			"o substrato entregou isolamento %q para um pedido de %q — sandbox descartado",
			status.Tier, tier)
	}
	return s.repo.MarkProvisioned(ctx, accountID, sb.ID, status.Tier,
		s.endpoints(sb.DemandID, status.Endpoints))
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
	// Os endpoints do retorno vêm do substrato, mas NÃO são regravados: fazer
	// isso emitiria um segundo "provisionado" para um sandbox que só foi
	// retomado, e toda projeção passaria a contar dois provisionamentos onde
	// houve um. Endpoint é estado corrente, lido pelo Describe.
	resumed.Endpoints = s.endpoints(sb.DemandID, status.Endpoints)
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

// ── execução de comando ──────────────────────────────────────────────────────

// RunCommand roda um comando no sandbox VIVO da demanda.
//
// É por aqui que o agente age (ADR-0023 + spec do substrato §4): o runtime de
// agente pergunta pela DEMANDA, que é o vocabulário dele, e este domínio resolve
// demanda → sandbox → substrato. O runtime nunca vê um id de sandbox, nunca vê
// `ports.SandboxLauncher` e nunca escolhe onde o comando roda.
//
// Três decisões que não são óbvias:
//
//  1. O ERRO É SÓ DO SUBSTRATO. Comando que sai com código != 0, que estoura o
//     prazo ou que tem a saída cortada volta em `ExecResult` com erro nil —
//     é a garantia 15 da porta, propagada intacta. O agente PRECISA ver que o
//     teste reprovou para consertar; devolver isso como erro tiraria dele a
//     única informação que resolve o problema;
//
//  2. TRABALHO DE AGENTE É ATIVIDADE. O toque adia a suspensão por ociosidade
//     (spec §3). Sem ele, o varredor de economia derrubaria o sandbox debaixo
//     de um agente que está justamente trabalhando nele — e o sintoma seria um
//     laço de ferramenta que falha na volta seguinte por "sandbox suspenso",
//     sem nada explicando por quê;
//
//  3. VIEWER NÃO RODA COMANDO. Rodar comando no sandbox é escrever no workspace
//     da demanda e gastar o tempo de uma máquina que a conta paga. É a mesma
//     linha que separa viewer de quem provisiona.
func (s *Service) RunCommand(ctx context.Context, demandID string, req ports.ExecRequest) (*ports.ExecResult, error) {
	accountID, _, role, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(demandID) == "" {
		return nil, errs.Invalid("demanda não informada")
	}
	if len(req.Command) == 0 {
		return nil, errs.Invalid("comando não informado")
	}
	if role == identity.RoleViewer {
		return nil, errs.Permission("viewer não executa comando no sandbox")
	}

	sb, err := s.repo.LiveByDemand(ctx, accountID, demandID)
	if err != nil {
		return nil, err
	}
	if sb == nil {
		// Demanda sem sandbox e demanda de outra conta saem iguais, pelo mesmo
		// motivo de `load`: a segunda resposta confirmaria que o id existe.
		return nil, errs.NotFound("sandbox da demanda %s", demandID)
	}
	if sb.State != StateActive {
		return nil, errs.Precondition(
			"o sandbox da demanda %s está em %q e não executa comando; retome-o antes",
			demandID, sb.State)
	}

	if err := s.repo.TouchActivity(ctx, accountID, sb.ID); err != nil {
		return nil, err
	}
	return s.launcher.Exec(ctx, sb.Handle(), req)
}

// ── logs ─────────────────────────────────────────────────────────────────────

// streamTailLines é quanto de histórico acompanha a reconexão.
//
// Sem teto, abrir os logs de um sandbox que roda há horas despejaria o log
// inteiro antes da primeira linha nova — e o dev que só queria ver o que está
// acontecendo agora esperaria por megabytes. O histórico profundo é assunto da
// projeção de timeline, não deste fluxo.
const streamTailLines = 500

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
		ports.LogQuery{Service: f.Service, Follow: true, TailLines: streamTailLines},
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

// NewSweeper monta o serviço só para VARRER.
//
// O scheduler não atende ninguém: ele não autoriza chamador nem consulta
// demanda, só suspende o que está parado. Montar o grafo inteiro lá dentro para
// satisfazer construtor exigiria inventar dependências que a varredura não usa
// — e dependência inventada é dependência que um dia alguém passa a usar.
//
// O serviço devolvido PANICA se alguém chamar Provision ou qualquer coisa que
// precise de ator: é erro de montagem, e falhar alto é melhor que autorizar com
// um duplo vazio.
func NewSweeper(repo Repository, launcher ports.SandboxLauncher, clock ports.Clock) *Service {
	if repo == nil || launcher == nil || clock == nil {
		panic("execution.NewSweeper: repositório, launcher e relógio são obrigatórios")
	}
	return &Service{repo: repo, launcher: launcher, clock: clock}
}

// SweepAllAccounts é o varredor de economia rodando como SISTEMA.
//
// O scheduler não tem conta ativa — e todo o resto deste domínio exige uma. A
// saída é visitar conta por conta: `AccountsWithIdle` diz QUAIS têm o que
// varrer, e cada varredura acontece com aquela conta no contexto, pelo mesmo
// caminho que uma chamada de usuário faria. O isolamento não é afrouxado; o que
// muda é quem decide a ordem de visita.
//
// Erro numa conta não interrompe as outras: sandbox ocioso de uma conta não
// deve ficar aceso porque a conta anterior tem um problema.
func (s *Service) SweepAllAccounts(ctx context.Context) (contas, suspensos int, err error) {
	ids, err := s.repo.AccountsWithIdle(ctx, int(IdleTimeout.Seconds()))
	if err != nil {
		return 0, 0, err
	}
	for _, accountID := range ids {
		// Ator de sistema, com a conta da vez: é o mesmo Call que o
		// interceptor montaria, e é o que faz MustAccount funcionar sem abrir
		// exceção no domínio.
		porConta := ctxutil.Into(ctx, ctxutil.Call{
			AccountID: accountID,
			ActorID:   "scheduler",
			ActorKind: ctxutil.ActorSystem,
		})
		n, err := s.SweepIdle(porConta)
		if err != nil {
			continue
		}
		contas++
		suspensos += n
	}
	return contas, suspensos, nil
}
