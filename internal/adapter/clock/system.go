// Adaptadores da porta Clock.
//
// Duas implementações desde o primeiro dia, pela mesma razão da ADR-0001: a
// porta com um adaptador só é palpite. Aqui a dupla tem função prática — o
// relógio de sistema roda em produção e o fixo roda no teste, e é o segundo
// que transforma regra de prazo (convite de 14 dias) em asserção exata em vez
// de espera cronometrada.
package clock

import (
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// System é o relógio de parede do processo.
type System struct{}

func NewSystem() System { return System{} }

// Now devolve o instante em UTC, sempre.
//
// Por que normalizar aqui e não em cada chamador: timestamp sem fuso definido
// é ambiguidade que só aparece quando o processo sobe numa máquina com TZ
// diferente do banco — e aí a comparação de expiração erra por horas. O fuso é
// decisão de apresentação, e apresentação é da borda.
//
// Efeito colateral desejado de .UTC(): a leitura monotônica é descartada, de
// modo que dois relógios diferentes produzam instantes comparáveis entre si.
func (System) Now() time.Time { return time.Now().UTC() }

var _ ports.Clock = System{}
