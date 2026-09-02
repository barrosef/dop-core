//go:build integration

// The SAME contract suite, now against Firebase's emulated Storage.
//
//	go test ./test/contract/ -tags=integration -v
//
// The file adapter and the GCS one have implementations with nothing in common;
// it is only by putting both through the same suite that "changing adapter does
// not change the behaviour" stops being a promise and becomes a verified fact.
package contract_test

import (
	"os"
	"testing"

	"github.com/Digital-Business-One/dop-core/internal/adapter/objectstore"
	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// TWO KNOWN EMULATOR DEFECTS — subtests 8 and 13.
//
// With uploadType=media and a Content-Type of EXACTLY "application/json",
// Firebase's Storage emulator never answers: the connection hangs until the
// client's timeout. Reproducible outside the test, with curl and the same body:
//
//	application/json          → hangs (no response)
//	application/json; charset=utf-8 → 400
//	text/plain, text/json, application/xml, application/octet-stream → 200
//
// Real GCS accepts and stores it normally. It is a difference of the emulator,
// not of the adapter — proven by `fs`, which passes all 13 subtests.
//
// A practical consequence: storing JSON in the object store HANGS in the local
// environment. Until the emulator fixes it, whoever writes JSON should use a
// type it accepts. Documented in dop-infra/docs/ambiente-local.md; the Makefile's
// `test-contract-integration` target excludes this subtest, with the reason in
// plain sight — instead of leaving it red forever and everyone learning to
// ignore the suite.
func TestObjectStoreContractGCS(t *testing.T) {
	host := os.Getenv("STORAGE_EMULATOR_HOST")
	if host == "" {
		host = "127.0.0.1:9199"
	}
	bucket := os.Getenv("STORAGE_BUCKET")
	if bucket == "" {
		bucket = "dop-local.firebasestorage.app"
	}
	contract.ObjectStoreSuite(t, "gcs-emulated", func(t *testing.T) (ports.ObjectStore, []string) {
		// A single bucket: the emulator does not provision a second, and
		// inventing one would make the isolation case fail for the wrong
		// reason.
		return objectstore.NewGCS(objectstore.GCSConfig{Endpoint: host}), []string{bucket}
	})
}
