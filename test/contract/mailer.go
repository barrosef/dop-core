package contract

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/notification"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// ════════════════════════════════════════════════════════════════════════════
// Suíte de contrato da porta ports.Mailer.
//
// Disciplina da ADR-0001: uma porta com um adaptador só é palpite. O mesmo
// conjunto roda contra o SendGrid e contra o SMTP.
//
// ── O subteste que justifica a suíte inteira ────────────────────────────────
//
// `1_resolve_todos_os_tipos_do_dominio`. A ADR-0025 escreveu a consequência que
// exige teste: um tipo de notificação pode existir na política e não ter
// template no fornecedor, e isso falharia em SILÊNCIO — o evento acontece, o
// consumidor roda, ninguém recebe. Nada mais no sistema fica vermelho por causa
// disso: não há erro, não há log, não há métrica. Só a caixa de entrada de
// alguém que fica vazia.
//
// A lista de tipos vem de `notification.Kinds()`, que por sua vez é DERIVADA da
// tabela de regras. É a corrente inteira: acrescentar uma linha na política
// acrescenta um tipo, que acrescenta uma exigência a TODO adaptador, que
// reprova aqui até alguém dar template a ele nos dois fornecedores.
//
// ── A API do fornecedor nunca é chamada de verdade ──────────────────────────
//
// Do outro lado do fio há um DUPLO (httptest.Server para o SendGrid, servidor
// SMTP local para o outro); sob teste está o adaptador REAL. É a única
// combinação que prova alguma coisa: duplo dos dois lados prova que o duplo é
// consistente consigo mesmo, e adaptador contra fornecedor real transforma a
// suíte em algo que ninguém roda.
// ════════════════════════════════════════════════════════════════════════════

// Caixa é o que o duplo do outro lado do fio recolheu. O tipo é da SUÍTE, e não
// de cada runner, para que as asserções sejam as mesmas nos dois adaptadores.
type Caixa struct {
	mu       sync.Mutex
	msgs     []SentMail
	chamadas int
}

// SentMail é uma mensagem que chegou ao duplo.
type SentMail struct {
	To      string
	Kind    string
	Subject string
	Body    string
}

// Recebeu registra uma mensagem. Chamado pelos duplos dos runners.
func (c *Caixa) Recebeu(m SentMail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	c.chamadas++
}

// Bateu registra que houve CONTATO, ainda que sem mensagem válida. É o que
// permite provar a garantia 5: recusa sem I/O.
func (c *Caixa) Bateu() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chamadas++
}

func (c *Caixa) Mensagens() []SentMail {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SentMail, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func (c *Caixa) Chamadas() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chamadas
}

// Falha é o modo de recusa que o duplo simula. São três porque a garantia 7 da
// porta faz três distinções, e confundi-las manda a equipe caçar defeito no
// lugar errado.
type Falha string

const (
	// FalhaCredencial: o fornecedor recusa a credencial (401 / 535).
	FalhaCredencial Falha = "credencial"
	// FalhaIndisponivel: o fornecedor não atende (503 / conexão recusada).
	FalhaIndisponivel Falha = "indisponivel"
	// FalhaConteudo: o fornecedor recusa a mensagem (400 / 550).
	FalhaConteudo Falha = "conteudo"
)

// MailerHarness é o que cada runner fornece: três montagens do MESMO adaptador
// real, contra duplos diferentes.
type MailerHarness struct {
	// Novo monta o adaptador contra um duplo saudável.
	Novo func(t *testing.T) (ports.Mailer, *Caixa)
	// NovoFalho monta contra um duplo que recusa do jeito pedido.
	NovoFalho func(t *testing.T, f Falha) (ports.Mailer, *Caixa)
	// NovoEnsaio monta SEM credencial — o modo em que o adaptador imprime em
	// vez de enviar.
	NovoEnsaio func(t *testing.T) (ports.Mailer, *Caixa)
	// Segredo é a credencial que os duplos ECOAM de volta na mensagem de erro.
	// É assim que a garantia 4 vira teste em vez de promessa: o fornecedor
	// devolve o segredo, e a suíte exige que ele não chegue ao erro.
	Segredo string
}

// MailerSuite verifica as dez garantias documentadas na porta.
func MailerSuite(t *testing.T, name string, h MailerHarness) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()
		tipos := notification.KindNames()
		if len(tipos) == 0 {
			t.Fatal("notification.Kinds() está vazio: sem tipos, esta suíte não prova nada")
		}

		// ── 1. A GARANTIA QUE JUSTIFICA A SUÍTE ─────────────────────────────
		t.Run("1_resolve_todos_os_tipos_do_dominio", func(t *testing.T) {
			m, _ := h.Novo(t)
			for _, kind := range tipos {
				if err := m.Resolve(ctx, kind); err != nil {
					t.Errorf("o adaptador NÃO resolve o aviso %q: %v\n\n"+
						"Isto falharia em SILÊNCIO em produção: o evento acontece, o "+
						"consumidor roda e ninguém recebe. Dê template a este tipo neste "+
						"fornecedor (ADR-0025).", kind, err)
				}
			}
		})

		t.Run("1b_envia_todos_os_tipos_do_dominio", func(t *testing.T) {
			// Resolve e Send precisam CONCORDAR (garantia 2). Um adaptador cujo
			// Resolve diz "sim" e cujo Send não encontra o template passaria no
			// subteste acima e continuaria falhando calado.
			for _, kind := range tipos {
				m, caixa := h.Novo(t)
				rec, err := m.Send(ctx, ports.Mail{
					AccountID: "conta-1", Kind: kind, To: "alguem@exemplo.test",
					ToName: "Alguém", Data: dadosDeExemplo(),
				})
				if err != nil {
					t.Errorf("Send do aviso %q falhou contra o duplo saudável: %v", kind, err)
					continue
				}
				if rec == nil || rec.State != ports.MailSent {
					t.Errorf("aviso %q: esperava State=%q, veio %+v", kind, ports.MailSent, rec)
					continue
				}
				if rec.Provider == "" {
					t.Errorf("aviso %q: Provider vazio — o registro não diria quem enviou", kind)
				}
				msgs := caixa.Mensagens()
				if len(msgs) != 1 {
					t.Errorf("aviso %q: o duplo recebeu %d mensagem(ns), esperava 1", kind, len(msgs))
					continue
				}
				if msgs[0].Kind != kind {
					t.Errorf("o aviso %q chegou ao fornecedor como %q — template trocado é "+
						"pior que template ausente: alguém recebe a mensagem errada",
						kind, msgs[0].Kind)
				}
				if strings.TrimSpace(msgs[0].Subject) == "" {
					t.Errorf("aviso %q chegou SEM assunto", kind)
				}
			}
		})

		// ── 2. tipo fora do índice é KindNotFound, e não sucesso silencioso ──
		t.Run("2_tipo_desconhecido_e_notfound", func(t *testing.T) {
			m, caixa := h.Novo(t)
			const inventado = "tipo-que-nunca-existiu"

			if err := m.Resolve(ctx, inventado); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("Resolve de tipo desconhecido: esperava KindNotFound, veio %v (%v)",
					errs.KindOf(err), err)
			}
			_, err := m.Send(ctx, ports.Mail{
				AccountID: "conta-1", Kind: inventado, To: "alguem@exemplo.test",
			})
			if errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("Send de tipo desconhecido: esperava KindNotFound, veio %v (%v)",
					errs.KindOf(err), err)
			}
			if caixa.Chamadas() != 0 {
				t.Fatalf("tipo desconhecido chegou a tocar o fornecedor (%d chamada(s)): "+
					"resolução acontece ANTES do I/O", caixa.Chamadas())
			}
		})

		// ── 3. ensaio local ─────────────────────────────────────────────────
		t.Run("3_ensaio_local_imprime_e_nao_envia", func(t *testing.T) {
			m, caixa := h.NovoEnsaio(t)
			rec, err := m.Send(ctx, ports.Mail{
				AccountID: "conta-1", Kind: tipos[0], To: "alguem@exemplo.test",
				Data: dadosDeExemplo(),
			})
			if err != nil {
				t.Fatalf("ensaio deveria funcionar sem credencial: %v", err)
			}
			if rec.State != ports.MailSentLocal {
				t.Fatalf("ensaio: esperava State=%q, veio %q — a diferença entre "+
					"'avisamos' e 'fingimos avisar' não pode depender de quem lê o log "+
					"lembrar em que ambiente aquilo rodou", ports.MailSentLocal, rec.State)
			}
			if caixa.Chamadas() != 0 {
				t.Fatalf("o ENSAIO falou com o fornecedor (%d chamada(s))", caixa.Chamadas())
			}
		})

		t.Run("3b_ensaio_ainda_resolve_o_template", func(t *testing.T) {
			// O subteste mais fácil de esquecer, e o que decide se a garantia 1
			// vale onde ela é exercitada. Toda máquina de dev e todo CI rodam sem
			// chave; se o ensaio pulasse a resolução, o tipo sem template
			// passaria em TODO lugar e só quebraria em produção.
			m, _ := h.NovoEnsaio(t)
			if err := m.Resolve(ctx, "tipo-que-nunca-existiu"); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("ensaio: Resolve de tipo desconhecido deveria ser KindNotFound, veio %v", err)
			}
			_, err := m.Send(ctx, ports.Mail{
				AccountID: "conta-1", Kind: "tipo-que-nunca-existiu", To: "alguem@exemplo.test",
			})
			if errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("ensaio: Send de tipo desconhecido deveria ser KindNotFound, veio %v", err)
			}
			for _, kind := range tipos {
				if err := m.Resolve(ctx, kind); err != nil {
					t.Errorf("ensaio: o aviso %q não resolve: %v", kind, err)
				}
			}
		})

		// ── 4. o segredo não sai ────────────────────────────────────────────
		t.Run("4_segredo_nao_vaza", func(t *testing.T) {
			if h.Segredo == "" {
				t.Fatal("o runner precisa informar o Segredo para esta garantia valer")
			}
			m, _ := h.Novo(t)

			// 4a. formatação do adaptador. `%#v` está na lista, e ele é o que
			// esta suíte NÃO checava até esta sessão: um String() com receptor
			// por valor engole `%v` e `%+v`, então guardar a chave num campo
			// passava despercebido — e `%#v` (que ignora String()) a imprimia
			// inteira. Verificado com um adaptador sabotado de propósito.
			for _, s := range []string{
				fmt.Sprintf("%v", m), fmt.Sprintf("%+v", m), fmt.Sprintf("%#v", m),
				fmt.Sprintf("%v", derefSeguro(m)), fmt.Sprintf("%+v", derefSeguro(m)),
				fmt.Sprintf("%#v", derefSeguro(m)),
			} {
				if strings.Contains(s, h.Segredo) {
					t.Fatalf("A CREDENCIAL VAZOU na formatação do adaptador: %s", s)
				}
			}

			// 4d. ESTRUTURAL, e é esta a garantia de verdade.
			//
			// Verificar formatação verifica um SINTOMA: enquanto existir um
			// String() por valor, o campo com a chave fica escondido de `%v` — e
			// o teste aprova um adaptador que guarda o segredo. No dia em que
			// alguém renomear o String(), acrescentar o adaptador dentro de
			// outro struct, ou um panic imprimir `%#v`, a chave sai.
			//
			// Aqui a pergunta é outra: o segredo ESTÁ em algum campo? A porta diz
			// que não deve estar — ele é capturado em closure, e closure a
			// reflexão não abre. "Não há chave para logar" é uma propriedade da
			// estrutura, e é assim que ela se verifica.
			if caminho := campoComSegredo(reflect.ValueOf(m), h.Segredo, "adaptador", 0); caminho != "" {
				t.Fatalf("A CREDENCIAL está guardada em %s.\n\n"+
					"Hoje ela não aparece em `%%v` só porque existe um String() por "+
					"valor — é proteção que depende de disciplina. Capture o segredo "+
					"num CLOSURE (como o `autorizar`/`autenticar` faz): closure imprime "+
					"como endereço, e não há o que vazar.", caminho)
			}

			// 4b. mensagem de erro, com o fornecedor ECOANDO o segredo de volta.
			falho, _ := h.NovoFalho(t, FalhaIndisponivel)
			_, err := falho.Send(ctx, ports.Mail{
				AccountID: "conta-1", Kind: tipos[0], To: "alguem@exemplo.test",
				Data: dadosDeExemplo(),
			})
			if err == nil {
				t.Fatal("o duplo indisponível deveria ter feito o envio falhar")
			}
			if strings.Contains(err.Error(), h.Segredo) {
				t.Fatalf("A CREDENCIAL VAZOU na mensagem de erro: %v", err)
			}

			// 4c. o mesmo pelo caminho da autenticação recusada, que é o outro
			// lugar onde o fornecedor tem o segredo em mãos.
			semCred, _ := h.NovoFalho(t, FalhaCredencial)
			_, err = semCred.Send(ctx, ports.Mail{
				AccountID: "conta-1", Kind: tipos[0], To: "alguem@exemplo.test",
				Data: dadosDeExemplo(),
			})
			if err == nil {
				t.Fatal("o duplo que recusa credencial deveria ter feito o envio falhar")
			}
			if strings.Contains(err.Error(), h.Segredo) {
				t.Fatalf("A CREDENCIAL VAZOU na recusa de autenticação: %v", err)
			}
		})

		// ── 5. recusa sem I/O ───────────────────────────────────────────────
		t.Run("5_pedido_invalido_sem_io", func(t *testing.T) {
			casos := []struct {
				nome string
				mail ports.Mail
			}{
				{"destinatário vazio", ports.Mail{Kind: tipos[0], To: ""}},
				{"destinatário sem arroba", ports.Mail{Kind: tipos[0], To: "fulano"}},
				{"destinatário com espaço", ports.Mail{Kind: tipos[0], To: "a b@c.test"}},
				{"arroba no fim", ports.Mail{Kind: tipos[0], To: "fulano@"}},
				{"tipo vazio", ports.Mail{Kind: "", To: "alguem@exemplo.test"}},
			}
			for _, c := range casos {
				m, caixa := h.Novo(t)
				_, err := m.Send(ctx, c.mail)
				if errs.KindOf(err) != errs.KindInvalid {
					t.Errorf("%s: esperava KindInvalid, veio %v (%v)", c.nome, errs.KindOf(err), err)
				}
				if caixa.Chamadas() != 0 {
					t.Errorf("%s: custou %d ida(s) ao fornecedor — quem chama sem "+
						"endereço não pode custar uma chamada", c.nome, caixa.Chamadas())
				}
			}
		})

		// ── 6/7. tradução de erro ───────────────────────────────────────────
		t.Run("7_erros_traduzidos_por_natureza", func(t *testing.T) {
			casos := []struct {
				falha    Falha
				esperado errs.Kind
				porque   string
			}{
				{FalhaCredencial, errs.KindUnauthorized,
					"credencial recusada não é indisponibilidade: manda a equipe caçar rede quando o problema é senha"},
				{FalhaIndisponivel, errs.KindUnavailable,
					"fornecedor fora do ar não é erro de quem chamou, e reenviar depois faz sentido"},
				{FalhaConteudo, errs.KindInvalid,
					"conteúdo ou endereço recusado é permanente: reenviar igual dá o mesmo resultado"},
			}
			for _, c := range casos {
				m, _ := h.NovoFalho(t, c.falha)
				_, err := m.Send(ctx, ports.Mail{
					AccountID: "conta-1", Kind: tipos[0], To: "alguem@exemplo.test",
					Data: dadosDeExemplo(),
				})
				if got := errs.KindOf(err); got != c.esperado {
					t.Errorf("falha %q: esperava %v, veio %v (%v) — %s",
						c.falha, c.esperado, got, err, c.porque)
				}
			}
		})

		// ── 10. concorrência ────────────────────────────────────────────────
		t.Run("10_seguro_para_uso_concorrente", func(t *testing.T) {
			m, caixa := h.Novo(t)
			const n = 8
			var wg sync.WaitGroup
			errCh := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, err := m.Send(ctx, ports.Mail{
						AccountID: "conta-1", Kind: tipos[i%len(tipos)],
						To:   fmt.Sprintf("dest-%d@exemplo.test", i),
						Data: dadosDeExemplo(),
					})
					errCh <- err
				}(i)
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					t.Fatalf("envio concorrente falhou: %v", err)
				}
			}
			if got := len(caixa.Mensagens()); got != n {
				t.Fatalf("o duplo recebeu %d mensagens, esperava %d", got, n)
			}
		})

		// ── 9. Send não muta o Data do chamador ─────────────────────────────
		t.Run("9_send_nao_muta_os_dados_do_chamador", func(t *testing.T) {
			// O resumo reusa o MESMO mapa para todos os destinatários. Um
			// adaptador que escrevesse nele (o SendGrid injeta o assunto)
			// contaminaria o segundo envio — e o defeito só apareceria em conta
			// com mais de um membro.
			m, _ := h.Novo(t)
			dados := dadosDeExemplo()
			antes := len(dados)
			for i := 0; i < 2; i++ {
				if _, err := m.Send(ctx, ports.Mail{
					AccountID: "conta-1", Kind: tipos[0],
					To: fmt.Sprintf("dest-%d@exemplo.test", i), Data: dados,
				}); err != nil {
					t.Fatalf("envio %d: %v", i, err)
				}
			}
			if len(dados) != antes {
				t.Fatalf("o adaptador MUTOU o Data do chamador: %d chaves viraram %d",
					antes, len(dados))
			}
		})
	})
}

// dadosDeExemplo é um payload que serve a TODOS os tipos: o resumo precisa de
// `items` e `total`, o convite de `email` e `role`. Chave sobrando não é erro
// (garantia 8), e é isso que permite um payload só.
func dadosDeExemplo() map[string]any {
	return map[string]any{
		"account_id": "conta-1",
		"email":      "convidado@exemplo.test",
		"role":       "member",
		"link":       "https://cockpit.exemplo.test/atencao",
		"total":      2,
		"items": []map[string]any{
			{"kind": "thread_blocked", "title": "Um agente precisa de resposta", "summary": "thread 7"},
			{"kind": "pr_review", "title": "PR aguardando revisão", "summary": ""},
		},
	}
}

// campoComSegredo procura o segredo em qualquer campo alcançável, e devolve o
// caminho até ele.
//
// `reflect.Value.String()` LÊ campo não exportado (só `Interface()` é que
// panica), e é isso que torna esta checagem possível sem `unsafe`. Closures não
// são percorríveis — o que é exatamente o ponto: o que está capturado num
// closure não é alcançável nem por aqui, nem pelo fmt, nem por um dump de
// depurador.
func campoComSegredo(v reflect.Value, segredo, caminho string, prof int) string {
	// Teto de profundidade: o adaptador carrega um http.Client, que carrega um
	// Transport, que carrega o mundo. O segredo, se estiver guardado, está
	// perto da superfície.
	if prof > 6 || !v.IsValid() || segredo == "" {
		return ""
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return campoComSegredo(v.Elem(), segredo, caminho, prof+1)
	case reflect.String:
		if strings.Contains(v.String(), segredo) {
			return caminho
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			nome := v.Type().Field(i).Name
			if achado := campoComSegredo(v.Field(i), segredo, caminho+"."+nome, prof+1); achado != "" {
				return achado
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return ""
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			// []byte é o outro jeito óbvio de guardar credencial.
			if b, ok := comoBytes(v); ok && strings.Contains(string(b), segredo) {
				return caminho
			}
			return ""
		}
		for i := 0; i < v.Len(); i++ {
			if achado := campoComSegredo(v.Index(i), segredo, fmt.Sprintf("%s[%d]", caminho, i), prof+1); achado != "" {
				return achado
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return ""
		}
		for _, k := range v.MapKeys() {
			if achado := campoComSegredo(v.MapIndex(k), segredo, fmt.Sprintf("%s[%v]", caminho, k), prof+1); achado != "" {
				return achado
			}
		}
	}
	return ""
}

// comoBytes lê um []byte mesmo não exportado, byte a byte — `Bytes()` recusa
// valor obtido de campo não exportado, `Index(i).Uint()` não.
func comoBytes(v reflect.Value) ([]byte, bool) {
	out := make([]byte, v.Len())
	for i := range out {
		out[i] = byte(v.Index(i).Uint())
	}
	return out, true
}

// derefSeguro devolve o VALOR apontado, quando o adaptador é ponteiro.
//
// Existe porque `%+v` de um ponteiro com String() por valor imprime o String();
// o que expõe campo é `%+v` do VALOR. Sem isto, a garantia 4 estaria sendo
// verificada no caminho mais fácil de passar.
func derefSeguro(m ports.Mailer) any {
	v := reflect.ValueOf(m)
	if v.Kind() == reflect.Ptr && !v.IsNil() {
		return v.Elem().Interface()
	}
	return m
}
