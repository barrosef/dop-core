package notification

import (
	"context"
	"time"
)

// DeliveryKey é a chave de idempotência, composta (ADR-0025). Existe como tipo
// para que nenhum chamador consiga montar meia chave.
type DeliveryKey struct {
	EventID string
	Rule    string
	Action  Action
}

// Claim é a RESERVA de uma execução de ação.
//
// A reserva acontece ANTES do envio, e essa ordem é o desenho inteiro da
// idempotência: reservar depois de enviar deixa uma janela em que a reentrega
// do JetStream manda o segundo e-mail. Reservar antes fecha a janela e, no
// pior caso, perde um aviso que fica REGISTRADO — assimetria correta, porque
// e-mail duplicado é visível para o usuário e não tem desfazer.
type Claim struct {
	DeliveryKey
	AccountID  string
	Kind       Kind
	Channel    string
	Recipients []string
}

// Outcome é o desfecho de UMA mensagem, cobrindo as chaves que ela atendeu.
//
// São chaves no plural porque o resumo agrupa: N itens da caixa viram um
// e-mail. A idempotência continua por (evento, regra, ação) — uma linha por
// item —, e o agrupamento aparece no `batch_id` que o repositório carimba.
type Outcome struct {
	AccountID  string
	Keys       []DeliveryKey
	Kind       Kind
	Recipients []string
	State      State
	Provider   string
	Reference  string
	// Error é a mensagem de falha JÁ REDIGIDA pelo adaptador (garantia 4 da
	// porta). O domínio não redige nada: ele não sabe qual é o segredo.
	Error string
}

// Repository é a PORTA de persistência do gatilho.
//
// Não há `Create` nem `Update` soltos: as duas únicas escritas são RESERVAR e
// LIQUIDAR, porque são as duas únicas coisas que acontecem. Uma porta com
// escrita genérica seria um convite a gravar envio à mão, e o registro
// deixaria de refletir o que saiu.
type Repository interface {
	// Claim reserva a chave. Devolve `true` quando ESTA chamada ficou com ela.
	//
	// Devolve `false` — sem erro — quando a chave já foi atendida: é o caminho
	// normal da reentrega, e transformá-lo em erro faria toda mensagem
	// reentregue parecer defeito. Uma reserva em StateError é RETOMADA (e
	// `attempts` sobe), porque falha de envio é a única situação em que
	// reenviar é certo.
	Claim(ctx context.Context, c Claim, maxAttempts int) (bool, error)

	// Settle grava o desfecho das chaves e emite o evento na MESMA transação.
	// Devolve o `batch_id` carimbado, que é o que amarra as linhas à mensagem.
	Settle(ctx context.Context, o Outcome) (string, error)

	// Recipients devolve quem recebe aviso da conta: membros com e-mail
	// conhecido. Lista vazia é resposta legítima — conta cujos membros
	// entraram por telefone ou SSO sem escopo de perfil não tem endereço.
	Recipients(ctx context.Context, accountID string) ([]Recipient, error)

	// AccountsWithRipeAttention diz QUAIS contas têm item maduro esperando
	// aviso. Existe separado de RipeAttention pelo mesmo motivo do varredor de
	// sandboxes: o scheduler não tem conta ativa, e a saída é visitar conta por
	// conta, cada visita com aquela conta no contexto — o isolamento não é
	// afrouxado, só muda quem decide a ordem de visita.
	AccountsWithRipeAttention(ctx context.Context, rule string, action Action, olderThan time.Time, maxAttempts int) ([]string, error)

	// RipeAttention devolve os itens ABERTOS há mais que o atraso e que ainda
	// não viraram aviso, da conta ativa.
	//
	// "Ainda abertos" é a regra inteira do atraso: item resolvido antes do
	// corte simplesmente não aparece aqui, e por isso não vira e-mail. Não há
	// agendador, não há cancelamento — há uma consulta que só enxerga o que
	// sobreviveu à espera.
	RipeAttention(ctx context.Context, accountID, rule string, action Action, olderThan time.Time, maxAttempts, limit int) ([]AttentionNotice, error)
}
