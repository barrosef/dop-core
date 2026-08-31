// Package hierarchy é o domínio da árvore do cockpit: workspaces e os projetos
// que vivem dentro deles.
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK nenhum. Ele
// declara o que precisa como PORTA (repository.go) e o composition root liga.
package hierarchy

import (
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// Workspace agrupa projetos e pertence a UMA conta. A conta é o limite de
// isolamento: nada atravessa de uma conta para outra (ADR-0017).
type Workspace struct {
	ID          string
	AccountID   string
	Name        string
	Key         string // opcional — prefixo curto que rotula o workspace na UI
	Description string
	Tags        []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Project é a unidade de trabalho. A conta NÃO é declarada por ele: vem
// herdada do workspace. Deixar o cliente escolher a conta do projeto abriria
// justamente o buraco que o isolamento multi-tenant existe para fechar.
type Project struct {
	ID          string
	AccountID   string // herdada do workspace, nunca informada pelo chamador
	WorkspaceID string
	Name        string
	Description string
	Repos       []ProjectRepo
	TaskManager *ProjectTaskManager // opcional: nem todo projeto tem quadro
	Resources   []string            // ids de recursos anexados (skill, workflow, git_flow)
	Rules       []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ProjectRepo é um repositório anexado ao projeto.
//
// O provedor é do REPOSITÓRIO, não do projeto (ADR-0013): é por isso que
// IntegrationID mora aqui e não no Project. Sem essa escolha, um projeto que
// tem um repo no GitHub e outro no GitLab seria impossível de representar.
type ProjectRepo struct {
	ID            string
	IntegrationID string
	ExternalID    string
	Name          string
	DefaultBranch string
	PRTargets     []string
}

// DefaultBranch aplicado quando o chamador não informa — espelha o default da
// coluna, para que domínio e banco não discordem.
const DefaultBranch = "main"

// ProjectTaskManager é o vínculo com o quadro do provedor.
//
// ExternalSpaceID guarda o "espaço" do provedor — nunca a palavra workspace
// nua: workspace já é conceito NOSSO, e confundir os dois é o caminho curto
// para ligar o projeto ao quadro errado.
type ProjectTaskManager struct {
	IntegrationID     string
	ExternalSpaceID   string
	ExternalProjectID string
	CardTypes         []string // dinâmicos: quem manda no vocabulário é o provedor
}

// TreeNode é um workspace com os projetos que ele contém. Existe para que a
// árvore do cockpit venha em UMA resposta — ver Repository.Tree.
type TreeNode struct {
	Workspace Workspace
	Projects  []Project
}

// ReferencedResourceIDs lista TODO recurso citado pelo projeto: integrações
// dos repositórios, integração do gerenciador de tarefas e recursos anexados.
//
// É a entrada da checagem de conta — por isso devolve tudo junto, sem
// distinguir a origem: qualquer id que escape da conta dona é um problema.
func (p Project) ReferencedResourceIDs() []string {
	seen := make(map[string]bool, len(p.Repos)+len(p.Resources)+1)
	out := make([]string, 0, len(p.Repos)+len(p.Resources)+1)
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, r := range p.Repos {
		add(r.IntegrationID)
	}
	if p.TaskManager != nil {
		add(p.TaskManager.IntegrationID)
	}
	for _, id := range p.Resources {
		add(id)
	}
	return out
}

// ── regras de nome e chave ───────────────────────────────────────────────────

const (
	nameMaxLen = 120
	keyMinLen  = 2
	keyMaxLen  = 12
)

// ValidateName vale para workspace e projeto: nome é obrigatório. Sem nome, a
// árvore do cockpit vira uma lista de itens em branco.
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errs.Invalid("nome é obrigatório")
	}
	if len(name) > nameMaxLen {
		return errs.Invalid("nome pode ter no máximo %d caracteres", nameMaxLen)
	}
	return nil
}

// NormalizeKey produz uma chave válida a partir de texto livre: maiúscula e
// alfanumérica.
func NormalizeKey(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidateKey aceita chave VAZIA — ela é opcional. Se vier, precisa ser curta
// e maiúscula alfanumérica: a chave aparece como prefixo em rótulos e
// referências, e prefixo longo ou com pontuação some da tela.
func ValidateKey(k string) error {
	if k == "" {
		return nil
	}
	if len(k) < keyMinLen {
		return errs.Invalid("a chave precisa de ao menos %d caracteres", keyMinLen)
	}
	if len(k) > keyMaxLen {
		return errs.Invalid("a chave pode ter no máximo %d caracteres", keyMaxLen)
	}
	if NormalizeKey(k) != k {
		return errs.Invalid("a chave aceita apenas letras maiúsculas e números")
	}
	return nil
}
