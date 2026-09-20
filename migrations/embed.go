// Package migrations embeds the schema's history into the binary, so that the
// process that owns the database applies it (ADR-0024 §2) and nothing is
// copied into a pod by hand.
//
// A migration carries the schema and the reference data the schema cannot
// exist without (the platform flow of 0005). Data that an environment loads
// lives in ../seed.
package migrations

import "embed"

// FS holds every *.sql in this directory, in goose's Up/Down form.
//
//go:embed *.sql
var FS embed.FS
