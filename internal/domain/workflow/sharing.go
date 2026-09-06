// This file opens the SHARING vocabulary for the workflow domain (flow sharing
// spec §3.2): what happens to a derivation that already exists when the flow it
// was made from gets revoked.
package workflow

// RevocationPolicy is what a revocation DOES to derivations that already exist.
//
// The three names are borrowed, not invented: `prospective` is the legal and
// API-deprecation sense of "applies going forward" (it is crates.io's `yank`);
// `drain` is the load balancer's and `kubectl drain`'s "stop taking new work,
// let current work end"; `terminate` is k8s/systemd's "end now".
type RevocationPolicy string

const (
	// PolicyProspective revokes the GRANT only: no new derivation is possible
	// and every existing one goes on working, untouched.
	PolicyProspective RevocationPolicy = "prospective"
	// PolicyDrain revokes the derivations, but a demand already running finishes
	// under the version it froze on start (ADR-0014 §4).
	PolicyDrain RevocationPolicy = "drain"
	// PolicyTerminate revokes at once: a running demand stops at its current gate
	// and raises an attention item.
	PolicyTerminate RevocationPolicy = "terminate"
)

// DefaultRevocationPolicy is the permissive one, and deliberately so: the
// destructive behaviour has to be chosen, never inherited by omission.
const DefaultRevocationPolicy = PolicyProspective

// ValidRevocationPolicy accepts only the three predefined policies. An unknown
// value is a contract error, not user data — whoever sends it is speaking a
// vocabulary this platform does not have.
func ValidRevocationPolicy(p RevocationPolicy) bool {
	switch p {
	case PolicyProspective, PolicyDrain, PolicyTerminate:
		return true
	}
	return false
}
