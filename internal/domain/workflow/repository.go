package workflow

import "context"

// Repository é a PORTA de persistência do domínio de fluxo.
//
// Declarada aqui, em linguagem de domínio; implementada em
// internal/adapter/postgres. O domínio nunca vê SQL.
//
// Toda operação recebe accountID explicitamente: isolamento multi-tenant é
// parâmetro obrigatório da porta, não algo que o adaptador possa esquecer. A
// única exceção é o CATÁLOGO DA PLATAFORMA, que não tem dono — leitura o
// enxerga sempre, escrita nunca o alcança.
//
// Repare no que NÃO existe aqui: nenhuma operação que altere uma versão
// gravada. Não é esquecimento — é a invariante do domínio expressa na forma da
// porta. O caminho para mudar um fluxo é AppendVersion, e só.
type Repository interface {
	// List devolve os fluxos da conta. scope vazio lista todos os níveis;
	// ownerID vazio lista todos os donos daquele nível.
	List(ctx context.Context, accountID string, scope Scope, ownerID string) ([]Flow, error)

	// ByID devolve a versão CORRENTE do fluxo.
	ByID(ctx context.Context, accountID, id string) (*Flow, error)

	// VersionOf devolve uma versão específica — congelada, exatamente como foi
	// gravada. É por aqui que uma demanda em andamento lê o fluxo que ela
	// congelou ao iniciar (ADR-0014 §4).
	VersionOf(ctx context.Context, accountID, id string, version int32) (*Flow, error)

	// ByOwners devolve a versão corrente do fluxo de CADA nível pedido, numa
	// consulta só.
	//
	// Existe como operação própria — e não como laço sobre ByID — porque a
	// resolução acontece a cada abertura de demanda e a cadeia tem cinco
	// níveis: seriam cinco idas ao banco na tela mais quente do produto.
	// Níveis sem fluxo declarado simplesmente não voltam.
	ByOwners(ctx context.Context, accountID string, refs []ScopeRef) ([]Flow, error)

	// Create grava o fluxo e a versão 1. idempotencyKey é obrigatória: repetir
	// a chamada devolve o que já foi criado em vez de criar um segundo fluxo.
	Create(ctx context.Context, f *Flow, idempotencyKey string) (*Flow, error)

	// AppendVersion grava a versão SEGUINTE sem tocar na anterior. baseVersion
	// é a versão sobre a qual o autor trabalhou: se o fluxo já avançou, a
	// escrita é recusada como conflito em vez de sobrescrever o trabalho alheio.
	AppendVersion(ctx context.Context, accountID string, f *Flow, baseVersion int32, idempotencyKey string) (*Flow, error)

	// Promote publica o conteúdo de src no nível alvo: cria o fluxo do alvo se
	// ele não tiver nenhum, ou acrescenta uma versão ao que já existe.
	//
	// Publica, não move: o fluxo de origem continua onde está. Mover apagaria
	// o fluxo sob os pés das demandas que já o adotaram.
	Promote(ctx context.Context, accountID string, src *Flow, target ScopeRef, idempotencyKey string) (*Flow, error)
}

// Ancestry é a porta ESTREITA para a árvore da conta: dado um nível concreto,
// qual é a cadeia de níveis acima dele.
//
// Este domínio precisa de UMA coisa da hierarquia e da demanda — a linhagem —
// e não de projeto, workspace nem demanda como entidades. Declarar a porta com
// essa superfície é o que impede o fluxo de virar cliente de outros dois
// domínios por causa de uma consulta.
//
// A cadeia volta do MAIS GENÉRICO ao MAIS ESPECÍFICO, incluindo o alvo, e
// sempre começando na plataforma:
//
//	demanda d ⇒ [plataforma, conta a, workspace w, projeto p, demanda d]
//
// Alvo que não existe, ou que pertence a outra conta, é NotFound: quem não é
// da conta nem deveria descobrir que o id existe.
type Ancestry interface {
	ChainOf(ctx context.Context, accountID string, target ScopeRef) ([]ScopeRef, error)
}

// Access é a porta ESTREITA para o domínio de identidade: promover um fluxo
// para o nível acima exige `manage`, e para saber isso basta o PAPEL do ator na
// conta ativa. Nada além disso.
//
// O papel viaja como string simples de propósito. O vocabulário é do domínio de
// identidade, e importar o tipo dele aqui acoplaria os dois pacotes por uma
// única comparação — o composition root liga com três linhas de cola e cada
// domínio continua entendendo apenas o que precisa.
type Access interface {
	RoleOf(ctx context.Context, userID, accountID string) (string, error)
}

// Papéis com `manage` implícito sobre o conteúdo da conta (ADR-0013): owner e
// admin. Sem isso, ninguém consegue consertar um fluxo publicado por quem já
// saiu da empresa.
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
)

func canManage(role string) bool { return role == RoleOwner || role == RoleAdmin }
