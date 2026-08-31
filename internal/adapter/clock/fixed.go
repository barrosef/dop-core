package clock

import (
	"sync"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// referencia é o instante usado quando ninguém escolhe um.
//
// Existe para que NewFixed(time.Time{}) continue cumprindo a garantia da porta
// ("Now nunca devolve o zero") em vez de produzir um relógio que passa no
// compilador e falha no contrato.
var referencia = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// Fixed é o relógio controlado pelo teste: só anda quando mandam andar.
//
// É o que substitui time.Sleep. Testar "convite vence em 14 dias" com relógio
// de verdade exige ou esperar 14 dias, ou fabricar um ExpiresAt no passado —
// isto é, testar outra coisa. Com o relógio fixo, o teste avança 14 dias em
// nanossegundos e verifica a regra REAL, a mesma que roda em produção.
type Fixed struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFixed cria o relógio parado em t (normalizado para UTC, como manda a porta).
func NewFixed(t time.Time) *Fixed {
	if t.IsZero() {
		t = referencia
	}
	return &Fixed{now: t.UTC()}
}

func (f *Fixed) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Advance empurra o relógio para frente e devolve o novo instante.
//
// Só para frente, de propósito: a porta garante que Now não regride, e um
// relógio de teste que anda para trás produziria um adaptador que passa no
// contrato por acidente do caminho percorrido. Duração negativa é erro de
// teste, e erro de teste tem que aparecer alto.
func (f *Fixed) Advance(d time.Duration) time.Time {
	if d < 0 {
		panic("clock.Fixed.Advance: o relógio não anda para trás")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

var _ ports.Clock = (*Fixed)(nil)
