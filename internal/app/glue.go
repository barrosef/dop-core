package app

import (
	"context"

	"github.com/Digital-Business-One/dop-core/internal/domain/delivery"
	"github.com/Digital-Business-One/dop-core/internal/domain/demand"
	"github.com/Digital-Business-One/dop-core/internal/domain/event"
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
	demands *demand.Service
}

func (a knowledgeDemands) ContextOf(ctx context.Context, _ string, demandID string) (*knowledge.DemandContext, error) {
	d, err := a.demands.Get(ctx, demandID)
	if err != nil {
		return nil, err
	}
	return &knowledge.DemandContext{
		DemandID:  d.ID,
		ProjectID: d.ProjectID,
		Title:     d.Title,
		// Spec, Repos e Findings ficam vazios por ora, e isso é DEGRADAÇÃO
		// declarada, não esquecimento: o pacote de contexto perde a camada de
		// retomada (o agente refaz investigação que já fora feita) mas continua
		// correto. Preencher exige listagem de achados por demanda, que o
		// repositório ainda não expõe, e os repos do projeto via hierarquia.
	}, nil
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
