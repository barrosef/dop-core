package contract_test

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/adapter/projectrepo"
	"github.com/Digital-Business-One/dop-core/test/contract"
)

// The Remote adapter speaks to a server in another process over the
// platform-side API. Here the "other process" is a server raised by the test:
// the adapter under test never touches it directly, only over HTTP.
func TestProjectRepositoryContractRemote(t *testing.T) {
	contract.ProjectRepositorySuite(t, "remote", func(t *testing.T) contract.ProjectRepositoryEnv {
		srv, err := projectrepo.NewServer(projectrepo.Config{
			Root: t.TempDir(), Key: []byte("contract-suite-key-0123456789abcdef"), Prefix: "/git",
		})
		if err != nil {
			t.Fatal(err)
		}
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		base := "http://" + lis.Addr().String()
		mux := http.NewServeMux()
		mux.Handle("/git/api/", projectrepo.NewAPI(srv, "admin-key", base))
		mux.Handle("/git/", srv)
		hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = hs.Serve(lis) }()
		t.Cleanup(func() { _ = hs.Close() })
		// No Secrets: this adapter does not host, so guarantees 7 and 8 are
		// proven on the hosting side, and the suite skips them here.
		return contract.ProjectRepositoryEnv{Repos: projectrepo.NewRemote(base, "/git", "admin-key")}
	})
}
