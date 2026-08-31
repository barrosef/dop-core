package contract

import (
	"sync"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// ClockSuite verifica as quatro garantias documentadas na porta Clock.
//
// Parece pouco para uma porta de uma linha só, e é justamente o ponto: foi por
// parecer trivial demais para ter adaptador que ela passou meses sendo passada
// como nil no composition root, com o domínio chamando time.Now() por dentro.
func ClockSuite(t *testing.T, name string, newClock func(t *testing.T) ports.Clock) {
	t.Run(name, func(t *testing.T) {
		t.Run("1_nunca_devolve_o_instante_zero", func(t *testing.T) {
			c := newClock(t)
			if c.Now().IsZero() {
				t.Fatal("Now() devolveu o instante zero — regra de prazo calculada a partir " +
					"do ano 1 expira tudo, ou nada, sem avisar")
			}
		})

		t.Run("2_sempre_em_utc", func(t *testing.T) {
			c := newClock(t)
			got := c.Now()
			if got.Location() != time.UTC {
				t.Fatalf("Now() devolveu fuso %v; a porta exige UTC", got.Location())
			}
			// _, offset := got.Zone() confirma que não é um "UTC" só no nome.
			if _, offset := got.Zone(); offset != 0 {
				t.Fatalf("deslocamento de fuso %ds; a porta exige UTC", offset)
			}
		})

		t.Run("3_nao_regride", func(t *testing.T) {
			c := newClock(t)
			anterior := c.Now()
			for i := 0; i < 1000; i++ {
				atual := c.Now()
				if atual.Before(anterior) {
					t.Fatalf("Now() regrediu na chamada %d: %s veio depois de %s",
						i, atual.Format(time.RFC3339Nano), anterior.Format(time.RFC3339Nano))
				}
				anterior = atual
			}
		})

		t.Run("4_seguro_para_uso_concorrente", func(t *testing.T) {
			c := newClock(t)
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					anterior := c.Now()
					for i := 0; i < 500; i++ {
						atual := c.Now()
						if atual.IsZero() || atual.Before(anterior) {
							t.Errorf("leitura inconsistente sob concorrência: %v depois de %v", atual, anterior)
							return
						}
						anterior = atual
					}
				}()
			}
			wg.Wait()
		})
	})
}
