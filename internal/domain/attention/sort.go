package attention

import (
	"sort"
	"time"
)

// Sort orders the box: most urgent first.
//
// A STABLE sort with a final tie-break by id. Without the tie-break, two items
// of the same kind opened at the same instant would swap places between one
// query and the next — and a list that moves on its own is a list nobody trusts.
func Sort(items []Item, now time.Time) {
	sort.SliceStable(items, func(i, j int) bool {
		pi, pj := items[i].Priority(now), items[j].Priority(now)
		if pi != pj {
			return pi < pj
		}
		return items[i].ID < items[j].ID
	})
}
