package attention

import (
	"sort"
	"time"
)

// Sort ordena a caixa: mais urgente primeiro.
//
// Ordenação ESTÁVEL e com desempate final por id. Sem o desempate, dois itens
// de mesmo tipo abertos no mesmo instante trocariam de lugar entre uma consulta
// e outra — e lista que se mexe sozinha é lista em que ninguém confia.
func Sort(itens []Item, agora time.Time) {
	sort.SliceStable(itens, func(i, j int) bool {
		pi, pj := itens[i].Priority(agora), itens[j].Priority(agora)
		if pi != pj {
			return pi < pj
		}
		return itens[i].ID < itens[j].ID
	})
}
