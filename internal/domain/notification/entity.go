// Package notification é o GATILHO da comunicação (ADR-0025).
//
// Nas palavras do dono do produto: *"Mailer viver sem o Notifier seria como se
// uma bala pudesse ser disparada sem o gatilho."* O ponto não é que o canal
// possa viver sozinho — é que **o gatilho existe de qualquer jeito**. Se não for
// projetado, alguém improvisa: o caso de uso de convite chama o `Mailer` direto
// e vira o gatilho, sem nome e sem lugar, espalhado por quantos casos de uso
// mandarem e-mail. O risco não é "canal sem gatilho", é GATILHO DIFUSO.
//
//	consumidor de evento
//	   └─ Notifier: decide O QUE notificar e PARA QUEM   ← este pacote
//	        └─ comando: (tipo, destinatário, dados)
//	             └─ ports.Mailer                          ← o disparo
//	                  └─ SendGrid | SMTP
//
// O caso de uso de convite não conhece nenhum dos dois: publica evento e acabou.
//
// ── Por que decisor e executor nascem separados ─────────────────────────────
//
// Porque a reação a evento vira DADO no P-29: o dono do produto quer uma
// estrutura mapeando evento → ação(ões), e as ações não serão só e-mail. O que
// já foi acordado é que a comunicação nasça no formato que essa mudança vai
// exigir — decisor separado do executor, ação endereçável por NOME, e chave de
// idempotência derivada de (evento, regra, ação), não só do evento.
//
// É por isso que a decisão é uma TABELA (rules.go) e não um `switch`: a troca
// tem que ser de CARREGADOR, não reescrita. `Apply` lê `Rules()`; no dia em que
// as regras vierem do banco, `Rules()` passa a lê-las de lá e nada mais muda.
package notification

import (
	"sort"
	"strings"
	"time"
)

// Kind é o TIPO da notificação — o vocabulário que o domínio sabe emitir e que
// todo adaptador de canal precisa saber resolver.
//
// Não confundir com `Action` (por onde sai) nem com template (como fica): o
// tipo é a INTENÇÃO. "Alguém foi convidado" é a mesma intenção no SendGrid, no
// SMTP e num futuro OneSignal; o que muda é o catálogo do outro lado.
type Kind string

const (
	// KindInvite — transacional: dispara na hora, sempre, um por evento.
	KindInvite Kind = "invite"
	// KindAttentionDigest — aviso de atenção, com atraso e agrupado. Liga na
	// CAIXA (ADR-0006), não nos eventos crus. Ver DefaultDigestDelay.
	KindAttentionDigest Kind = "attention_digest"
)

// Action é O QUE se faz quando a regra casa, endereçável por NOME.
//
// Hoje só existe e-mail e mesmo assim a ação é um valor nomeado, e não um
// campo implícito: no P-29 as ações deixam de ser só aviso ("disparar outros
// processos também"), e a chave de idempotência já a carrega. Acrescentar ação
// depois, com a chave gravada sem ela, exigiria migrar dado vivo.
type Action string

const (
	ActionEmail Action = "email"
)

// Trigger é DE ONDE a regra é acionada, e a distinção é da ADR-0025:
// transacional e aviso de atenção são coisas diferentes.
type Trigger string

const (
	// TriggerEvent — o gatilho é um evento da espinha (ADR-0019). Convite,
	// verificação de conta: dispara na hora, sempre, um por evento.
	TriggerEvent Trigger = "event"
	// TriggerAttentionBox — o gatilho é a CAIXA de atenção, não o evento cru.
	// Dois mapas divergem no primeiro ajuste: a caixa já decide o que exige
	// decisão humana, e um segundo mapa de "o que merece e-mail" começaria
	// igual e terminaria diferente.
	TriggerAttentionBox Trigger = "attention_box"
)

// DefaultDigestDelay é o atraso do aviso de atenção.
//
// O item abre, ESPERA, e só vira e-mail se ainda estiver aberto — quem estava
// no cockpit já resolveu. Quinze minutos é curto o bastante para o urgente não
// esperar e longo o bastante para o trivial se resolver sozinho.
//
// É PALPITE INFORMADO, como a tabela do roteador de modelo, e vira número
// calibrado quando houver telemetria. Por isso é configurável: o valor certo
// para uma equipe de plantão não é o de uma equipe que olha a caixa de manhã.
const DefaultDigestDelay = 15 * time.Minute

// DefaultMaxAttempts limita o reenvio do que FALHOU.
//
// Existe porque o oposto — tentar para sempre — transforma uma conta com
// e-mail inválido num gerador de tráfego contra o fornecedor, que é como se
// perde reputação de remetente e, com ela, a entrega de todo mundo.
const DefaultMaxAttempts = 5

// Recipient é para quem o aviso vai. Nome é opcional; endereço não.
type Recipient struct {
	Email string
	Name  string
}

// Event é o mínimo que a regra precisa saber do evento. Existe — como em
// attention.Event, e pelo mesmo motivo — para que este pacote não importe nem o
// adaptador nem o envelope do barramento.
type Event struct {
	ID          string
	AccountID   string
	Aggregate   string
	AggregateID string
	Type        string
	OccurredAt  time.Time
	Payload     map[string]any
}

// Command é o que o DECISOR devolve e o que o EXECUTOR consome: (tipo,
// destinatário, dados). É a fronteira entre os dois, e ela é deliberadamente
// burra — não há aqui nada que um carregador de regras vindas do banco não
// consiga preencher.
type Command struct {
	AccountID string
	// EventID, Rule e Action são a CHAVE DE IDEMPOTÊNCIA, composta desde o
	// primeiro dia (ADR-0025). Ver Key.
	EventID string
	Rule    string
	Action  Action

	Kind       Kind
	Recipients []Recipient
	Data       map[string]any
}

// Key é a chave de idempotência, composta.
//
// Não é `EventID` sozinho: com reação declarativa (P-29) um evento poderá
// disparar N ações, e chave só pelo evento descartaria a segunda como
// duplicata. Descarte por idempotência é silencioso por desenho — o segundo
// aviso simplesmente não aconteceria, sem erro e sem log.
func (c Command) Key() (eventID, rule string, action Action) {
	return c.EventID, c.Rule, c.Action
}

// Valid diz se o comando tem o mínimo para ser executado.
//
// Comando sem destinatário NÃO é erro: é o caso normal de uma conta cujos
// membros ainda não têm e-mail conhecido. Erro seria parar o consumidor por
// causa disso — e consumidor parado atrasa TODAS as notificações da fila.
func (c Command) Valid() bool {
	return c.AccountID != "" && c.EventID != "" && c.Rule != "" &&
		c.Action != "" && c.Kind != "" && len(c.Recipients) > 0
}

// State é o estado do REGISTRO de envio, no formato que o projeto irmão provou
// útil em operação — "o convite não chegou" precisa ser pergunta respondível.
//
// Os três últimos são os de lá; `pending` é a reserva, e existe porque a
// reserva acontece ANTES do envio.
//
// Não há struct de leitura (`Delivery`) neste pacote, e a ausência é
// deliberada: o registro não tem superfície de consulta hoje (ver o relatório —
// nenhum RPC foi inventado para ele), e um tipo de leitura sem leitor é um tipo
// que envelhece divergindo da tabela sem ninguém notar.
type State string

const (
	// StatePending: reservado, ainda sem desfecho. Linha parada aqui é anomalia
	// visível — o processo morreu entre o envio e o registro — e ela NÃO é
	// reenviada de propósito: e-mail duplicado é visível para o usuário e não
	// tem desfazer, enquanto um aviso perdido fica registrado.
	StatePending State = "pending"
	// StateSent: o fornecedor aceitou.
	StateSent State = "sent"
	// StateSentLocal: ENSAIO. Não havia credencial, imprimiu-se em vez de
	// enviar, e ninguém recebeu nada.
	StateSentLocal State = "sent_local"
	// StateError: falhou. É o ÚNICO estado reenviável.
	StateError State = "error"
)

// AttentionNotice é um item da caixa MADURO — aberto há mais que o atraso e
// ainda sem aviso.
//
// O pacote não importa `attention`: precisaria só de três strings, e importar
// um domínio inteiro por três strings acopla os dois no dia em que a caixa
// mudar de forma. A divergência possível é cosmética (título e tipo), não
// estrutural, e o mapeamento fica no adaptador que lê a tabela.
type AttentionNotice struct {
	AccountID string
	// EventID é o evento que ABRIU o item — é ele que entra na chave de
	// idempotência, e é por isso que o mesmo item nunca vira dois avisos.
	EventID  string
	ItemID   string
	Kind     string
	Title    string
	Summary  string
	DemandID string
	OpenedAt time.Time
}

// Kinds devolve os tipos que o domínio sabe emitir, DERIVADOS da tabela de
// regras — nunca uma segunda lista escrita à mão.
//
// Esta função é o contrato inteiro da garantia 1 da porta `Mailer`: a suíte
// exige de todo adaptador que resolva tudo o que ela devolve. Se fosse lista
// própria, alguém acrescentaria uma regra sem acrescentar o tipo aqui, e a
// suíte continuaria verde enquanto o e-mail novo não chegaria a ninguém — o
// exato silêncio que a ADR-0025 manda testar.
func Kinds() []Kind {
	vistos := map[Kind]bool{}
	out := make([]Kind, 0, len(Rules()))
	for _, r := range Rules() {
		if r.Kind == "" || vistos[r.Kind] {
			continue
		}
		vistos[r.Kind] = true
		out = append(out, r.Kind)
	}
	// Ordem estável: a suíte de contrato itera sobre isto, e teste cuja ordem
	// muda entre execuções é teste que ninguém consegue depurar.
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// KindNames é Kinds em string, para quem fala com a porta (que não conhece o
// tipo nomeado deste pacote — ver ports.Mail.Kind).
func KindNames() []string {
	ks := Kinds()
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return out
}

// enderecoValido é a checagem mais barata que separa "endereço" de "texto".
//
// Deliberadamente frouxa: validar e-mail por regex é um clássico de rejeitar
// endereço legítimo, e quem sabe de verdade se o endereço existe é o servidor
// do outro lado. O que se quer aqui é só não gastar uma ida ao fornecedor com
// string que obviamente não é endereço.
func enderecoValido(s string) bool {
	s = strings.TrimSpace(s)
	i := strings.LastIndex(s, "@")
	return i > 0 && i < len(s)-1 && !strings.ContainsAny(s, " \t\r\n")
}
