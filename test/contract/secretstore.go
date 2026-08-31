// Package contract carrega os testes de CONTRATO das portas.
//
// Disciplina da ADR-0001: uma porta com um adaptador só é palpite. O mesmo
// conjunto roda contra TODO adaptador — memória, k8s, GCP Secret Manager — e é
// ele que garante substituibilidade de fato, não de intenção.
package contract

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// SecretStoreSuite verifica as seis garantias documentadas na porta.
// refSeq garante unicidade mesmo dentro do mesmo nanossegundo.
var refSeq atomic.Int64

func SecretStoreSuite(t *testing.T, name string, newStore func(t *testing.T) ports.SecretStore) {
	t.Run(name, func(t *testing.T) {
		// Contas ÚNICAS por execução, e não literais fixos.
		//
		// A primeira versão usava "acct-a"/"acct-b" fixos, e a suíte passava —
		// contra o duplo em memória, onde `newStore` devolve um cofre novo a
		// cada subteste. Contra um backend REAL, `newStore` devolve um cliente
		// novo para o MESMO cofre, e o segredo gravado no subteste 1 fazia o
		// subteste de `Exists` falhar por encontrar o que ele mesmo deixara.
		//
		// Era a suíte escrita em cima do duplo: ela verificava a porta, mas
		// carregava junto uma suposição que só o duplo cumpria. Adaptador real
		// nenhum passaria — e nenhum estava sendo rodado, o que fechava o
		// círculo.
		//
		// Limpar no fim não bastaria: subteste que falha no meio deixa
		// resíduo, e a execução seguinte falharia por causa da anterior.
		id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), refSeq.Add(1))
		refA := ports.SecretRef{AccountID: "acct-a-" + id, Kind: "integration_credential", OwnerID: "res-1"}
		refB := ports.SecretRef{AccountID: "acct-b-" + id, Kind: "integration_credential", OwnerID: "res-1"}
		val := ports.SecretValue("token-super-secreto")

		// A limpeza pega o cofre AGORA, não no fim: `newStore` pode pular a
		// suíte quando a infra não responde, e pular dentro de um Cleanup
		// marcaria o teste como SKIP depois de todos os subtestes terem
		// PASSADO — saída que mente sobre o que aconteceu.
		limpeza := newStore(t)
		t.Cleanup(func() {
			_ = limpeza.Delete(context.Background(), refA)
			_ = limpeza.Delete(context.Background(), refB)
		})

		t.Run("1_leitura_apos_escrita_imediata", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			if err := s.Put(ctx, refA, val); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := s.Get(ctx, refA)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, val) {
				t.Fatalf("valor divergente: %q != %q", got, val)
			}
		})

		t.Run("2_ausente_devolve_nil_sem_erro", func(t *testing.T) {
			s := newStore(t)
			got, err := s.Get(context.Background(),
				ports.SecretRef{AccountID: "acct-x", Kind: "integration_credential", OwnerID: "nao-existe"})
			if err != nil {
				t.Fatalf("esperava nil sem erro, veio erro: %v", err)
			}
			if got != nil {
				t.Fatalf("esperava nil, veio %q", got)
			}
		})

		t.Run("3_delete_idempotente", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			_ = s.Put(ctx, refA, val)
			if err := s.Delete(ctx, refA); err != nil {
				t.Fatalf("1º Delete: %v", err)
			}
			if err := s.Delete(ctx, refA); err != nil {
				t.Fatalf("2º Delete deveria ser inócuo: %v", err)
			}
		})

		t.Run("4_put_substitui", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			_ = s.Put(ctx, refA, ports.SecretValue("antigo"))
			_ = s.Put(ctx, refA, ports.SecretValue("novo"))
			got, _ := s.Get(ctx, refA)
			if string(got) != "novo" {
				t.Fatalf("esperava 'novo', veio %q", got)
			}
		})

		t.Run("5_isolamento_entre_contas", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			if err := s.Put(ctx, refA, ports.SecretValue("da-conta-a")); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := s.Get(ctx, refB)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got != nil {
				t.Fatalf("VAZAMENTO ENTRE CONTAS: conta B leu %q", got)
			}
		})

		t.Run("6_valor_nao_vaza_em_texto", func(t *testing.T) {
			// SecretValue não pode revelar o conteúdo ao ser formatado.
			v := ports.SecretValue("nunca-me-mostre")
			if got := v.String(); got != "***" {
				t.Fatalf("String() deveria redigir, devolveu %q", got)
			}
			if formatted := fmtValue(v); formatted != "***" {
				t.Fatalf("formatação com %%v deveria redigir, devolveu %q", formatted)
			}
		})

		t.Run("exists_reflete_estado", func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			// Referência PRÓPRIA: este é o único subteste que afirma algo
			// sobre a AUSÊNCIA, e `refA` já foi gravada pelos anteriores. No
			// duplo isso não aparecia porque cada subteste ganhava um cofre
			// novo; num cofre real, ausência exige uma chave que ninguém tocou.
			refNova := refA
			refNova.OwnerID = "res-exists-" + strconv.FormatInt(refSeq.Add(1), 10)
			t.Cleanup(func() { _ = s.Delete(context.Background(), refNova) })

			if ok, _ := s.Exists(ctx, refNova); ok {
				t.Fatal("Exists deveria ser falso antes do Put")
			}
			_ = s.Put(ctx, refNova, val)
			if ok, _ := s.Exists(ctx, refNova); !ok {
				t.Fatal("Exists deveria ser verdadeiro após o Put")
			}
		})
	})
}
