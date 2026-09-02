//go:build integration

// The SAME contract suite, now against Secret Manager.
//
//	go test ./test/contract/ -tags=integration -v -run SecretStore
//
// Locally the target is the community emulator (there is no official one; P-17
// in the ROADMAP). Pointing SECRET_MANAGER_EMULATOR_HOST at nothing and
// supplying a credential, the SAME function runs against real GCP — which is the
// only way to discover the divergences listed in the adapter's header.
//
// BEWARE when reading a PASS here: the emulator is more permissive than real GCP
// in nine points documented in internal/adapter/secretstore/gcp.go, and one of
// them is the port's guarantee 1 — read-after-write, which Google does NOT
// promise through the `latest` alias. Green here is no proof of green there.
package contract_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

func TestSecretStoreContractGCP(t *testing.T) {
	endpoint := os.Getenv("SECRET_MANAGER_EMULATOR_HOST")
	if endpoint == "" {
		endpoint = "127.0.0.1:8085"
	}

	// One PROJECT per newStore call. The suite calls newStore once per subtest
	// and counts on clean state — the `exists_reflects_state` subtest requires
	// reference A not to exist, and it has already been written by four earlier
	// subtests. Against an in-memory backend that is automatic; against a REAL
	// backend, the isolation has to come from somewhere, and here it comes from
	// the project's name. (On real GCP one project per subtest is impractical:
	// see the note at the end of this file.)
	var n int
	contract.SecretStoreSuite(t, "gcp-emulated", func(t *testing.T) ports.SecretStore {
		n++
		project := fmt.Sprintf("dop-contract-%d-%d", time.Now().UnixNano(), n)

		ctx := context.Background()
		s, err := secretstore.NewGCP(ctx, secretstore.GCPConfig{
			ProjectID: project,
			Endpoint:  endpoint,
			// Short on purpose: against the emulator the first attempt is
			// enough, and a large ceiling would turn "a mute emulator" into four
			// minutes of waiting before the error.
			Propagation: 5 * time.Second,
		})
		if err != nil {
			t.Skipf("Secret Manager unavailable at %s: %v", endpoint, err)
		}
		t.Cleanup(func() { _ = s.Close() })

		// The gRPC client connects lazily: NewGCP does not fail with the
		// emulator switched off. A real call is what discovers that — and
		// without this probe the test would die with a network error in the
		// middle of a subtest, instead of skipping with a clear message.
		//
		// THE SKIP IS NARROW ON PURPOSE: only KindUnavailable (it did not
		// answer) becomes a Skip. ANY OTHER error is a failure of the adapter
		// and has to FAIL. The first version of this probe skipped on any error,
		// and the break experiment proved the damage: breaking guarantee 2 on
		// purpose — a Get of an absent reference returning an error — made the
		// whole suite SKIP and `go test` print "ok". A test that switches itself
		// off precisely when the code breaks is worse than no test at all.
		probe := ports.SecretRef{AccountID: "probe", Kind: "integration_credential", OwnerID: "probe"}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		switch _, err := s.Get(pctx, probe); {
		case err == nil:
		case errs.KindOf(err) == errs.KindUnavailable:
			t.Skipf("the Secret Manager emulator did not answer at %s: %v\n"+
				"Bring the local environment up (dop-infra: make up) and run\n"+
				"  kubectl port-forward -n dop-local svc/secretmanager 8085:9090",
				endpoint, err)
		default:
			t.Fatalf("the adapter failed the probe with an error that is NOT an "+
				"unavailability — this is a defect, not a missing environment: %v", err)
		}
		return s
	})
}

// Why there is no equivalent test against REAL GCP in this file, and what would
// change if there were — recorded here so it is not rediscovered from scratch:
//
//   - the suite writes the SAME reference several times in a row. Real GCP
//     limits AddSecretVersion to 2 qps / 120 qpm PER SECRET and
//     DestroySecretVersion to 1 qps PER VERSION. The suite runs into that;
//   - the suite deletes and recreates the same name (subtest 3 followed by 4).
//     On real GCP DeleteSecret is irreversible and immediate, but the metadata
//     is eventually consistent: recreating right after may give AlreadyExists;
//   - subtest 1 requires immediate read-after-write. Google only promises that
//     for access BY VERSION NUMBER, and the port has nowhere to keep a number.
//     It is the architecture discovery recorded in the adapter.
//
// That is: this suite, as it stands, passes on the emulator and is NOT
// trustworthy against real GCP. Fixing that means changing the port, not the
// test.
//
// And what the suite does NOT cover, discovered by breaking guarantees on
// purpose to see what it catches:
//
//   - THAT Put DESTROYS THE PREVIOUS VALUE. Removing the destruction of the old
//     versions from the GCP adapter, the seven subtests stay GREEN: subtest 4
//     checks that Get returns the new value, and not that the old one stopped
//     being readable. On real GCP the old version would stay accessible by
//     number — and "I rotated the leaked credential" would start meaning
//     different things in each adapter. Covering that requires the port to
//     expose something it hides today, or the test to know the adapter;
//
//   - ISOLATION BY IAM. Here guarantee 5 is only exercised in the half that
//     lives in the NAME. The other half, in production, is the service account's
//     IAM policy — and the emulator has no access control at all.
