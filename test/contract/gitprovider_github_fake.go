package contract

// A local double of GitHub.
//
// ── Why it exists, and where its limit is ────────────────────────────────────
//
// We have no real GitHub in CI or on the laptop of whoever touches the adapter.
// With no double, the GitProvider's contract suite would be a file nobody runs —
// and this session's lesson is that a suite that does not run verifies nothing
// (SecretStore's k8s adapter spent months never having been exercised, and the
// in-memory double only proved it was consistent with itself).
//
// The double's RISK is the opposite and it is worse: a double written from what
// MY adapter expects proves nothing — it confirms my own assumptions and the
// first contact with the real provider contradicts everything. That is why the
// responses here come from GitHub's DOCUMENTATION (the published OpenAPI
// description and the published examples), with the names and the codes they
// publish. Where the documentation does NOT say, the comment reads `NOT
// DOCUMENTED` — and the adapter was designed not to depend on those points.
//
// What this double does NOT prove, and nothing here can pretend it does:
//   - that the undocumented message of the 422 for a duplicate PR is that one;
//   - that the undocumented message of the GraphQL error on a conflicted rebase
//     is that one;
//   - real latency, pagination, rate limits, and `mergeable`'s asynchronous
//     behaviour (here it is born already computed).
// That is what the path under the `integration` tag, against a real GitHub, is
// for.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHubFake is the server. It keeps real state (open and merged PRs) because
// guarantees 3 and 6's idempotency is about STATE: a double with no memory would
// always answer the same thing and would prove neither.
type GitHubFake struct {
	srv   *httptest.Server
	Token string

	mu   sync.Mutex
	prs  map[string][]*ghFakePR // repo → PRs
	seq  int
	log  []string
	subs int
}

type ghFakePR struct {
	Number   int
	NodeID   string
	Source   string
	Target   string
	Titulo   string
	Corpo    string
	Head     string
	Base     string
	Merged   bool
	MergeSHA string
	MergedAt time.Time
	Criado   time.Time
}

// The repositories this double knows. Any other name gives a 404 — which is
// exactly what GitHub does, including for a private repository the token cannot
// see (the documentation is explicit: a 404 instead of a 403 "to avoid
// confirming the existence of private repositories").
const (
	GHRepoOK             = "dop/plataforma"
	GHRepoWithQueue      = "dop/with-queue"
	GHRepoWithoutQueue   = "dop/without-queue"
	GHRepoQueueForbidden = "dop/queue-forbidden"
	GHRepoInvisible      = "dop/does-not-exist"
)

// Markers in the BRANCH NAME choose the scenario. It is the cheapest way of
// giving the suite a conflict on demand without inventing a configuration API
// the real provider would not have.
const (
	MarkConflict  = "conflict"
	MarkBlocked   = "blocked"
	MarkNoCommits = "no-commits"
)

func NewGitHubFake(t *testing.T, token string) *GitHubFake {
	f := &GitHubFake{Token: token, prs: map[string][]*ghFakePR{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *GitHubFake) URL() string { return f.srv.URL }

// GraphQLURL is separate on purpose: on GitHub Enterprise REST lives at
// /api/v3 and GraphQL at /api/graphql, and an adapter that deduced one from the
// other would break there.
// would work against github.com and break on every self-hosted installation.
func (f *GitHubFake) GraphQLURL() string { return f.srv.URL + "/graphql" }

// Calls returns the request log, for the tests that need to assert about what
// was NOT sent (see guarantee 4).
func (f *GitHubFake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *GitHubFake) respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// basicError is GitHub's OpenAPI `Basic Error` schema: message,
// documentation_url, url and status — ALL optional.
func (f *GitHubFake) basicError(w http.ResponseWriter, code int, msg string) {
	f.respond(w, code, map[string]any{
		"message":           msg,
		"documentation_url": "https://docs.github.com/rest",
	})
}

func (f *GitHubFake) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	// The User-Agent is one of the few headers GitHub documents as genuinely
	// mandatory: "Requests without a valid User-Agent header will be rejected…
	// you will receive a 403 Forbidden response".
	if r.Header.Get("User-Agent") == "" {
		f.basicError(w, http.StatusForbidden, "Request forbidden by administrative rules. Please make sure your request has a User-Agent header.")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		// NOT DOCUMENTED: the text "Bad credentials" appears in no example
		// published by GitHub. What IS documented is the 401 STATUS for an
		// invalid credential and the body's Basic Error schema. The adapter
		// classifies by the status, never by this text.
		f.basicError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	p := strings.Trim(r.URL.Path, "/")
	if p == "graphql" {
		f.graphql(w, r)
		return
	}
	seg := strings.Split(p, "/")
	// /repos/{owner}/{name}/...
	if len(seg) < 3 || seg[0] != "repos" {
		f.basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := seg[1] + "/" + seg[2]
	if !ghRepoConhecido(repo) {
		f.basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	resto := seg[3:]

	switch {
	case len(resto) == 0 && r.Method == http.MethodGet:
		// Only the field the adapter uses; the real object is enormous and
		// copying it whole would add no verification at all.
		f.respond(w, http.StatusOK, map[string]any{
			"full_name": repo, "default_branch": "main", "private": false,
		})

	case len(resto) == 3 && resto[0] == "rules" && resto[1] == "branches":
		f.branchRules(w, repo)

	case len(resto) == 1 && resto[0] == "pulls" && r.Method == http.MethodPost:
		f.criarPR(w, r, repo)

	case len(resto) == 1 && resto[0] == "pulls" && r.Method == http.MethodGet:
		f.listarPRs(w, r, repo)

	case len(resto) == 2 && resto[0] == "pulls" && r.Method == http.MethodGet:
		f.lerPR(w, repo, resto[1])

	case len(resto) == 3 && resto[0] == "pulls" && resto[2] == "merge" && r.Method == http.MethodPut:
		f.mergearPR(w, repo, resto[1])

	default:
		// GitHub documents that an unsupported method returns a 404, and NOT a 405.
		f.basicError(w, http.StatusNotFound, "Not Found")
	}
}

func ghRepoConhecido(r string) bool {
	switch r {
	case GHRepoOK, GHRepoWithQueue, GHRepoWithoutQueue, GHRepoQueueForbidden:
		return true
	}
	return false
}

// branchRules reproduces GET /repos/{o}/{r}/rules/branches/{branch}: an ARRAY of
// rules, each with `type`, `ruleset_source_type`, `ruleset_source`, `ruleset_id`
// and `parameters` — the shape published in the documentation's example.
func (f *GitHubFake) branchRules(w http.ResponseWriter, repo string) {
	if repo == GHRepoQueueForbidden {
		// Guarantee 14's case: the adapter CANNOT look. It has to become an
		// error, never a `false` of convenience.
		f.basicError(w, http.StatusForbidden,
			"Resource not accessible by personal access token")
		return
	}
	if repo != GHRepoWithQueue {
		// A rule of another type, and not an empty list: that way the test
		// proves the adapter looks for `merge_queue`, and not that it counts
		// elements.
		f.respond(w, http.StatusOK, []any{
			map[string]any{
				"type": "required_linear_history", "ruleset_source_type": "Repository",
				"ruleset_source": repo, "ruleset_id": 42,
			},
		})
		return
	}
	// `merge_queue`'s seven parameters are all mandatory when `parameters` is
	// present, and the enums are UPPERCASE here (ALLGREEN, MERGE) — unlike the
	// lowercase enum of the merge endpoint. The inconsistency is GitHub's and is
	// copied on purpose.
	f.respond(w, http.StatusOK, []any{
		map[string]any{
			"type": "merge_queue", "ruleset_source_type": "Organization",
			"ruleset_source": "dop", "ruleset_id": 73,
			"parameters": map[string]any{
				"check_response_timeout_minutes":    60,
				"grouping_strategy":                 "ALLGREEN",
				"max_entries_to_build":              5,
				"max_entries_to_merge":              5,
				"merge_method":                      "MERGE",
				"min_entries_to_merge":              1,
				"min_entries_to_merge_wait_minutes": 5,
			},
		},
	})
}

func (f *GitHubFake) find(repo, source, target string) *ghFakePR {
	for _, pr := range f.prs[repo] {
		if pr.Source == source && pr.Target == target && !pr.Merged {
			return pr
		}
	}
	return nil
}

func (f *GitHubFake) criarPR(w http.ResponseWriter, r *http.Request, repo string) {
	var body struct {
		Head, Base, Title, Body string
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.Contains(body.Head, MarkNoCommits) {
		// A 422 with the Validation Error schema. NOT DOCUMENTED: GitHub
		// publishes only the schema, with no `errors[]` example for this case.
		// The text below is plausible, it is not a promise — and the adapter does
		// NOT read it: faced with any 422 it goes looking for the open PR, and it
		// is that PR's ABSENCE that turns the 422 into an error.
		f.respond(w, http.StatusUnprocessableEntity, map[string]any{
			"message":           "Validation Failed",
			"documentation_url": "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
			"errors": []any{map[string]any{
				"resource": "PullRequest", "code": "custom", "field": "base",
				"message": fmt.Sprintf("No commits between %s and %s", body.Base, body.Head),
			}},
		})
		return
	}
	if found := f.find(repo, body.Head, body.Base); found != nil {
		// NOT DOCUMENTED: neither the `code` nor the text. GitHub has never
		// published an example of this response — only the 422's schema. The
		// adapter was designed NOT to depend on it: it reacts to the STATUS and
		// then queries the listing, which is a fact, not a string.
		f.respond(w, http.StatusUnprocessableEntity, map[string]any{
			"message":           "Validation Failed",
			"documentation_url": "https://docs.github.com/rest/pulls/pulls#create-a-pull-request",
			"errors": []any{map[string]any{
				"resource": "PullRequest", "code": "custom",
				"message": fmt.Sprintf("A pull request already exists for dop:%s.", body.Head),
			}},
		})
		return
	}

	f.seq++
	f.subs++
	pr := &ghFakePR{
		Number: 1300 + f.seq, NodeID: fmt.Sprintf("PR_kwDO%08d", f.seq),
		Source: body.Head, Target: body.Base, Titulo: body.Title, Corpo: body.Body,
		Head:   fmt.Sprintf("%040x", 0xC0FFEE00+f.seq),
		Base:   fmt.Sprintf("%040x", 0xBA5E0000),
		Criado: time.Date(2026, 8, 31, 12, 0, f.seq, 0, time.UTC),
	}
	f.prs[repo] = append(f.prs[repo], pr)
	f.respond(w, http.StatusCreated, f.json(repo, pr))
}

func (f *GitHubFake) listarPRs(w http.ResponseWriter, r *http.Request, repo string) {
	q := r.URL.Query()
	head, base := q.Get("head"), q.Get("base")
	// The documentation only describes the format WITH the prefix
	// ("user:ref-name" or "organization:ref-name") — so the double REQUIRES the
	// prefix. Accepting it without one would let through an adapter that only
	// worked here.
	if head != "" && !strings.Contains(head, ":") {
		f.respond(w, http.StatusUnprocessableEntity, map[string]any{
			"message":           "Validation Failed",
			"documentation_url": "https://docs.github.com/rest/pulls/pulls#list-pull-requests",
			"errors": []any{map[string]any{
				"resource": "PullRequest", "field": "head", "code": "invalid",
				"message": "head must be in the format user:ref-name or organization:ref-name",
			}},
		})
		return
	}
	if i := strings.Index(head, ":"); i >= 0 {
		head = head[i+1:]
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	out := []any{}
	for _, pr := range f.prs[repo] {
		if pr.Merged && q.Get("state") == "open" {
			continue
		}
		if head != "" && pr.Source != head {
			continue
		}
		if base != "" && pr.Target != base {
			continue
		}
		out = append(out, f.json(repo, pr))
	}
	f.respond(w, http.StatusOK, out)
}

func (f *GitHubFake) lerPR(w http.ResponseWriter, repo, num string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.porNumero(repo, num)
	if pr == nil {
		f.basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	f.respond(w, http.StatusOK, f.json(repo, pr))
}

func (f *GitHubFake) porNumero(repo, num string) *ghFakePR {
	n, _ := strconv.Atoi(num)
	for _, pr := range f.prs[repo] {
		if pr.Number == n {
			return pr
		}
	}
	return nil
}

func (f *GitHubFake) mergearPR(w http.ResponseWriter, repo, num string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.porNumero(repo, num)
	if pr == nil {
		f.basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	// HERE IS THE AMBIGUITY THE PORT HAS TO UNDO: GitHub answers exactly the
	// same thing — a 405, with the same phrase — for an already merged PR, a
	// conflicted PR and a PR blocked by a check. The published body does NOT
	// carry `documentation_url`, and the double copies that absence.
	if pr.Merged || strings.Contains(pr.Source, MarkConflict) || strings.Contains(pr.Source, MarkBlocked) {
		f.respond(w, http.StatusMethodNotAllowed, map[string]any{
			"message": "Pull Request is not mergeable",
		})
		return
	}
	pr.Merged = true
	pr.MergeSHA = fmt.Sprintf("%040x", 0x0EADBEEF00+pr.Number)
	pr.MergedAt = time.Date(2026, 8, 31, 13, 0, 0, 0, time.UTC)
	f.respond(w, http.StatusOK, map[string]any{
		"sha": pr.MergeSHA, "merged": true, "message": "Pull Request successfully merged",
	})
}

// json builds the PR object with the published example's names and shape.
func (f *GitHubFake) json(repo string, pr *ghFakePR) map[string]any {
	estado := "open"
	if pr.Merged {
		estado = "closed"
	}
	// `mergeable` is NULLABLE in the schema (null while GitHub computes it).
	// Here it is born computed — it is a SIMPLIFICATION of the double, and that
	// is why the adapter's "still computing" path is only exercised against the
	// real provider. It is recorded in the report.
	mergeable := !strings.Contains(pr.Source, MarkConflict)
	estadoMerge := "clean"
	switch {
	case strings.Contains(pr.Source, MarkConflict):
		estadoMerge = "dirty"
	case strings.Contains(pr.Source, MarkBlocked):
		estadoMerge = "blocked"
	}
	m := map[string]any{
		"url":        fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repo, pr.Number),
		"id":         pr.Number,
		"node_id":    pr.NodeID,
		"html_url":   fmt.Sprintf("https://github.com/%s/pull/%d", repo, pr.Number),
		"number":     pr.Number,
		"state":      estado,
		"locked":     false,
		"title":      pr.Titulo,
		"body":       pr.Corpo,
		"created_at": pr.Criado.Format(time.RFC3339),
		"updated_at": pr.Criado.Format(time.RFC3339),
		"head": map[string]any{
			"label": "dop:" + pr.Source, "ref": pr.Source, "sha": pr.Head,
		},
		"base": map[string]any{
			"label": "dop:" + pr.Target, "ref": pr.Target, "sha": pr.Base,
		},
		"draft":           false,
		"merged":          pr.Merged,
		"mergeable":       mergeable,
		"rebaseable":      mergeable,
		"mergeable_state": estadoMerge,
	}
	if pr.Merged {
		m["merged_at"] = pr.MergedAt.Format(time.RFC3339)
		m["merge_commit_sha"] = pr.MergeSHA
	} else {
		m["merged_at"] = nil
		m["merge_commit_sha"] = nil
	}
	return m
}

// graphql serves the `updatePullRequestBranch` mutation.
//
// This stretch's ENTIRE point: GitHub's GraphQL answers HTTP 200 even when the
// mutation FAILS — the failure comes in the body's `errors` array. An adapter
// that looked only at the status would report "rebase done" for every conflict,
// and ADR-0005's queue would merge onto a branch that was not reapplied. The
// double reproduces that trap on purpose.
func (f *GitHubFake) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !strings.Contains(req.Query, "updatePullRequestBranch") {
		f.respond(w, http.StatusOK, map[string]any{
			"errors": []any{map[string]any{"message": "unsupported query in this double"}},
		})
		return
	}
	// The mutation takes the NODE ID, not the number — it is the difference that
	// forces the adapter to locate the PR first.
	node, _ := req.Variables["pr"].(string)

	f.mu.Lock()
	defer f.mu.Unlock()
	var target *ghFakePR
	for _, prs := range f.prs {
		for _, pr := range prs {
			if pr.NodeID == node {
				target = pr
			}
		}
	}
	if target == nil {
		f.respond(w, http.StatusOK, map[string]any{
			"errors": []any{map[string]any{
				"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of '" + node + "'.",
			}},
		})
		return
	}
	if strings.Contains(target.Source, MarkConflict) {
		// NOT DOCUMENTED: GitHub publishes neither the text of a conflicted
		// rebase's error nor a structured field saying "conflict". This text is
		// a reasonable BET, and it is exactly why the adapter classifies by
		// marker and falls on the ERROR side when it does not recognize
		// something — erring towards conflict would send a human to solve a
		// permission problem in the attention box.
		f.respond(w, http.StatusOK, map[string]any{
			"data": map[string]any{"updatePullRequestBranch": nil},
			"errors": []any{map[string]any{
				"type":    "UNPROCESSABLE",
				"message": "merge conflict between base and head",
			}},
		})
		return
	}
	// The rebase is done: the head changes. It is what the queue re-verifies in
	// the next position.
	target.Head = fmt.Sprintf("%040x", 0xEBA5E00+target.Number)
	f.respond(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"updatePullRequestBranch": map[string]any{
				"pullRequest": map[string]any{"id": target.NodeID},
			},
		},
	})
}
