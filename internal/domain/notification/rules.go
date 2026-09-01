package notification

import (
	"strings"
	"time"
)

// ════════════════════════════════════════════════════════════════════════════
// A TABELA. É ela o decisor.
//
// Mesma forma de `attention.impacto` e de `cost.routingTable`, e pelo mesmo
// motivo: uma política precisa ser AUDITÁVEL e SUBSTITUÍVEL de uma vez só.
// Quinze `if` espalhados por chamadores dão a mesma resposta hoje e são
// impossíveis de recalibrar amanhã — e ninguém consegue explicar por que um
// e-mail saiu.
//
// Aqui a exigência é mais forte do que nos dois primos: no P-29 esta tabela
// deixa de ser código e vira DADO. Por isso cada linha é preenchível por um
// carregador burro — não há função, closure nem `switch` dentro de linha
// nenhuma. O que parece rigidez (copiar campo do payload por NOME, montar o
// link por CAMINHO) é o que torna a troca um carregador em vez de uma
// reescrita.
//
// ANTES DE ACRESCENTAR LINHA, a pergunta é a mesma da caixa de atenção, um
// degrau acima: "isto merece ATRAPALHAR a pessoa fora da plataforma?". A caixa
// já filtra o que exige decisão; o e-mail filtra o que não pode esperar a
// pessoa voltar ao cockpit.
// ════════════════════════════════════════════════════════════════════════════

// Tipos de evento que a tabela conhece. Constante nomeada, e não literal na
// linha, para que o compilador ajude quando um evento for renomeado.
const (
	EvInviteCreated = "dop.identity.invite.created"
)

// recipientSource é DE ONDE sai o destinatário. São duas origens porque são
// dois mundos: quem já é usuário da plataforma (a conta sabe o e-mail) e quem
// ainda não é (o e-mail só existe no evento).
type recipientSource string

const (
	// fromPayload: o endereço vem de um campo do payload do evento. É o caso do
	// convite, e é o único jeito possível — o convidado AINDA NÃO É USUÁRIO, e
	// não há de onde a plataforma resolver o endereço dele.
	fromPayload recipientSource = "payload"
	// fromAccountMembers: o endereço vem de quem tem vínculo com a conta.
	fromAccountMembers recipientSource = "account_members"
)

// recipientSpec é declarativa de propósito: `Field` é um NOME de campo, não uma
// função de extração. Ver o cabeçalho.
type recipientSpec struct {
	Source recipientSource
	Field  string // usado só com fromPayload
}

// Rule é uma LINHA da tabela.
type Rule struct {
	// Name é o nome ENDEREÇÁVEL da regra, e ele entra na chave de idempotência.
	// Renomear regra em produção reabre tudo o que ela já enviou — o nome é
	// identidade, não rótulo.
	Name    string
	Trigger Trigger
	// Event só vale com TriggerEvent.
	Event  string
	Action Action
	Kind   Kind

	Recipients recipientSpec
	// Data são as chaves do payload copiadas para os dados do template, por
	// NOME. Não há transformação: o que o adaptador recebe é o que o evento
	// trouxe. Transformar aqui traria vocabulário de apresentação para dentro
	// da política.
	Data []string
	// LinkPath é o caminho relativo do cockpit para onde o aviso leva. Relativo
	// porque a base é da INSTALAÇÃO (Config.BaseURL), não da política — a mesma
	// regra vale no SaaS e num self-hosted com outro domínio.
	LinkPath string
	// Delay só vale com TriggerAttentionBox. Zero usa DefaultDigestDelay.
	Delay time.Duration
	// Why é o porquê da LINHA, não a repetição do que ela faz. Sem isto a
	// política não é auditável: ninguém consegue discordar de
	// "invite.created → e-mail", e qualquer um consegue discordar da razão.
	Why string
}

// tabela — A TABELA. Slice e não map: a ordem é a da leitura, e duas regras
// para o mesmo evento (que o P-29 vai permitir) precisam de ordem definida.
var tabela = []Rule{
	{
		Name:       "convite-criado",
		Trigger:    TriggerEvent,
		Event:      EvInviteCreated,
		Action:     ActionEmail,
		Kind:       KindInvite,
		Recipients: recipientSpec{Source: fromPayload, Field: "email"},
		Data:       []string{"email", "role"},
		LinkPath:   "/convites",
		Why: "é a única notificação cujo destinatário AINDA NÃO É USUÁRIO: ele não " +
			"tem cockpit para olhar, não tem caixa de atenção, e o convite não " +
			"existe para ele até chegar por fora. Sem este e-mail o convite é " +
			"um registro que ninguém vê",
	},
	{
		Name:       "resumo-da-caixa",
		Trigger:    TriggerAttentionBox,
		Action:     ActionEmail,
		Kind:       KindAttentionDigest,
		Recipients: recipientSpec{Source: fromAccountMembers},
		LinkPath:   "/atencao",
		Delay:      DefaultDigestDelay,
		Why: "o aviso liga na CAIXA e não nos eventos crus — a caixa já decide o " +
			"que exige decisão humana, e um segundo mapa começaria igual e " +
			"divergiria no primeiro ajuste. O atraso existe porque um e-mail por " +
			"item torna a caixa de entrada inútil, e caixa ignorada não protege " +
			"ninguém (risco R-1 da spec)",
	},
}

// Rules devolve a política. Cópia, e não a fatia: política que o chamador
// consegue editar em memória deixa de ser política.
//
// É AQUI que o P-29 entra. Hoje devolve o literal acima; no dia em que a reação
// for dado, esta função lê do banco e nenhum chamador muda — nem `Apply`, nem
// `Kinds`, nem `Subjects`, nem a suíte de contrato.
func Rules() []Rule {
	out := make([]Rule, len(tabela))
	copy(out, tabela)
	return out
}

// Subjects é o que o consumidor assina, DERIVADO da tabela.
//
// Derivado, e não escrito à mão, porque a divergência entre os dois é
// silenciosa nos dois sentidos: assinatura a menos faz o evento nunca chegar
// (ninguém recebe e nada falha), assinatura a mais desperdiça entrega. É o
// mesmo raciocínio de `attention.Subjects`, com a diferença de que lá a lista é
// literal — aqui ela não pode ser, porque a tabela vai virar dado.
func Subjects() []string {
	vistos := map[string]bool{}
	out := make([]string, 0, len(tabela))
	for _, r := range tabela {
		if r.Trigger != TriggerEvent || r.Event == "" || vistos[r.Event] {
			continue
		}
		vistos[r.Event] = true
		out = append(out, r.Event)
	}
	return out
}

// DigestRule devolve a regra do aviso de atenção, se houver.
//
// A varredura precisa dela pelo NOME, e não por posição: a chave de
// idempotência gravada carrega o nome da regra, e uma varredura que assumisse
// "a segunda linha da tabela" gravaria chave errada no dia em que alguém
// reordenasse o literal.
func DigestRule() (Rule, bool) {
	for _, r := range tabela {
		if r.Trigger == TriggerAttentionBox {
			return r, true
		}
	}
	return Rule{}, false
}

// Apply traduz um evento em comandos.
//
// Devolve fatia — e não um comando — desde o primeiro dia: com reação
// declarativa um evento poderá disparar N ações, e uma assinatura que devolve
// um só forçaria a mudar todo chamador junto com a política.
//
// Devolve VAZIO para a esmagadora maioria dos eventos, que é o caso normal.
func Apply(e Event, resolve func(recipientSpec, Event) []Recipient) []Command {
	if e.AccountID == "" || e.ID == "" {
		// Evento sem conta (`user.ensured`, migração 0003) não pertence a
		// notificação nenhuma: não há membros a avisar nem conta em nome de
		// quem avisar.
		return nil
	}
	var out []Command
	for _, r := range tabela {
		if r.Trigger != TriggerEvent || r.Event != e.Type {
			continue
		}
		dest := resolve(r.Recipients, e)
		if len(dest) == 0 {
			// Sem destinatário não há o que disparar. Não é erro — ver
			// Command.Valid.
			continue
		}
		out = append(out, Command{
			AccountID:  e.AccountID,
			EventID:    e.ID,
			Rule:       r.Name,
			Action:     r.Action,
			Kind:       r.Kind,
			Recipients: dest,
			Data:       dadosDoEvento(r, e),
		})
	}
	return out
}

// dadosDoEvento copia, por NOME, o que a linha pediu. Chave ausente vira campo
// ausente, nunca erro: o template decide o que fazer com a falta, e derrubar um
// convite porque um campo cosmético não veio trocaria um problema de aparência
// por um bloqueio de acesso.
func dadosDoEvento(r Rule, e Event) map[string]any {
	d := map[string]any{}
	for _, chave := range r.Data {
		if v, ok := e.Payload[chave]; ok {
			d[chave] = v
		}
	}
	return d
}

// PayloadEmail extrai o endereço de um campo do payload. Exportada porque quem
// resolve destinatário é o serviço (que conhece o repositório), não a tabela.
func payloadEmail(spec recipientSpec, e Event) []Recipient {
	if spec.Source != fromPayload || spec.Field == "" {
		return nil
	}
	v, _ := e.Payload[spec.Field].(string)
	v = strings.TrimSpace(v)
	if !enderecoValido(v) {
		return nil
	}
	return []Recipient{{Email: v}}
}
