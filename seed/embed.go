// Package seed embeds the data an environment loads after its migrations
// (ADR-0024 §3).
//
// A seed is idempotent BY CONTRACT — `INSERT … ON CONFLICT DO UPDATE` — and is
// applied in file order every time the worker starts. There is no seed version
// table: re-running is how a row is edited. The root holds what every
// environment needs; a subdirectory holds what one profile needs
// (`local/` for development) and runs only when DOP_SEED_PROFILE names it.
package seed

import "embed"

// FS holds the root seeds and every profile directory. `all:` keeps a profile
// directory that has no .sql yet (only its README) in the image.
//
//go:embed *.sql all:local
var FS embed.FS
