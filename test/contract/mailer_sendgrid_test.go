package contract_test

// A suíte de contrato do Mailer contra o adaptador REAL do SendGrid, com um
// httptest.Server do outro lado do fio.
//
//	go test ./test/contract/ -run Mailer -v
//
// Roda SEMPRE — sem tag, sem infra, sem chave. É condição para a suíte existir
// de verdade: a lição desta casa é que suíte que não roda não verifica nada (o
// adaptador k8s de SecretStore passou meses sem nunca ter sido exercitado).
//
// A API do SendGrid nunca é chamada de verdade. Ver o cabeçalho de
// mailer_fakes.go para o que o duplo prova e o que ele não prova.

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/mailer"
	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// chaveSG é o segredo que os duplos ecoam de volta. Valor com cara de chave
// real de propósito: um segredo "abc" casaria por acidente com qualquer texto e
// a garantia 4 passaria a acusar falso positivo.
const chaveSG = "SG.4nZk-teste-NAO-USAR.9xQv7hJp2LmR0tWc"

// idsDeTeste é tipo → `template_id`, montado a partir de `notification.Kinds()`.
//
// Derivado, e não literal: um literal aqui precisaria ser atualizado a cada tipo
// novo, e quem esquecesse veria a suíte falhar por CONFIGURAÇÃO faltando, não
// por TEMPLATE faltando — que é a falha que importa. Com o mapa derivado, a
// única forma de o subteste 1 reprovar é o índice do adaptador estar incompleto.
func idsDeTeste() map[string]string {
	out := map[string]string{}
	for _, k := range notification.KindNames() {
		out[k] = "d-" + k
	}
	return out
}

func TestMailerContractSendGrid(t *testing.T) {
	ids := idsDeTeste()
	novo := func(t *testing.T, f contract.Falha, comChave bool) (ports.Mailer, *contract.Caixa) {
		url, caixa := contract.NovoDuploSendGrid(t, ids, f, chaveSG)
		chave := chaveSG
		if !comChave {
			chave = "" // ensaio local
		}
		return mailer.NewSendGrid(mailer.SendGridConfig{
			APIKey:    chave,
			BaseURL:   url,
			From:      "avisos@dop.test",
			FromName:  "DOP",
			Templates: ids,
		}), caixa
	}

	contract.MailerSuite(t, "sendgrid", contract.MailerHarness{
		Segredo: chaveSG,
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

// O template configurado mas AUSENTE no fornecedor é a outra metade do silêncio
// da ADR-0025: o tipo está no índice, o id está na configuração, e o `d-…`
// aponta para nada. O SendGrid responde 400; o adaptador precisa dizer que foi
// recusa de conteúdo, e não indisponibilidade — senão o worker fica reenviando
// para sempre um template que não existe.
func TestMailerSendGridTemplateInexistenteNaoViraRetryEterno(t *testing.T) {
	ids := idsDeTeste()
	url, _ := contract.NovoDuploSendGrid(t, ids, contract.FalhaConteudo, chaveSG)
	m := mailer.NewSendGrid(mailer.SendGridConfig{
		APIKey: chaveSG, BaseURL: url, Templates: ids,
	})
	_, err := m.Send(t.Context(), ports.Mail{
		AccountID: "conta-1", Kind: notification.KindNames()[0], To: "alguem@exemplo.test",
	})
	if err == nil {
		t.Fatal("esperava recusa")
	}
	if got := errsKindOf(err); got != "invalid_argument" {
		t.Fatalf("esperava invalid_argument (permanente), veio %v: %v", got, err)
	}
}

// SEM id configurado, e COM chave, o adaptador precisa recusar por PRECONDIÇÃO
// — erro de montagem, não de política. A distinção existe para que quem lê o
// erro saiba se falta uma linha no código ou uma variável no deploy.
func TestMailerSendGridSemIDConfiguradoEhErroDeMontagem(t *testing.T) {
	url, caixa := contract.NovoDuploSendGrid(t, idsDeTeste(), "", chaveSG)
	m := mailer.NewSendGrid(mailer.SendGridConfig{
		APIKey: chaveSG, BaseURL: url, // Templates vazio
	})
	kind := notification.KindNames()[0]
	if got := errsKindOf(m.Resolve(t.Context(), kind)); got != "failed_precondition" {
		t.Fatalf("esperava failed_precondition, veio %v", got)
	}
	if _, err := m.Send(t.Context(), ports.Mail{
		AccountID: "c", Kind: kind, To: "a@b.test",
	}); errsKindOf(err) != "failed_precondition" {
		t.Fatalf("Send sem id: esperava failed_precondition, veio %v", err)
	}
	if caixa.Chamadas() != 0 {
		t.Fatalf("erro de montagem custou %d ida(s) ao fornecedor", caixa.Chamadas())
	}
}
