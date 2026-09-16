package projectrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// Remote is the adapter for a server that runs in ANOTHER process: the same
// protocol, over HTTP, with the admin key. It is what the core uses the day the
// git server is split out of `serve` — and it is the second adapter ADR-0001
// asks for, proven by the same contract suite as Local.
//
// OnPush here is a no-op with a reason: pushes reach a remote process through
// the event bus, not through a callback across the network. The server's own
// OnPush, where it runs, is what feeds the bus.
type Remote struct {
	base     string // the server's address as seen from THIS process
	adminKey string
	client   *http.Client
}

func NewRemote(serverURL, prefix, adminKey string) *Remote {
	p := strings.TrimRight(prefix, "/")
	if p == "" {
		p = "/git"
	}
	return &Remote{
		base:     strings.TrimRight(serverURL, "/") + p + "/api/projects/",
		adminKey: adminKey,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (r *Remote) call(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return errs.Wrap(errs.KindInternal, err, "request unreadable for the git server")
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.adminKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to talk to the git server")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errs.NotFound("file")
	case resp.StatusCode == http.StatusBadRequest:
		return errs.Invalid("%s", strings.TrimSpace(string(raw)))
	case resp.StatusCode >= 300:
		return errs.New(errs.KindUnavailable, "git server answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		switch o := out.(type) {
		case *[]byte:
			*o = raw
		default:
			return json.Unmarshal(raw, out)
		}
	}
	return nil
}

func (r *Remote) Ensure(ctx context.Context, projectID string) (ports.RepositoryInfo, error) {
	var out struct {
		CloneURL string `json:"clone_url"`
	}
	if err := r.call(ctx, http.MethodPost, projectID+"/ensure", nil, &out); err != nil {
		return ports.RepositoryInfo{}, err
	}
	return ports.RepositoryInfo{ProjectID: projectID, CloneURL: out.CloneURL}, nil
}

func (r *Remote) IssueToken(ctx context.Context, projectID, demandID string, ttl time.Duration) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	in := map[string]any{"demand_id": demandID, "ttl_seconds": int(ttl.Seconds())}
	if err := r.call(ctx, http.MethodPost, projectID+"/token", in, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

func (r *Remote) Commit(ctx context.Context, projectID string, c ports.RepositoryCommit) (string, error) {
	var out struct {
		Commit string `json:"commit"`
	}
	if err := r.call(ctx, http.MethodPost, projectID+"/commit", c, &out); err != nil {
		return "", err
	}
	return out.Commit, nil
}

func (r *Remote) Read(ctx context.Context, projectID, path string) ([]byte, error) {
	var out []byte
	if err := r.call(ctx, http.MethodGet, projectID+"/files/"+path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Remote) SetMirror(ctx context.Context, projectID, remoteURL string, credential ports.SecretRef) error {
	in := map[string]any{"remote_url": remoteURL, "credential": credential}
	return r.call(ctx, http.MethodPut, projectID+"/mirror", in, nil)
}

func (r *Remote) OnPush(func(ctx context.Context, p ports.Push)) {}

var _ ports.ProjectRepository = (*Remote)(nil)
var _ ports.ProjectRepository = (*Local)(nil)
