package contract_test

// A MESMA suíte de contrato, contra o adaptador REAL de SMTP e um servidor
// local de teste.
//
//	go test ./test/contract/ -run Mailer -v
//
// O SMTP é o adaptador que FORÇA a resolução local de template: ele não tem
// template de fornecedor nenhum. Se a suíte só rodasse contra o SendGrid, nada
// impediria a porta de vazar `template_id` — e a "porta que fala intenção"
// viraria uma porta que fala SendGrid sem que nenhum teste percebesse.

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/mailer"
	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// senhaSMTP é o segredo que o duplo ecoa na recusa de AUTH.
const senhaSMTP = "s3nh4-de-relay-NAO-USAR-9xQv7hJp"

func TestMailerContractSMTP(t *testing.T) {
	novo := func(t *testing.T, f contract.Falha, comServidor bool) (ports.Mailer, *contract.Caixa) {
		addr, caixa := contract.NovoDuploSMTP(t, f, senhaSMTP)
		if !comServidor {
			addr = "" // ensaio local: sem endereço, imprime em vez de enviar
		}
		return mailer.NewSMTP(mailer.SMTPConfig{
			Addr:     addr,
			Username: "dop",
			Password: senhaSMTP,
			From:     "avisos@dop.test",
			FromName: "DOP",
		}), caixa
	}

	contract.MailerSuite(t, "smtp", contract.MailerHarness{
		Segredo: senhaSMTP,
		Novo: func(t *testing.T) (ports.Mailer, *contract.Caixa) {
			return novo(t, "", true)
		},
		NovoFalho: func(t *testing.T, f contract.Falha) (ports.Mailer, *contract.Caixa) {
			return novo(t, f, true)
		},
		NovoEnsaio: func(t *testing.T) (ports.Mailer, *contract.Caixa) {
			return novo(t, "", false)
		},
	})
}

// O SMTP renderiza de verdade, então dá para exigir dele o que o SendGrid não
// permite verificar daqui: que os DADOS chegaram ao corpo.
//
// Sem isto, um template que ignorasse `items` passaria em toda a suíte — o
// e-mail sairia, com assunto certo, dizendo "2 pendências" e listando nenhuma.
func TestMailerSMTPRenderizaOsDadosNoCorpo(t *testing.T) {
	addr, caixa := contract.NovoDuploSMTP(t, "", senhaSMTP)
	m := mailer.NewSMTP(mailer.SMTPConfig{Addr: addr, From: "avisos@dop.test"})

	_, err := m.Send(t.Context(), ports.Mail{
		AccountID: "conta-1",
		Kind:      string(notification.KindAttentionDigest),
		To:        "dev@exemplo.test",
		Data: map[string]any{
			"total": 2,
			"link":  "https://cockpit.exemplo.test/atencao",
			"items": []map[string]any{
				{"kind": "thread_blocked", "title": "Um agente precisa de resposta", "summary": "thread 7"},
				{"kind": "pr_review", "title": "PR aguardando revisão"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := caixa.Mensagens()
	if len(msgs) != 1 {
		t.Fatalf("esperava 1 mensagem, veio %d", len(msgs))
	}
	corpo := msgs[0].Body
	for _, esperado := range []string{
		"Um agente precisa de resposta",
		"PR aguardando revisão",
		"thread 7",
		"https://cockpit.exemplo.test/atencao",
	} {
		if !contains(corpo, esperado) {
			t.Errorf("o corpo renderizado NÃO contém %q — o e-mail sairia dizendo "+
				"que há pendências e listando nenhuma", esperado)
		}
	}
	// Chave AUSENTE não pode virar "<no value>" na cara do usuário: o segundo
	// item não tem `summary`, e o template precisa lidar com isso em silêncio.
	if contains(corpo, "<no value>") {
		t.Errorf("o corpo saiu com \"<no value>\": falta Option(missingkey=zero)")
	}
}

// O assunto do resumo carrega o NÚMERO, e isso é decisão de produto, não
// cosmética: "3 pendências esperando você" é lido; "Você tem pendências" é
// arquivado. Um assunto que perdesse o número passaria em toda a suíte.
func TestMailerSMTPAssuntoDoResumoCarregaONumero(t *testing.T) {
	addr, caixa := contract.NovoDuploSMTP(t, "", senhaSMTP)
	m := mailer.NewSMTP(mailer.SMTPConfig{Addr: addr})
	if _, err := m.Send(t.Context(), ports.Mail{
		AccountID: "conta-1", Kind: string(notification.KindAttentionDigest),
		To: "dev@exemplo.test", Data: map[string]any{"total": 7},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := caixa.Mensagens()
	if len(msgs) != 1 {
		t.Fatalf("esperava 1 mensagem, veio %d", len(msgs))
	}
	if !contains(msgs[0].Subject, "7") {
		t.Fatalf("o assunto do resumo perdeu o número: %q", msgs[0].Subject)
	}
}
