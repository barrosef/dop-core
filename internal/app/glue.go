package app

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
	"github.com/Digital-Business-One/dop-core/internal/domain/hierarchy"
	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/knowledge"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
)

// Cola entre domínios.
//
// Cada domínio declara a porta ESTREITA do que precisa do vizinho, em vez de
// importar o pacote dele. O preço é este arquivo; o que se compra é que
// `demand` não sabe que `workflow` existe, e nenhum dos dois quebra quando o
// outro mudar de forma internamente.
//
// É deliberado que a cola seja chata e mecânica: no dia em que uma destas
// funções precisar de um `if` de regra, a regra está no domínio errado.

// ── workflow → demand ───────────────────────────────────────────────────────

// demandFlows resolve o fluxo efetivo para a demanda congelar.
//
// Os dois vocabulários batem STRING A STRING (`StageType`, `ArtifactKind`,
// portão e escopo), então a conversão é troca de tipo nomeado, não tradução.
// Se um dia divergirem, é aqui que quebra — e quebrar aqui é melhor do que
// silenciosamente congelar um fluxo com etapa de tipo desconhecido.
type demandFlows struct{ wf *workflow.Service }

func (a demandFlows) Resolve(ctx context.Context, _ string, scope, scopeID string) (demand.Flow, error) {
	// A conta vem do contexto no serviço de fluxo — o parâmetro accountID da
	// porta existe para o caso de outro adaptador precisar dele.
	ef, err := a.wf.Resolve(ctx, workflow.Scope(scope), scopeID)
	if err != nil {
		return demand.Flow{}, err
	}
	etapas := make([]demand.StageSpec, 0, len(ef.Flow.Stages))
	for _, s := range ef.Flow.Stages {
		artefatos := make([]demand.ArtifactKind, 0, len(s.Artifacts))
		for _, a := range s.Artifacts {
			artefatos = append(artefatos, demand.ArtifactKind(a))
		}
		etapas = append(etapas, demand.StageSpec{
			Key:       s.Key,
			Name:      s.Name,
			Type:      demand.StageType(s.Type),
			Gate:      demand.Gate(s.Gate),
			Artifacts: artefatos,
			Subtypes:  s.Subtypes,
		})
	}
	return demand.Flow{
		ID:           ef.Flow.ID,
		Name:         ef.Flow.Name,
		Version:      ef.Flow.Version,
		ResolvedFrom: ef.ResolvedFrom,
		Stages:       etapas,
	}, nil
}

// ── event → demand ──────────────────────────────────────────────────────────

// demandWatcher entrega ao domínio de demanda o fan-out que o domínio de
// evento já tem — replay, isolamento por conta e política de consumidor lento
// incluídos. Uma segunda implementação de fan-out seria uma segunda chance de
// errar isolamento entre contas.
type demandWatcher struct{ ev *event.Service }

func (a demandWatcher) Watch(ctx context.Context, since string, aggregates, types []string, emit func(ports.Event) error) error {
	return a.ev.Watch(ctx, since, event.Filter{Aggregates: aggregates, Types: types}, emit)
}

// ── identity → workflow ─────────────────────────────────────────────────────

// workflowAccess adapta identidade à porta estreita do domínio de fluxo, que
// fala PAPEL como string simples: o vocabulário de papel é do domínio de
// identidade, e importá-lo dentro de workflow acoplaria dois domínios que não
// precisam se conhecer.
type workflowAccess struct{ id *identity.Service }

func (a workflowAccess) RoleOf(ctx context.Context, userID, accountID string) (string, error) {
	m, err := a.id.Authorize(ctx, userID, accountID)
	if err != nil {
		return "", err
	}
	return string(m.Role), nil
}

// ── demand → knowledge ──────────────────────────────────────────────────────

// knowledgeDemands responde "o que esta demanda é" para o montador de contexto.
//
// O domínio de conhecimento recebe só um `demand_id` e precisa de projeto,
// título, spec, repositórios e achados — que moram em três lugares. Juntar isso
// é trabalho de composição, não de nenhum dos dois domínios.
type knowledgeDemands struct {
	demands   *demand.Service
	hierarchy *hierarchy.Service
}

func (a knowledgeDemands) ContextOf(ctx context.Context, _ string, demandID string) (*knowledge.DemandContext, error) {
	d, err := a.demands.Get(ctx, demandID)
	if err != nil {
		return nil, err
	}

	// Os achados JÁ publicados são a camada de retomada: sem eles, um agente
	// que pega a demanda no meio refaz investigação que outro concluiu — o
	// desperdício exato que o quadro de achados existe para evitar (ADR-0009).
	achados, err := a.demands.Findings(ctx, demandID)
	if err != nil {
		return nil, err
	}
	convertidos := make([]knowledge.Finding, 0, len(achados))
	for _, f := range achados {
		convertidos = append(convertidos, knowledge.Finding{
			ID:       f.ID,
			ThreadID: f.ThreadID,
			Title:    f.Title,
			Summary:  resumoDoAchado(f),
		})
	}

	// Os repositórios recortam o índice de código: o pacote traz o índice DOS
	// REPOS DA DEMANDA, nunca do projeto inteiro. Falhar aqui não vale a
	// viagem — sem a lista, o índice sai vazio em vez de sair errado.
	var repos []string
	if p, err := a.hierarchy.GetProject(ctx, d.ProjectID); err == nil {
		for _, r := range p.Repos {
			repos = append(repos, r.ID)
		}
	}

	return &knowledge.DemandContext{
		DemandID:  d.ID,
		ProjectID: d.ProjectID,
		Title:     d.Title,
		// Spec continua vazia: ela é ARTEFATO de etapa, e o armazenamento de
		// artefato por etapa ainda não existe. Degradação declarada, não
		// esquecimento — o pacote perde a spec, não fica incorreto.
		Repos:    repos,
		Findings: convertidos,
	}, nil
}

// resumoDoAchado extrai o texto do achado do payload livre.
//
// O payload é `map[string]any` porque o formato do achado é do agente que o
// publicou, não da plataforma. Aceitar as duas chaves mais prováveis e cair no
// título é melhor do que exigir esquema — achado sem resumo ainda vale mais no
// contexto do que achado ausente.
func resumoDoAchado(f demand.Finding) string {
	for _, chave := range []string{"summary", "resumo"} {
		if v, ok := f.Payload[chave].(string); ok && v != "" {
			return v
		}
	}
	return f.Title
}

// ── demand → delivery ───────────────────────────────────────────────────────

// deliveryDemands é somente LEITURA, e isso é a regra da ADR-0015 §5 virada
// tipo: a entrega não tem como parar demanda nenhuma, porque a porta não
// oferece um jeito. `Active` existe para o evento contar a verdade — "a
// diretriz foi decidida e a demanda 1 continua andando" —, nunca para decidir
// se ela para.
type deliveryDemands struct{ d *demand.Service }

func (a deliveryDemands) Demand(ctx context.Context, _ string, id string) (*delivery.DemandInfo, error) {
	dm, err := a.d.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &delivery.DemandInfo{
		ID:        dm.ID,
		ProjectID: dm.ProjectID,
		Active:    dm.Status != demand.StatusDelivered,
	}, nil
}

// ── demand → execution ──────────────────────────────────────────────────────

// executionDemands responde de quem é a demanda, e só isso.
//
// Demanda inexistente e demanda de OUTRA conta chegam aqui como o MESMO erro,
// porque `demand.Service.Get` já filtra por conta: distinguir os dois casos
// vazaria a existência de ids alheios para quem ficasse tentando.
type executionDemands struct{ d *demand.Service }

func (a executionDemands) DemandAccount(ctx context.Context, demandID string) (string, error) {
	dm, err := a.d.Get(ctx, demandID)
	if err != nil {
		return "", err
	}
	return dm.AccountID, nil
}
