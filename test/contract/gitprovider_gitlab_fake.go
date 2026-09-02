package contract

// A local double of GitLab. See the header of gitprovider_github_fake.go for
// WHY a double exists and for the limit of what it proves.
//
// The responses here come from GitLab's API v4 documentation (docs.gitlab.com,
// the merge_requests, projects and rest/authentication sections). Where the
// documentation does not say, the comment reads `NOT DOCUMENTED`.
//
// This side is firmer than GitHub's on one point and looser on another:
//   - the text of a conflicted rebase IS documented word for word ("Rebase
//     failed. Please rebase locally") — there is no bet here;
//   - the refusal for a duplicate MR is NOT documented anywhere: neither the
//     code nor the body. The 409 and the text below come from GitLab's source,
//     not from the docs, and the adapter was designed not to depend on them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type GitLabFake struct {
	srv   *httptest.Server
	Token string

	mu  sync.Mutex
	mrs map[string][]*glFakeMR
	seq int
	log []string
}

type glFakeMR struct {
	IID      int64
	ID       int64
	Source   string
	Target   string
	Titulo   string
	Corpo    string
	SHA      string
	Base     string
	Merged   bool
	MergeSHA string
	MergedAt time.Time
	Criado   time.Time
	// Rebases left until the worker "finishes". It exists to genuinely exercise
	// guarantee 9's polling: a double that answered `rebase_in_progress:false`
	// on the first read would never make the waiting loop run, and the adapter
	// could be broken there with nobody knowing.
	RebasesPending int
	MergeError     string
}

const (
	GLProjOK           = "dop/plataforma"
	GLProjWithTrain    = "dop/with-train"
	GLProjWithoutTrain = "dop/without-train"
	GLProjNoLicence    = "dop/no-licence"
	GLProjBadScope     = "dop/escopo-insuficiente"
	GLProjInvisible    = "dop/does-not-exist"
)

func NewGitLabFake(t *testing.T, token string) *GitLabFake {
	f := &GitLabFake{Token: token, mrs: map[string][]*glFakeMR{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)
	return f
}

// URL already includes the /api/v4, like the real base.
func (f *GitLabFake) URL() string { return f.srv.URL + "/api/v4" }

func (f *GitLabFake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *GitLabFake) respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail returns GitLab's standard shape: {"message": "<text>"}. Note that
// GitLab's `message` is polymorphic — a string here, a LIST on duplication, an
// OBJECT on field validation. It is the divergence that forces the adapter to
// flatten the three shapes instead of decoding one struct.
func (f *GitLabFake) fail(w http.ResponseWriter, code int, msg any) {
	f.respond(w, code, map[string]any{"message": msg})
}

func (f *GitLabFake) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	// Both headers are documented: PRIVATE-TOKEN for a personal/project token,
	// Authorization: Bearer for OAuth. The double accepts both because both are
	// legitimate, and which one to use depends on the credential's TYPE.
	tok := r.Header.Get("PRIVATE-TOKEN")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if tok != f.Token {
		// Documented word for word: {"message": "401 Unauthorized"}.
		f.fail(w, http.StatusUnauthorized, "401 Unauthorized")
		return
	}

	// EscapedPath, and NOT Path: the project identifier travels with the slash
	// escaped ("dop%2Fplatform"), and `r.URL.Path` arrives already decoded —
	// which would turn one segment into two and make every project with a path
	// become a 404. It is the same trap as on the client side, on the other end
	// of the wire.
	p := strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4"), "/")
	seg := strings.Split(p, "/")
	if len(seg) < 2 || seg[0] != "projects" {
		f.fail(w, http.StatusNotFound, "404 Not Found")
		return
	}
	// O caminho vem escapado ("dop%2Fplataforma"); o servidor real desescapa.
	proj, err := url.PathUnescape(seg[1])
	if err != nil {
		f.fail(w, http.StatusNotFound, "404 Project Not Found")
		return
	}
	if !glKnownProject(proj) {
		// Documented: the same 404 for a nonexistent project AND for a project
		// the token cannot reach — GitLab masks the 403 in that case.
		f.fail(w, http.StatusNotFound, "404 Project Not Found")
		return
	}
	rest := seg[2:]

	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		f.readProject(w, proj)

	case len(rest) == 1 && rest[0] == "merge_requests" && r.Method == http.MethodPost:
		f.createMR(w, r, proj)

	case len(rest) == 1 && rest[0] == "merge_requests" && r.Method == http.MethodGet:
		f.listMRs(w, r, proj)

	case len(rest) == 2 && rest[0] == "merge_requests" && r.Method == http.MethodGet:
		f.readMR(w, r, proj, rest[1])

	case len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "rebase" && r.Method == http.MethodPut:
		f.rebase(w, proj, rest[1])

	case len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "merge" && r.Method == http.MethodPut:
		f.merge(w, proj, rest[1])

	default:
		f.fail(w, http.StatusNotFound, "404 Not Found")
	}
}

func glKnownProject(p string) bool {
	switch p {
	case GLProjOK, GLProjWithTrain, GLProjWithoutTrain, GLProjNoLicence, GLProjBadScope:
		return true
	}
	return false
}

// readProject reproduces the TRI-STATE of `merge_trains_enabled`, which is
// this side's whole subtlety for guarantee 14.
//
// The field is exposed by Enterprise Edition code under a licence condition
// (`if: project.feature_available?(:merge_pipelines)`): on an installation
// without the feature the KEY DOES NOT APPEAR — it does not come as `false`, it
// vanishes. A client deserializing into a `bool` would read `false` in both
// cases and never know the difference; that is why the adapter uses a
// pointer.
func (f *GitLabFake) readProject(w http.ResponseWriter, proj string) {
	if proj == GLProjBadScope {
		// Guarantee 14's "I cannot look" case. The documented shape for an
		// OAuth scope failure: {"error": "insufficient_scope", …} — which is
		// the exception to the rest of the API's {"message": …}.
		f.respond(w, http.StatusForbidden, map[string]any{
			"error":             "insufficient_scope",
			"error_description": "The request requires higher privileges than provided by the access token.",
			"scope":             "api read_api",
		})
		return
	}
	m := map[string]any{
		"id": 3, "path_with_namespace": proj, "default_branch": "main",
		"merge_method": "merge",
	}
	switch proj {
	case GLProjWithTrain:
		m["merge_pipelines_enabled"] = true
		m["merge_trains_enabled"] = true
	case GLProjNoLicence:
		// The key simply does not go out. It is the case the pointer exists to
		// see.
	default:
		m["merge_pipelines_enabled"] = false
		m["merge_trains_enabled"] = false
	}
	f.respond(w, http.StatusOK, m)
}

func (f *GitLabFake) find(proj, source, target string) *glFakeMR {
	for _, mr := range f.mrs[proj] {
		if mr.Source == source && mr.Target == target && !mr.Merged {
			return mr
		}
	}
	return nil
}

func (f *GitLabFake) createMR(w http.ResponseWriter, r *http.Request, proj string) {
	var body struct {
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Description  string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.Contains(body.SourceBranch, MarkNoCommits) {
		// Source: GitLab's code, NOT the docs. `validate_branch_existence`
		// adds the error under the :source_branch key, which is not in the list
		// of 422 keys, so it falls into the validation 400 — with `message` as
		// a field→list OBJECT. The same API's third shape of `message`.
		f.fail(w, http.StatusBadRequest, map[string]any{
			"source_branch": []string{"does not exist"},
		})
		return
	}
	if found := f.find(proj, body.SourceBranch, body.TargetBranch); found != nil {
		// NOT DOCUMENTED. It comes from the code (`conflicting_mr_message` +
		// `conflict!` → 409, body `{"message" => errors[:validate_branches]}`,
		// which is an Array). The adapter does not read this text: it reacts to
		// the status and then QUERIES the listing.
		f.fail(w, http.StatusConflict, []string{
			fmt.Sprintf("Another open merge request already exists for this source branch: !%d", found.IID),
		})
		return
	}

	f.seq++
	mr := &glFakeMR{
		IID: int64(f.seq), ID: int64(100 + f.seq),
		Source: body.SourceBranch, Target: body.TargetBranch,
		Titulo: body.Title, Corpo: body.Description,
		SHA:    fmt.Sprintf("%040x", 0xC0FFEE00+f.seq),
		Base:   fmt.Sprintf("%040x", 0xBA5E0000),
		Criado: time.Date(2026, 8, 31, 12, 0, f.seq, 0, time.UTC),
	}
	f.mrs[proj] = append(f.mrs[proj], mr)
	f.respond(w, http.StatusCreated, f.json(proj, mr, false))
}

func (f *GitLabFake) listMRs(w http.ResponseWriter, r *http.Request, proj string) {
	q := r.URL.Query()
	source, target, estado := q.Get("source_branch"), q.Get("target_branch"), q.Get("state")

	f.mu.Lock()
	defer f.mu.Unlock()
	out := []any{}
	for _, mr := range f.mrs[proj] {
		if estado == "opened" && mr.Merged {
			continue
		}
		if source != "" && mr.Source != source {
			continue
		}
		if target != "" && mr.Target != target {
			continue
		}
		out = append(out, f.json(proj, mr, false))
	}
	f.respond(w, http.StatusOK, out)
}

func (f *GitLabFake) byIID(proj, iid string) *glFakeMR {
	n, _ := strconv.ParseInt(iid, 10, 64)
	for _, mr := range f.mrs[proj] {
		if mr.IID == n {
			return mr
		}
	}
	return nil
}

func (f *GitLabFake) readMR(w http.ResponseWriter, r *http.Request, proj, iid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr := f.byIID(proj, iid)
	if mr == nil {
		f.fail(w, http.StatusNotFound, "404 Not found")
		return
	}
	// `include_rebase_in_progress` is the documented parameter: WITHOUT it the
	// field does not even appear in the response. A double that always returned
	// the field would let through an adapter that forgot the parameter — and
	// against the real GitLab that adapter would read the absence as
	// "finished".
	withRebase := r.URL.Query().Get("include_rebase_in_progress") == "true"
	if withRebase && mr.RebasesPending > 0 {
		mr.RebasesPending--
		if mr.RebasesPending == 0 && mr.MergeError == "" {
			mr.SHA = fmt.Sprintf("%040x", 0xEBA5E00+mr.IID)
		}
	}
	f.respond(w, http.StatusOK, f.json(proj, mr, withRebase))
}

// rebase reproduces the ASYNCHRONOUS route: 202 with {"rebase_in_progress":
// true}, exactly the published body.
func (f *GitLabFake) rebase(w http.ResponseWriter, proj, iid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr := f.byIID(proj, iid)
	if mr == nil {
		f.fail(w, http.StatusNotFound, "404 Not found")
		return
	}
	// Two polling rounds: the first read still finds the worker working. It is
	// what makes the adapter's waiting loop genuinely run.
	mr.RebasesPending = 2
	if strings.Contains(mr.Source, MarkConflict) {
		// DOCUMENTED word for word, including the missing full stop.
		mr.MergeError = "Rebase failed. Please rebase locally"
	}
	f.respond(w, http.StatusAccepted, map[string]any{"rebase_in_progress": true})
}

func (f *GitLabFake) merge(w http.ResponseWriter, proj, iid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mr := f.byIID(proj, iid)
	if mr == nil {
		f.fail(w, http.StatusNotFound, "404 Not found")
		return
	}
	// The SAME ambiguity as GitHub, with another number: the 405 covers already
	// merged, conflicted and blocked. Documented: "405 | 405 Method Not Allowed
	// | The merge request cannot merge" — with the numeric prefix inside the
	// text.
	if mr.Merged || strings.Contains(mr.Source, MarkConflict) || strings.Contains(mr.Source, MarkBlocked) {
		f.fail(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
		return
	}
	mr.Merged = true
	mr.MergeSHA = fmt.Sprintf("%040x", 0x0EADBEEF00+mr.IID)
	mr.MergedAt = time.Date(2026, 8, 31, 13, 0, 0, 0, time.UTC)
	f.respond(w, http.StatusOK, f.json(proj, mr, false))
}

func (f *GitLabFake) json(proj string, mr *glFakeMR, withRebase bool) map[string]any {
	estado := "opened"
	if mr.Merged {
		estado = "merged"
	}
	// `detailed_merge_status` is the current field; `merge_status` is the
	// legacy one (deprecated in 15.6) and still comes — the double sends BOTH,
	// like GitLab does, so that an adapter reading only one gets caught.
	detalhado, legado := "mergeable", "can_be_merged"
	switch {
	case mr.Merged:
		detalhado, legado = "not_open", "can_be_merged"
	case strings.Contains(mr.Source, MarkConflict):
		detalhado, legado = "conflict", "cannot_be_merged"
	case strings.Contains(mr.Source, MarkBlocked):
		detalhado, legado = "ci_still_running", "can_be_merged"
	}
	m := map[string]any{
		"id": mr.ID, "iid": mr.IID, "project_id": 3,
		"title": mr.Titulo, "description": mr.Corpo,
		"state":         estado,
		"source_branch": mr.Source, "target_branch": mr.Target,
		"web_url":               fmt.Sprintf("http://gitlab.example.com/%s/-/merge_requests/%d", proj, mr.IID),
		"sha":                   mr.SHA,
		"merge_status":          legado,
		"detailed_merge_status": detalhado,
		// Documented: has_conflicts "Returns false unless merge_status is
		// cannot_be_merged". The double honours that dependency instead of
		// filling both in independently.
		"has_conflicts": legado == "cannot_be_merged",
		"created_at":    mr.Criado.Format("2006-01-02T15:04:05.000Z"),
		"updated_at":    mr.Criado.Format("2006-01-02T15:04:05.000Z"),
		"draft":         false,
		"squash":        false,
		"diff_refs": map[string]any{
			"base_sha": mr.Base, "head_sha": mr.SHA, "start_sha": mr.Base,
		},
		"squash_commit_sha": nil,
	}
	if mr.Merged {
		m["merged_at"] = mr.MergedAt.Format("2006-01-02T15:04:05.000Z")
		// The documentation shows `merge_commit_sha: null` EVEN in the merge
		// response — an artefact of the canned example GitLab reuses across all
		// the sections. The double returns a real SHA, which is the real
		// behaviour and the only one that lets guarantee 7 be verified. The
		// divergence is recorded in the report.
		m["merge_commit_sha"] = mr.MergeSHA
	} else {
		m["merged_at"] = nil
		m["merge_commit_sha"] = nil
	}
	if withRebase {
		m["rebase_in_progress"] = mr.RebasesPending > 0
		if mr.RebasesPending == 0 && mr.MergeError != "" {
			m["merge_error"] = mr.MergeError
		} else {
			m["merge_error"] = nil
		}
	} else {
		m["merge_error"] = nil
	}
	return m
}
