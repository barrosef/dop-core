// Package contract carrega os testes de CONTRATO das portas.
//
// Disciplina da ADR-0001: uma porta com um adaptador só é palpite. O mesmo
// conjunto roda contra TODO adaptador — memória, k8s, GCP Secret Manager — e é
// ele que garante substituibilidade de fato, não de intenção.
package contract

import (
	"bytes"
	"context"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// SecretStoreSuite verifica as seis garantias documentadas na porta.
func SecretStoreSuite(t *testing.T, name string, newStore func(t *testing.T) ports.SecretStore) {
	t.Run(name, func(t *testing.T) {
		refA := ports.SecretRef{AccountID: "acct-a", Kind: "integration_credential", OwnerID: "res-1"}
		refB := ports.SecretRef{AccountID: "acct-b", Kind: "integration_credential", OwnerID: "res-1"}
		val := ports.SecretValue("token-super-secreto")

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
			if ok, _ := s.Exists(ctx, refA); ok {
				t.Fatal("Exists deveria ser falso antes do Put")
			}
			_ = s.Put(ctx, refA, val)
			if ok, _ := s.Exists(ctx, refA); !ok {
				t.Fatal("Exists deveria ser verdadeiro após o Put")
			}
		})
	})
}
