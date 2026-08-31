// Package resource é o domínio dos recursos da conta: integrações, skills,
// workflows e fluxos git — a unidade de posse e compartilhamento (ADR-0013).
//
// Regra da casa: este pacote não conhece Postgres, gRPC nem SDK nenhum. Ele
// declara o que precisa como PORTA (repository.go) e o composition root liga.
//
// A distinção que organiza tudo aqui é a NATUREZA do recurso: recurso com
// credencial carrega risco; recurso de conteúdo carrega conhecimento. Os dois
// não podem ter o mesmo default de acesso (ADR-0014 §6).
package resource

import (
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/identity"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

type Kind string

const (
	KindIntegration Kind = "integration"
	KindSkill       Kind = "skill"
	KindWorkflow    Kind = "workflow"
	KindGitFlow     Kind = "git_flow"
)

// ValidKind aceita apenas os quatro tipos do enum resource_kind do banco —
// tipo fora do vocabulário é erro de contrato, não dado do usuário.
func ValidKind(k Kind) bool {
	switch k {
	case KindIntegration, KindSkill, KindWorkflow, KindGitFlow:
		return true
	}
	return false
}

// HasCredential: só integração tem credencial. É o predicado que decide o
// default de acesso, o versionamento e se SetCredential faz sentido — três
// regras diferentes penduradas na MESMA distinção, o que é sinal de que a
// distinção é a certa.
func (k Kind) HasCredential() bool { return k == KindIntegration }

// IsContent é o outro lado: skill, workflow e git_flow são conhecimento
// escrito, versionável e compartilhável dentro da conta.
func (k Kind) IsContent() bool { return ValidKind(k) && !k.HasCredential() }

// Category classifica a integração. Não existe categoria fora destas três:
// git (onde o código mora), task_manager (de onde a demanda vem) e agent
// (quem executa — Claude, Codex, Google Code Assist).
type Category string

const (
	CategoryGit         Category = "git"
	CategoryTaskManager Category = "task_manager"
	CategoryAgent       Category = "agent"
)

func ValidCategory(c Category) bool {
	switch c {
	case CategoryGit, CategoryTaskManager, CategoryAgent:
		return true
	}
	return false
}

// Level é o nível de uma concessão. São dois, e de propósito: mais níveis
// viram matriz de permissão que ninguém sabe explicar.
type Level string

const (
	LevelNone   Level = "" // ausência de acesso — nunca é gravado, só devolvido
	LevelUse    Level = "use"
	LevelManage Level = "manage"
)

func ValidLevel(l Level) bool { return l == LevelUse || l == LevelManage }

// AtLeast: manage inclui use. Quem gerencia a integração também a usa —
// o contrário não vale.
func (l Level) AtLeast(want Level) bool {
	switch want {
	case LevelUse:
		return l == LevelUse || l == LevelManage
	case LevelManage:
		return l == LevelManage
	}
	return false
}

type Resource struct {
	ID            string
	AccountID     string
	Kind          Kind
	Name          string
	Version       int32
	Config        map[string]any
	CredentialRef string // ponteiro OPACO ao SecretStore — nunca o segredo
	Status        string
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (r Resource) HasCredential() bool { return r.Kind.HasCredential() }

// IsVersioned: conteúdo é versionado, credencial não.
//
// Skill e workflow são texto que alguém escreveu e que outra pessoa vai
// executar amanhã — sobrescrever apaga a resposta de "com qual versão isso
// rodou?". Já a configuração de uma integração é estado corrente do mundo
// externo: versionar o base_url de ontem não serve a ninguém.
func (r Resource) IsVersioned() bool { return r.Kind.IsContent() }

type Grant struct {
	ID         string
	ResourceID string
	UserID     string
	Level      Level
	GrantedBy  string
	CreatedAt  time.Time
}

// ── credencial ───────────────────────────────────────────────────────────────

// CredentialKind é o Kind da SecretRef de credencial de integração. Fixo: o
// domínio não inventa taxonomia de cofre.
const CredentialKind = "integration_credential"

// SecretRefFor monta a referência LÓGICA do segredo. O domínio não conhece
// caminho, namespace nem nome de segredo — só o adaptador sabe resolvê-la
// (ADR-0001).
func SecretRefFor(accountID, resourceID string) ports.SecretRef {
	return ports.SecretRef{
		AccountID: accountID,
		Kind:      CredentialKind,
		OwnerID:   resourceID,
	}
}

// CredentialRef é o que fica GRAVADO na linha do recurso: um rótulo opaco,
// derivável da própria linha. Ele não é caminho de cofre e não abre nada —
// serve para responder "esta integração já tem credencial?" e para a auditoria
// registrar QUAL credencial autorizou uma ação, sem jamais tocar no valor.
func CredentialRef(accountID, resourceID string) string {
	return CredentialKind + ":" + accountID + ":" + resourceID
}

// ── o coração: acesso efetivo ────────────────────────────────────────────────

// EffectiveLevel resolve o nível do ator sobre um recurso. É a única função do
// sistema que responde "esta pessoa pode?" — e por isso está aqui, no domínio,
// testável sem banco.
//
// A ordem das cláusulas É a regra:
//
//  1. owner e admin têm manage IMPLÍCITO em todo recurso da conta. Sem isso
//     surge o cenário em que a integração do GitHub quebra, quem a conectou
//     saiu da empresa e ninguém — nem o dono da conta — consegue consertar.
//  2. concessão explícita vale a seguir, para qualquer natureza de recurso.
//  3. sem concessão, decide a NATUREZA (ADR-0014 §6): credencial é risco,
//     conhecimento é conhecimento.
func EffectiveLevel(r Resource, role identity.Role, accountKind identity.AccountKind, explicit *Grant) Level {
	if role.HasImplicitManage() {
		return LevelManage
	}
	if explicit != nil && ValidLevel(explicit.Level) {
		return explicit.Level
	}
	if r.HasCredential() {
		// FECHADO por natureza. Uma credencial é a chave de um sistema de
		// terceiros: quem não recebeu a chave explicitamente não a tem.
		return LevelNone
	}
	if accountKind == identity.AccountOrganization {
		// ABERTO por natureza, dentro da conta. Skill e workflow existem para
		// serem usados pelo time; exigir concessão para cada membro ler uma
		// skill transforma conhecimento em burocracia. Continua restringível
		// por concessão explícita, que a cláusula 2 já honra.
		return LevelUse
	}
	// Conta pessoal: ela tem exatamente um membro, e esse membro é owner —
	// logo a cláusula 1 já respondeu. Chegar aqui significa vínculo que não
	// existe. É daí que sai, de graça, a regra "recurso de conta pessoal nunca
	// é compartilhável": não há para quem compartilhar.
	return LevelNone
}

// ── validação de entrada ─────────────────────────────────────────────────────

const nameMaxLen = 120

// ValidateName: nome é a chave natural do recurso dentro do par (conta, tipo),
// então precisa ser estável e legível.
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errs.Invalid("o recurso precisa de um nome")
	}
	if len(name) > nameMaxLen {
		return errs.Invalid("o nome do recurso pode ter no máximo %d caracteres", nameMaxLen)
	}
	return nil
}

// IntegrationSpec é a leitura tipada do config de uma integração — o que o
// proto descreve como IntegrationSpec e o banco guarda em jsonb.
type IntegrationSpec struct {
	Category Category
	Provider string
	BaseURL  string
}

// ParseIntegration valida o config de uma integração no momento da escrita.
//
// Integração sem categoria e sem provedor é linha inútil: ninguém sabe se
// aquilo é um GitHub, um Jira ou um agente, e o roteamento de execução depende
// exatamente disso. Validar na escrita evita descobrir na hora do deploy.
func ParseIntegration(config map[string]any) (IntegrationSpec, error) {
	var spec IntegrationSpec
	spec.Category = Category(strings.TrimSpace(str(config["category"])))
	spec.Provider = strings.TrimSpace(str(config["provider"]))
	spec.BaseURL = strings.TrimSpace(str(config["base_url"]))

	if !ValidCategory(spec.Category) {
		return spec, errs.Invalid(
			"categoria de integração inválida: %q (use git, task_manager ou agent)", spec.Category)
	}
	if spec.Provider == "" {
		return spec, errs.Invalid("a integração precisa declarar o provedor")
	}
	return spec, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
