package contract_test

// Os testes que atravessam OS DOIS adaptadores.
//
// A suíte (mailer.go) roda uma vez por adaptador e prova que cada um cumpre a
// porta. O que ela não consegue dizer sozinha é o que só aparece comparando os
// dois — e é aí que mora o defeito da ADR-0025: um fornecedor fica para trás e
// nada fica vermelho.

import (
	"strings"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/mailer"
	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

func errsKindOf(err error) errs.Kind { return errs.KindOf(err) }
func contains(s, sub string) bool    { return strings.Contains(s, sub) }

// TODO adaptador resolve TODO tipo — dito de uma vez, sem duplo e sem rede.
//
// É a mesma afirmação do subteste 1 da suíte, escrita aqui para que a mensagem
// de falha nomeie QUAL fornecedor ficou para trás. Com a suíte sozinha, quem
// acrescenta um tipo vê "sendgrid falhou" e "smtp falhou" em dois blocos
// distantes, e a conclusão certa — "o tipo novo não tem template em lugar
// nenhum" — depende de o leitor juntar as duas coisas.
func TestTodoAdaptadorResolveTodoTipoDaPolitica(t *testing.T) {
	tipos := notification.KindNames()
	if len(tipos) == 0 {
		t.Fatal("a política não declara tipo nenhum")
	}
	adaptadores := map[string]ports.Mailer{
		// Em ENSAIO: sem credencial e sem rede. A garantia 3 da porta exige que
		// a resolução aconteça mesmo assim, e é isso que permite este teste
		// existir sem duplo nenhum.
		"sendgrid": mailer.NewSendGrid(mailer.SendGridConfig{}),
		"smtp":     mailer.NewSMTP(mailer.SMTPConfig{}),
	}
	for nome, m := range adaptadores {
		for _, kind := range tipos {
			if err := m.Resolve(t.Context(), kind); err != nil {
				t.Errorf("o aviso %q não tem template no adaptador %q: %v\n"+
					"Enquanto isso durar, quem estiver com esse fornecedor ligado "+
					"simplesmente NÃO RECEBE — sem erro, sem log, sem métrica.",
					kind, nome, err)
			}
		}
	}
}

// O script de publicação precisa cobrir tudo o que o adaptador do SendGrid
// promete resolver.
//
// Sem este teste, o buraco é o seguinte: o índice tem o tipo, a suíte fica
// verde, o id vai para a configuração — e o template nunca foi publicado,
// porque o catálogo do script não o incluía. O envio falha em produção com
// "template not found", meses depois de a suíte ter aprovado.
func TestCatalogoDePublicacaoCobreOIndiceDoSendGrid(t *testing.T) {
	catalogo, err := mailer.SendGridCatalog()
	if err != nil {
		t.Fatalf("catálogo de publicação inválido: %v", err)
	}
	vistos := map[string]bool{}
	for _, spec := range catalogo {
		if len(spec.HTML) == 0 {
			t.Errorf("o template %q (%s) está vazio", spec.Kind, spec.File)
		}
		if strings.TrimSpace(spec.Subject) == "" {
			t.Errorf("o template %q não tem assunto para publicar", spec.Kind)
		}
		if spec.Name == "" {
			t.Errorf("o template %q não tem nome — é o nome que torna a publicação "+
				"idempotente; sem ele, cada execução cria um template novo", spec.Kind)
		}
		vistos[spec.Kind] = true
	}
	for _, kind := range notification.KindNames() {
		if !vistos[kind] {
			t.Errorf("o aviso %q não está no catálogo de publicação: o template nunca "+
				"chega ao SendGrid, e o envio falha com 'template not found'", kind)
		}
	}
}

// Os dois templates do MESMO aviso precisam falar dos mesmos dados.
//
// É o "dois lugares para o template do mesmo aviso" que a ADR-0025 assume como
// custo. O que ela pede em troca é que a suíte impeça um de ficar para trás — e
// "ficar para trás" não é só ausência: é também o SMTP passar a mostrar um
// campo que o SendGrid ignora. Este teste não compara HTML (seriam dois motores
// diferentes); compara quais VARIÁVEIS cada lado consome.
func TestOsDoisTemplatesConsomemOsMesmosCampos(t *testing.T) {
	catalogo, err := mailer.SendGridCatalog()
	if err != nil {
		t.Fatalf("catálogo: %v", err)
	}
	for _, spec := range catalogo {
		hbs := camposHandlebars(string(spec.HTML))
		gos := camposGo(t, spec.File)
		for c := range gos {
			if !hbs[c] {
				t.Errorf("%s: o template SMTP usa %q e o do SendGrid não — quem estiver "+
					"no SendGrid recebe o aviso sem essa informação", spec.Kind, c)
			}
		}
		for c := range hbs {
			if !gos[c] {
				t.Errorf("%s: o template SendGrid usa %q e o do SMTP não — quem estiver "+
					"em self-hosted recebe o aviso sem essa informação", spec.Kind, c)
			}
		}
	}
}

// camposHandlebars extrai os nomes usados num template do SendGrid.
// Reconhece `{{campo}}`, `{{#if campo}}`, `{{#each campo}}` e `{{this.campo}}`.
func camposHandlebars(s string) map[string]bool {
	out := map[string]bool{}
	for _, bruto := range entreChaves(s) {
		bruto = strings.TrimSpace(bruto)
		bruto = strings.TrimPrefix(bruto, "#if ")
		bruto = strings.TrimPrefix(bruto, "#each ")
		if strings.HasPrefix(bruto, "/") || strings.HasPrefix(bruto, "#") {
			continue
		}
		bruto = strings.TrimPrefix(bruto, "this.")
		if bruto != "" && bruto != "subject" {
			out[bruto] = true
		}
	}
	return out
}

// camposGo extrai os nomes usados num template do SMTP. Reconhece `{{.campo}}`,
// `{{if .campo}}` e `{{range .campo}}`; dentro de um `range`, `{{.campo}}` é
// campo do item — e é por isso que o nome, não o caminho, é o que se compara.
func camposGo(t *testing.T, arquivo string) map[string]bool {
	t.Helper()
	bruto, err := mailer.SMTPTemplateSource(arquivo)
	if err != nil {
		t.Fatalf("fonte do template SMTP %q: %v", arquivo, err)
	}
	out := map[string]bool{}
	for _, exp := range entreChaves(bruto) {
		exp = strings.TrimSpace(exp)
		for _, prefixo := range []string{"if ", "range ", "with ", "else if "} {
			exp = strings.TrimPrefix(exp, prefixo)
		}
		if exp == "end" || exp == "else" || !strings.HasPrefix(exp, ".") {
			continue
		}
		nome := strings.TrimPrefix(exp, ".")
		if nome != "" {
			out[nome] = true
		}
	}
	return out
}

func entreChaves(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			return out
		}
		s = s[i+2:]
		j := strings.Index(s, "}}")
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+2:]
	}
}

// Cada template DECLARA o tipo que ele serve, e o índice precisa concordar.
//
// ── O buraco que este teste fecha, e como ele foi encontrado ────────────────
//
// Depois de todas as quebras esperadas reprovarem, sobrou a pergunta do que a
// suíte NÃO pegava. Duas sabotagens passaram verdes:
//
//  1. trocar os ARQUIVOS de `invite` e `attention_digest` no índice do SMTP —
//     só um teste escrito à mão para o resumo reclamou, e um TIPO NOVO não
//     teria esse teste;
//  2. trocar os ASSUNTOS dos dois tipos no índice do SendGrid — nada reclamou.
//
// São a mesma família e é a pior delas: o adaptador resolve o tipo CERTO para o
// artefato ERRADO. A garantia 1 continua cumprida — ninguém fica sem receber —,
// e a pessoa convidada recebe um resumo de pendências de uma conta da qual ela
// ainda não faz parte.
//
// A metade estrutural (1) fecha aqui, e fecha de forma GENÉRICA — vale para
// todo tipo que existir depois, sem ninguém precisar lembrar: o arquivo declara
// o tipo no cabeçalho, e o índice tem de apontar para o arquivo que declara
// aquele tipo. A metade do ASSUNTO (2) continua aberta e está no relatório: dois
// textos escritos por gente, trocados entre duas linhas de uma tabela, não são
// distinguíveis por máquina sem uma declaração redundante — e a redundância
// certa é o assunto morar DENTRO do template, o que o SendGrid não permite
// enquanto ele guardar assunto separado do corpo.
func TestCadaTemplateDeclaraOTipoQueEleServe(t *testing.T) {
	catalogo, err := mailer.SendGridCatalog()
	if err != nil {
		t.Fatalf("catálogo: %v", err)
	}
	for _, spec := range catalogo {
		marca := "dop-template: " + spec.Kind

		// SendGrid: o HTML que o script vai publicar.
		if !contains(string(spec.HTML), marca) {
			t.Errorf("o template SendGrid do aviso %q (%s) não declara %q: o índice pode "+
				"estar apontando para o arquivo de outro tipo, e quem receber vai ler a "+
				"mensagem errada", spec.Kind, spec.File, marca)
		}

		// SMTP: o arquivo que o índice associa ao MESMO tipo.
		arquivo, err := mailer.SMTPTemplateFile(spec.Kind)
		if err != nil {
			t.Errorf("o aviso %q não tem arquivo no índice do SMTP: %v", spec.Kind, err)
			continue
		}
		fonte, err := mailer.SMTPTemplateSource(arquivo)
		if err != nil {
			t.Errorf("fonte de %q: %v", arquivo, err)
			continue
		}
		if !contains(fonte, marca) {
			t.Errorf("o índice do SMTP associa o aviso %q ao arquivo %q, e esse arquivo "+
				"declara servir outro tipo — a mensagem sai com o rótulo certo e o "+
				"conteúdo errado, que é pior que não sair", spec.Kind, arquivo)
		}
	}
}
