package contract_test

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/projectrepo"
	"github.com/Digital-Business-One/dop-core/internal/adapter/secretstore"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// The Local adapter hosts the repositories in-process; the suite reaches the
// smart-HTTP side through a listener on the loopback.
func TestProjectRepositoryContractLocal(t *testing.T) {
	contract.ProjectRepositorySuite(t, "local", func(t *testing.T) contract.ProjectRepositoryEnv {
		secrets := secretstore.NewMemory()
		srv, err := projectrepo.NewServer(projectrepo.Config{
			Root: t.TempDir(), Key: []byte("contract-suite-key-0123456789abcdef"), Prefix: "/git",
		})
		if err != nil {
			t.Fatal(err)
		}
		srv.WithSecrets(secrets)
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.Handle("/git/", srv)
		hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = hs.Serve(lis) }()
		t.Cleanup(func() { _ = hs.Close() })
		return contract.ProjectRepositoryEnv{
			Repos:     projectrepo.NewLocal(srv, "http://"+lis.Addr().String()),
			Secrets:   secrets,
			AccountID: "contract-account",
		}
	})
}
