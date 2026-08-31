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
