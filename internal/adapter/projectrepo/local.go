package projectrepo

import (
	"context"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
)

// Local is the in-process adapter: the server runs inside this process and the
// port's verbs call it directly. It is the laptop's adapter and the one the
// `serve` mode uses when it hosts the repositories itself.
//
// BaseURL is what a SANDBOX can reach — `http://dop-core.dop-local.svc:9091`
// inside the cluster, `http://host.docker.internal:9091` from a container on
// the laptop. It is a fact about the network, not about the repositories,
// which is why it is configuration and not derived.
type Local struct {
	srv     *Server
	baseURL string
}

func NewLocal(srv *Server, baseURL string) *Local {
	return &Local{srv: srv, baseURL: strings.TrimRight(baseURL, "/")}
}

func (l *Local) Ensure(ctx context.Context, projectID string) (ports.RepositoryInfo, error) {
	if err := l.srv.ensure(ctx, projectID); err != nil {
		return ports.RepositoryInfo{}, err
	}
	return ports.RepositoryInfo{
		ProjectID: projectID,
		CloneURL:  l.baseURL + l.srv.prefix + "/" + projectID + ".git",
	}, nil
}

func (l *Local) IssueToken(_ context.Context, projectID, demandID string, ttl time.Duration) (string, error) {
	return l.srv.mint(projectID, demandID, ttl)
}

func (l *Local) Commit(ctx context.Context, projectID string, c ports.RepositoryCommit) (string, error) {
	if err := l.srv.ensure(ctx, projectID); err != nil {
		return "", err
	}
	return l.srv.commit(ctx, projectID, c)
}

func (l *Local) Read(ctx context.Context, projectID, path string) ([]byte, error) {
	return l.srv.read(ctx, projectID, path)
}

func (l *Local) SetMirror(_ context.Context, projectID, remoteURL string, credential ports.SecretRef) error {
	l.srv.mirrors.Store(projectID, mirror{url: remoteURL, credential: credential})
	return nil
}

func (l *Local) OnPush(fn func(ctx context.Context, p ports.Push)) { l.srv.onPush = fn }
