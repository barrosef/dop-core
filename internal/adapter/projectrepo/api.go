package projectrepo

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The platform-side API: what the Remote adapter speaks when the server runs
// in another process. It is NOT what a sandbox uses — a sandbox only ever
// speaks git over `<prefix>/<project>.git`, with its per-demand token. This API
// is guarded by the ADMIN key, which no sandbox holds.
//
// Endpoints, all under `<prefix>/api/projects/<id>`:
//
//	POST   /ensure          → {clone_url}
//	POST   /token           {demand_id, ttl_seconds} → {token}
//	POST   /commit          RepositoryCommit → {commit}
//	GET    /files/<path>    → the bytes (404 when absent)
//	PUT    /mirror          {remote_url, credential} → 204
//
// The push fan-out (guarantee 7) does not cross this API: a remote process
// learns about pushes the way everything else does — through the event bus,
// which the server's own OnPush feeds where it runs.

type API struct {
	srv      *Server
	adminKey string
	baseURL  string
}

func NewAPI(srv *Server, adminKey, baseURL string) *API {
	return &API{srv: srv, adminKey: adminKey, baseURL: strings.TrimRight(baseURL, "/")}
}

func (a *API) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return a.adminKey != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.adminKey)) == 1
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, a.srv.prefix+"/api/projects/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	projectID, op := parts[0], parts[1]
	ctx := r.Context()
	switch {
	case op == "ensure" && r.Method == http.MethodPost:
		if err := a.srv.ensure(ctx, projectID); err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"clone_url": a.baseURL + a.srv.prefix + "/" + projectID + ".git"})
	case op == "token" && r.Method == http.MethodPost:
		var in struct {
			DemandID   string `json:"demand_id"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		tok, err := a.srv.mint(projectID, in.DemandID, time.Duration(in.TTLSeconds)*time.Second)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"token": tok})
	case op == "commit" && r.Method == http.MethodPost:
		var c ports.RepositoryCommit
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := a.srv.ensure(ctx, projectID); err != nil {
			fail(w, err)
			return
		}
		sha, err := a.srv.commit(ctx, projectID, c)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, map[string]string{"commit": sha})
	case strings.HasPrefix(op, "files/") && r.Method == http.MethodGet:
		out, err := a.srv.read(ctx, projectID, strings.TrimPrefix(op, "files/"))
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(out)
	case op == "mirror" && r.Method == http.MethodPut:
		var in struct {
			RemoteURL  string          `json:"remote_url"`
			Credential ports.SecretRef `json:"credential"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		a.srv.mirrors.Store(projectID, mirror{url: in.RemoteURL, credential: in.Credential})
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	switch errs.KindOf(err) {
	case errs.KindNotFound:
		http.Error(w, err.Error(), http.StatusNotFound)
	case errs.KindInvalid:
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}

var _ context.Context
