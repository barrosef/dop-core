// Package catalog is what the onboarding journey reads and never writes: the
// plans a person may choose and the providers a connection may point at.
//
// It is data, not code (spec 2026-09-20 D-4, D-6): the cockpit knows neither
// list, the seed lives in a migration, and the only rule here is "active only",
// which belongs to the query.
package catalog

// Plan is a row of plan_catalog. It carries no price on purpose (spec D-6).
type Plan struct {
	Key      string
	Name     string
	Tagline  string
	Features []string
	Sort     int
	Active   bool
}

// Provider is a row of provider_catalog: one tile on the connections step and,
// through Operated, the honest answer to "does the platform do anything with
// this credential yet".
type Provider struct {
	Key            string
	Category       string // git | task_manager — mirrors resource.Category
	Name           string
	CredentialKind string
	Permissions    []string
	NeedsBaseURL   bool
	Operated       bool
	DocsURL        string
	BrandColor     string
	Sort           int
	Active         bool
}
