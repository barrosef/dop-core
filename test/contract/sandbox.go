package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// SandboxEnv descreve o que ESTE substrato tem para a suíte trabalhar.
//
// Existe porque as duas coisas que mudam entre um cluster e o Docker do host
// não são comportamento, e sim ambiente: o nome do espaço onde criar e qual
// nível de isolamento a máquina realmente oferece. Tudo o mais — comando,
// marcador de workspace, tempos — é da suíte, para que os dois adaptadores
// sejam medidos com a MESMA régua.
type SandboxEnv struct {
	// NamespacePrefix precisa ser válido em DNS-1123: o k8s exige, o Docker
	// aceita, e usar a regra mais estrita nos dois é o que mantém o mesmo teste
	// rodando dos dois lados.
	NamespacePrefix string
	Image           string
	// Tier é o que este substrato entrega. Unsupported é um que ele NÃO
	// entrega — é o caso que prova a recusa em vez da degradação.
	Tier        ports.IsolationTier
	Unsupported ports.IsolationTier
	// Ready é quanto esperar por uma mudança de fase. Um pod puxando imagem
	// demora muito mais que um contêiner local.
	Ready time.Duration
}

// SandboxSuite verifica as doze garantias documentadas na porta SandboxLauncher.
//
// Disciplina da ADR-0001: uma porta com um adaptador só é palpite. O Kubernetes
// e o Docker não têm UMA linha em comum na implementação — é só passando os dois
// por esta suíte que "trocar de substrato não muda o comportamento" deixa de ser
// promessa e vira fato verificado.
func SandboxSuite(t *testing.T, name string, newLauncher func(t *testing.T) (ports.SandboxLauncher, SandboxEnv)) {
	t.Run(name, func(t *testing.T) {
		t.Run("3_supported_tiers_nunca_vazio", func(t *testing.T) {
			l, env := newLauncher(t)
			tiers, err := l.SupportedTiers(context.Background())
			if err != nil {
				t.Fatalf("SupportedTiers: %v", err)
			}
			if len(tiers) == 0 {
				t.Fatal("lista vazia sem erro: substrato sem nível nenhum é substrato indisponível")
			}
			for _, tr := range tiers {
				if !ports.ValidIsolationTier(tr) {
					t.Errorf("tier fora do vocabulário: %q", tr)
				}
			}
			if !containsTier(tiers, env.Tier) {
				t.Errorf("o ambiente diz entregar %q, o substrato não o lista: %v", env.Tier, tiers)
			}
			if containsTier(tiers, env.Unsupported) {
				t.Errorf("o ambiente diz NÃO entregar %q, mas o substrato o lista", env.Unsupported)
			}
		})

		t.Run("1_tier_entregue_e_o_declarado", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			st, err := l.Launch(ctx, spec)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if st.Tier != spec.Tier {
				t.Fatalf("DEGRADAÇÃO SILENCIOSA: pedi %q, recebi %q", spec.Tier, st.Tier)
			}
			st = waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			if st.Tier != spec.Tier {
				t.Fatalf("o tier mudou depois do provisionamento: %q != %q", st.Tier, spec.Tier)
			}
		})

		t.Run("2_tier_nao_suportado_recusa_sem_rastro", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)
			spec.Tier = env.Unsupported

			if _, err := l.Launch(ctx, spec); err == nil {
				t.Fatal("aceitou tier que o substrato não oferece — degradar em silêncio é proibido")
			} else if k := errs.KindOf(err); k != errs.KindPrecondition {
				t.Fatalf("esperava KindPrecondition com mensagem, veio %s: %v", k, err)
			}
			// Recusa que provisiona metade é pior que recusa nenhuma.
			if _, err := l.Describe(ctx, spec.SandboxHandle); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("a recusa deixou rastro: Describe devolveu %v", err)
			}
		})

		t.Run("4_launch_idempotente", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("1º Launch: %v", err)
			}
			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("2º Launch deveria devolver o existente: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// Um Destroy só tem de bastar: se o relançamento tivesse criado um
			// segundo sandbox, algo sobreviveria a ele.
			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("Destroy: %v", err)
			}
			waitGone(t, l, spec.SandboxHandle, env.Ready)
		})

		t.Run("5e6_suspende_preserva_workspace_e_resume_retoma", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			waitLog(t, l, spec.SandboxHandle, marcaAusente, env.Ready)

			if err := l.Suspend(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			st := waitPhase(t, l, spec.SandboxHandle, ports.PhaseSuspended, env.Ready)
			if st.Tier != spec.Tier {
				t.Errorf("suspenso perdeu o tier declarado: %q != %q", st.Tier, spec.Tier)
			}

			if _, err := l.Resume(ctx, spec); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// A GARANTIA 5, e só ela: o que estava sob SandboxWorkspacePath
			// sobreviveu. Nada FORA dele é verificado aqui de propósito — o
			// adaptador k8s apaga o pod inteiro na suspensão e o do Docker
			// mantém a camada gravável do contêiner. Exigir o comportamento do
			// Docker faria a suíte reprovar o k8s por cumprir a porta.
			waitLog(t, l, spec.SandboxHandle, marcaPresente, env.Ready)
		})

		t.Run("7_suspend_e_resume_idempotentes", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			if _, err := l.Resume(ctx, spec); err != nil {
				t.Fatalf("Resume de sandbox ativo deveria ser inócuo: %v", err)
			}
			if err := l.Suspend(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("1º Suspend: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseSuspended, env.Ready)
			if err := l.Suspend(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("2º Suspend deveria ser inócuo: %v", err)
			}
		})

		t.Run("8_destroy_irreversivel_e_idempotente", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env)

			if _, err := l.Launch(ctx, spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("Destroy: %v", err)
			}
			waitGone(t, l, spec.SandboxHandle, env.Ready)

			// Irreversível quer dizer que não há caminho de volta PELA PORTA.
			if _, err := l.Resume(ctx, spec); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("destruído voltou a viver: Resume devolveu %v", err)
			}
			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Fatalf("2º Destroy deveria ser inócuo: %v", err)
			}
		})

		t.Run("9_inexistente", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			spec := newSpec(t, l, env) // nunca lançado

			if _, err := l.Describe(ctx, spec.SandboxHandle); errs.KindOf(err) != errs.KindNotFound {
				t.Errorf("Describe: esperava KindNotFound, veio %v", err)
			}
			if err := l.Suspend(ctx, spec.SandboxHandle); errs.KindOf(err) != errs.KindNotFound {
				t.Errorf("Suspend: esperava KindNotFound, veio %v", err)
			}
			if _, err := l.Resume(ctx, spec); errs.KindOf(err) != errs.KindNotFound {
				t.Errorf("Resume: esperava KindNotFound, veio %v", err)
			}
			// Só o Destroy trata ausência como sucesso: nele a ausência é o
			// resultado desejado.
			if err := l.Destroy(ctx, spec.SandboxHandle); err != nil {
				t.Errorf("Destroy de inexistente deveria ser inócuo: %v", err)
			}
		})

		t.Run("10_sandboxes_nao_interferem", func(t *testing.T) {
			l, env := newLauncher(t)
			ctx := context.Background()
			a, b := newSpec(t, l, env), newSpec(t, l, env)

			if _, err := l.Launch(ctx, a); err != nil {
				t.Fatalf("Launch A: %v", err)
			}
			if _, err := l.Launch(ctx, b); err != nil {
				t.Fatalf("Launch B: %v", err)
			}
			waitPhase(t, l, a.SandboxHandle, ports.PhaseActive, env.Ready)
			waitPhase(t, l, b.SandboxHandle, ports.PhaseActive, env.Ready)

			if err := l.Destroy(ctx, a.SandboxHandle); err != nil {
				t.Fatalf("Destroy A: %v", err)
			}
			waitGone(t, l, a.SandboxHandle, env.Ready)

			st, err := l.Describe(ctx, b.SandboxHandle)
			if err != nil {
				t.Fatalf("destruir A afetou B: %v", err)
			}
			if st.Phase != ports.PhaseActive {
				t.Fatalf("B saiu de ativo por causa de A: %s", st.Phase)
			}
		})

		t.Run("11_tail_morre_junto_com_o_cliente", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- l.Tail(ctx, spec.SandboxHandle, ports.LogQuery{Follow: true},
					func(ports.LogLine) error { return nil })
			}()
			// Deixa o follow pegar o fluxo antes de puxar o tapete.
			time.Sleep(500 * time.Millisecond)
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("cancelamento do cliente não é falha: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("Tail não morreu com o cliente — goroutine vazada por chamada")
			}
		})

		t.Run("12_erro_do_emit_interrompe_o_tail", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			waitLog(t, l, spec.SandboxHandle, marcaAusente, env.Ready)

			boom := errs.Internal("cliente sumiu")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := l.Tail(ctx, spec.SandboxHandle, ports.LogQuery{Follow: true},
				func(ports.LogLine) error { return boom })
			if err == nil {
				t.Fatal("erro do emit precisa subir: é como o servidor sabe que o cliente sumiu")
			}
		})

		t.Run("endpoints_vazios_sem_porta_publicada", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			st := waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			// Endpoint inventado faz o cockpit oferecer link que não abre.
			if len(st.Endpoints) != 0 {
				t.Fatalf("sandbox sem porta publicada devolveu %d endpoint(s): %+v",
					len(st.Endpoints), st.Endpoints)
			}
		})

		// ── Exec: as garantias 13 a 18 ──────────────────────────────────────
		//
		// Elas entraram quando exec entrou na porta, e cada uma existe porque a
		// implementação ingênua correspondente PASSA sem elas: um exec que lê
		// só o stream devolve código de saída zero para todo comando que
		// falhou; um que confia no `Env` do processo do núcleo entrega a
		// credencial do processo ao código do agente; um que lê até o EOF sem
		// teto transforma um `cat` de log numa fatura.

		t.Run("13e14_exec_roda_dentro_do_sandbox_e_no_workspace", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			// O sandbox de teste grava `marca` no workspace ao subir; esperar
			// a linha dele é esperar o arquivo existir.
			waitLog(t, l, spec.SandboxHandle, marcaAusente, env.Ready)

			// `pwd` prova a garantia 14 (o diretório de trabalho é o workspace,
			// e nenhum dos dois adaptadores recebeu isso por parâmetro — os
			// dois o fixam no contêiner). `cat marca` sem caminho absoluto
			// prova as duas de uma vez: só funciona se o comando começou lá.
			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "pwd; cat marca"},
			})
			if res.ExitCode != 0 {
				t.Fatalf("exit %d, stderr=%q", res.ExitCode, res.Stderr)
			}
			if !strings.Contains(res.Stdout, ports.SandboxWorkspacePath) {
				t.Fatalf("o comando não começou em %s: pwd disse %q",
					ports.SandboxWorkspacePath, res.Stdout)
			}
			if !strings.Contains(res.Stdout, "ok") {
				t.Fatalf("o comando não enxergou o workspace do sandbox: %q", res.Stdout)
			}
		})

		t.Run("15_codigo_de_saida_nao_e_erro_da_porta", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// É a garantia que sustenta o laço de ferramenta inteiro: o modelo
			// precisa VER que o comando falhou para corrigir. Um erro de
			// transporte no lugar disto apagaria a diferença entre "o teste
			// reprovou" e "o substrato caiu".
			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "exit 7"},
			})
			if res.ExitCode != 7 {
				t.Fatalf("CÓDIGO DE SAÍDA PERDIDO: veio %d, esperava 7 — um exec que lê só o "+
					"stream devolve 0 para tudo, e o agente lê falha como sucesso", res.ExitCode)
			}
			// E o zero continua sendo zero: sem esta metade, um adaptador que
			// devolvesse -1 sempre passaria na de cima.
			if ok := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"true"},
			}); ok.ExitCode != 0 {
				t.Fatalf("comando bem-sucedido devolveu código %d", ok.ExitCode)
			}
		})

		t.Run("16_stdout_e_stderr_separados", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// Diferente do Tail, onde o k8s funde os dois e a porta não promete
			// nada: no exec os dois substratos separam de verdade.
			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "echo SAIDA-PADRAO; echo SAIDA-DE-ERRO 1>&2"},
			})
			if !strings.Contains(res.Stdout, "SAIDA-PADRAO") {
				t.Fatalf("stdout não trouxe a linha de stdout: %q", res.Stdout)
			}
			if !strings.Contains(res.Stderr, "SAIDA-DE-ERRO") {
				t.Fatalf("stderr não trouxe a linha de stderr: %q", res.Stderr)
			}
			if strings.Contains(res.Stdout, "SAIDA-DE-ERRO") {
				t.Fatalf("os dois fluxos vieram fundidos em stdout: %q", res.Stdout)
			}
		})

		t.Run("17_saida_limitada_e_prazo_respeitado", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// ~22 KB de saída contra um teto de 1 KB. Shell puro de propósito:
			// `yes | head` mata o produtor com SIGPIPE e mediria outra coisa.
			grande := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c",
					"i=0; while [ $i -lt 2000 ]; do echo 0123456789; i=$((i+1)); done"},
				MaxOutputBytes: 1024,
			})
			if len(grande.Stdout) > 1024 {
				t.Fatalf("SAÍDA SEM TETO: vieram %d bytes contra um teto de 1024. Saída de "+
					"ferramenta vira contexto de modelo, e contexto é fatura (ADR-0011)",
					len(grande.Stdout))
			}
			if !grande.Truncated {
				t.Fatal("a saída foi cortada e Truncated veio falso: um agente que conclui a " +
					"partir de saída cortada sem saber conclui errado")
			}
			// O código de saída continua vindo mesmo com a saída cortada — é o
			// que prova que o adaptador continuou drenando o fluxo em vez de
			// fechar a conexão no teto.
			if grande.ExitCode != 0 {
				t.Fatalf("com a saída cortada, o código de saída se perdeu: %d", grande.ExitCode)
			}

			inicio := time.Now()
			pendurado := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "sleep 60"}, TimeoutSeconds: 3,
			})
			if !pendurado.TimedOut {
				t.Fatal("o comando não terminou no prazo e TimedOut veio falso")
			}
			if pendurado.ExitCode == 0 {
				t.Fatal("comando pendurado devolveu código 0: zero afirma sucesso, e não houve")
			}
			if decorrido := time.Since(inicio); decorrido > 40*time.Second {
				t.Fatalf("o prazo de 3s foi ignorado: a chamada levou %s", decorrido)
			}
		})

		t.Run("18_exec_em_suspenso_e_em_inexistente", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			cmd := ports.ExecRequest{Command: []string{"true"}}

			// Inexistente: NotFound, como Describe.
			if _, err := l.Exec(context.Background(), spec.SandboxHandle, cmd); errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("exec em sandbox inexistente: esperava KindNotFound, veio %v", err)
			}

			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)
			if err := l.Suspend(context.Background(), spec.SandboxHandle); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseSuspended, env.Ready)

			// Suspenso: PRECONDIÇÃO, e nunca um código de saída inventado.
			// Substrato sem execução não roda comando, e dizer isso é diferente
			// de dizer que o comando falhou.
			res, err := l.Exec(context.Background(), spec.SandboxHandle, cmd)
			if err == nil {
				t.Fatalf("exec em sandbox SUSPENSO devolveu resultado: %+v", res)
			}
			if k := errs.KindOf(err); k != errs.KindPrecondition {
				t.Fatalf("exec em sandbox suspenso: esperava KindPrecondition, veio %s: %v", k, err)
			}
		})

		t.Run("exec_nao_leva_o_ambiente_do_nucleo_para_dentro", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			// O núcleo é o processo que TEM as credenciais — a chave do
			// provedor de modelo sai do cofre e vive na memória dele
			// (ADR-0023). O sandbox roda código de agente, que lê conteúdo não
			// confiável (spec do substrato §6). Um adaptador que passasse
			// `os.Environ()` para o exec — que é o caminho mais curto e o que
			// um SDK faria por conveniência — entregaria as duas coisas uma à
			// outra, e nada falharia.
			const sentinela = "SENTINELA_DO_NUCLEO_NAO_PODE_ENTRAR_NO_SANDBOX"
			t.Setenv("DOP_SENTINELA_DE_CREDENCIAL", sentinela)

			res := execOK(t, l, spec.SandboxHandle, ports.ExecRequest{
				Command: []string{"sh", "-c", "env; echo ---; set"},
			})
			if strings.Contains(res.Stdout, sentinela) {
				t.Fatal("O AMBIENTE DO PROCESSO DO NÚCLEO VAZOU PARA DENTRO DO SANDBOX: " +
					"é lá que mora a credencial do provedor de agente, e é ali que roda o " +
					"código que lê conteúdo não confiável")
			}
		})

		t.Run("processo_desconhecido_no_tail_e_not_found", func(t *testing.T) {
			l, env := newLauncher(t)
			spec := newSpec(t, l, env)
			if _, err := l.Launch(context.Background(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			waitPhase(t, l, spec.SandboxHandle, ports.PhaseActive, env.Ready)

			err := l.Tail(context.Background(), spec.SandboxHandle,
				ports.LogQuery{Service: "nao-existe"}, func(ports.LogLine) error { return nil })
			if errs.KindOf(err) != errs.KindNotFound {
				t.Fatalf("esperava KindNotFound para processo inexistente, veio %v", err)
			}
		})
	})
}

// ── o sandbox de teste ───────────────────────────────────────────────────────

// As duas frases que o sandbox de teste imprime ao subir. Elas são o
// instrumento da garantia 5: o marcador só existe no workspace, então a segunda
// frase só aparece se o workspace tiver sobrevivido à suspensão.
const (
	marcaAusente  = "MARCA=ausente"
	marcaPresente = "MARCA=gravada"
)

// sandboxCommand imprime o estado do marcador, grava-o e fica vivo.
//
// O `[infra]` no fim é de propósito: é a convenção de classificação de log do
// domínio, e vê-la atravessar o substrato inteiro prova que nenhum dos dois
// adaptadores mexe no texto da linha.
func sandboxCommand() []string {
	return []string{"sh", "-c",
		"if [ -f " + ports.SandboxWorkspacePath + "/marca ]; then echo " + marcaPresente +
			"; else echo " + marcaAusente + "; fi; " +
			"echo ok > " + ports.SandboxWorkspacePath + "/marca; " +
			"echo '[infra] sandbox pronto'; sleep 900"}
}

// newSpec monta um sandbox novo e registra a limpeza. Cada subteste ganha o seu:
// namespace compartilhado entre testes esconderia justamente a interferência que
// a garantia 10 existe para detectar.
func newSpec(t *testing.T, l ports.SandboxLauncher, env SandboxEnv) ports.SandboxSpec {
	t.Helper()
	id := randomID()
	spec := ports.SandboxSpec{
		SandboxHandle: ports.SandboxHandle{
			ID:        id,
			Namespace: env.NamespacePrefix + "-" + id,
		},
		AccountID: "conta-de-contrato",
		DemandID:  "demanda-" + id,
		Tier:      env.Tier,
		Image:     env.Image,
		Command:   sandboxCommand(),
		Env:       map[string]string{"DOP_SANDBOX_ID": id},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = l.Destroy(ctx, spec.SandboxHandle)
	})
	return spec
}

// execOK roda um comando e exige que a PORTA não tenha falhado.
//
// Repare no que ela NÃO checa: o código de saída. Erro aqui é falha do
// substrato; o que o comando fez — inclusive falhar — é assunto de quem chamou.
// Misturar os dois neste auxiliar apagaria a garantia 15 de todos os subtestes
// que o usam.
func execOK(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, req ports.ExecRequest) *ports.ExecResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := l.Exec(ctx, h, req)
	if err != nil {
		t.Fatalf("Exec(%v): %v", req.Command, err)
	}
	if res == nil {
		t.Fatalf("Exec(%v) devolveu resultado nulo sem erro", req.Command)
	}
	return res
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func containsTier(list []ports.IsolationTier, want ports.IsolationTier) bool {
	for _, t := range list {
		if t == want {
			return true
		}
	}
	return false
}

// ── espera ───────────────────────────────────────────────────────────────────
//
// Provisionar é assíncrono nos dois substratos: Launch devolve quando o pedido
// foi aceito, não quando o processo subiu. Esperar aqui, e não dentro do
// adaptador, é o que mantém a porta não-bloqueante para quem chama de verdade.

func waitPhase(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, want ports.SandboxPhase, d time.Duration) *ports.SandboxStatus {
	t.Helper()
	if d <= 0 {
		d = 90 * time.Second
	}
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		st, err := l.Describe(context.Background(), h)
		if err != nil {
			last = err.Error()
		} else {
			last = string(st.Phase)
			if st.Phase == want {
				return st
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("o sandbox não chegou a %q em %s (último: %s)", want, d, last)
	return nil
}

func waitGone(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, d time.Duration) {
	t.Helper()
	if d <= 0 {
		d = 90 * time.Second
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := l.Describe(context.Background(), h); errs.KindOf(err) == errs.KindNotFound {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("o sandbox continuou existindo %s depois de destruído", d)
}

// waitLog espera uma frase aparecer no log. Usa Follow=false de propósito: é a
// leitura do que o substrato JÁ tem, que é o que interessa para provar
// persistência de workspace.
func waitLog(t *testing.T, l ports.SandboxLauncher, h ports.SandboxHandle, want string, d time.Duration) {
	t.Helper()
	if d <= 0 {
		d = 90 * time.Second
	}
	deadline := time.Now().Add(d)
	var seen []string
	for time.Now().Before(deadline) {
		seen = seen[:0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := l.Tail(ctx, h, ports.LogQuery{Follow: false}, func(line ports.LogLine) error {
			seen = append(seen, line.Text)
			return nil
		})
		cancel()
		if err == nil {
			for _, line := range seen {
				if strings.Contains(line, want) {
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("a frase %q não apareceu no log em %s (linhas vistas: %v)", want, d, seen)
}
