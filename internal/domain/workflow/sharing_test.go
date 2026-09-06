package workflow_test

import (
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/domain/workflow"
)

func TestRevocationPolicyVocabularyIsClosed(t *testing.T) {
	for _, p := range []workflow.RevocationPolicy{
		workflow.PolicyProspective, workflow.PolicyDrain, workflow.PolicyTerminate,
	} {
		if !workflow.ValidRevocationPolicy(p) {
			t.Fatalf("%q has to be accepted", p)
		}
	}
	// An unknown value is a contract error, not user data: whoever sends it is
	// speaking a vocabulary this platform does not have.
	for _, p := range []workflow.RevocationPolicy{"", "soft", "hard", "cascade"} {
		if workflow.ValidRevocationPolicy(p) {
			t.Fatalf("%q must not be accepted", p)
		}
	}
	if workflow.DefaultRevocationPolicy != workflow.PolicyProspective {
		t.Fatal("the default has to be prospective: revoking must not reach a copy unless somebody chose that")
	}
}
