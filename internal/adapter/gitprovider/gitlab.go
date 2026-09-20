// A delivery.GitProvider adapter over GitLab (REST API v4).
//
// ── What is different from GitHub, and why it matters ────────────────────────
//
// The vocabulary diverges in almost everything, and each divergence cost a
// decision:
//
//   - PR × MR, `number` × `iid` (the iid is per PROJECT, not global — the global
//     `id` exists and does NOT work in the MR routes);
//   - a PR already exists for the branch: 422 on GitHub, 409 on GitLab, with
//     bodies in different formats — and GitLab's `message` is sometimes a
//     string, sometimes a LIST of strings, sometimes an OBJECT of field→list;
//   - conflict: `mergeable:false` on GitHub, `detailed_merge_status:"conflict"`
//     on GitLab (with a legacy `merge_status` that still comes along);
//   - rebase: GitHub has it ONLY through GraphQL; GitLab has a REST route of its
//     own, but an ASYNCHRONOUS one, whose result is discovered by polling;
//   - native queue: GitHub's merge queue is PER BRANCH and comes from a ruleset;
//     GitLab's merge train is PER PROJECT and comes from a project field — which
//     disappears from the response when the plan does not include the feature.
//
// ── Where GitLab documents and where it does not ─────────────────────────────
//
// The refusal for a conflicted rebase IS documented, word for word ("Rebase
// failed. Please rebase locally"), which makes this side firmer than GitHub's.
// The refusal for a duplicate MR, on the other hand, is NOT documented anywhere
// — and that is why, here too, the adapter does not match text: it QUERIES.
package gitprovider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/delivery"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

type GitLab struct {
	rest          *client
	actorID       string
	squash        bool
	rebaseTimeout time.Duration
	poll          time.Duration
}

type GitLabConfig struct {
	// APIBase includes the /api/v4: "https://gitlab.com/api/v4" or
	// "https://gitlab.internal/api/v4".
	APIBase string
	// Token is the ALREADY RESOLVED value of the resource credential (ADR-0009).
	Token   string
	ActorID string
	// MergeMethod is the git flow's vocabulary (ADR-0009). GitLab does not
	// accept the method on the merge call — it is a PROJECT setting — but it
	// does accept `squash`, which is the only translatable piece. See the
	// conversion below.
	MergeMethod   string
	Timeout       time.Duration
	RebaseTimeout time.Duration
	Poll          time.Duration
	Client        httpDoer
	// UseBearer swaps the PRIVATE-TOKEN header (a personal/project token) for
	// Authorization: Bearer (an OAuth token). Both are documented; which one to
	// use depends on the credential's TYPE, and only whoever stored it knows
	// that.
	UseBearer bool
}

func NewGitLab(cfg GitLabConfig) *GitLab {
	base := cfg.APIBase
	if base == "" {
		base = "https://gitlab.com/api/v4"
	}
	authorize := func(r *http.Request) {
		if cfg.Token == "" {
			return
		}
		if cfg.UseBearer {
			r.Header.Set("Authorization", "Bearer "+cfg.Token)
			return
		}
		r.Header.Set("PRIVATE-TOKEN", cfg.Token)
	}
	rt := cfg.RebaseTimeout
	if rt <= 0 {
		rt = DefaultRebaseTimeout
	}
	p := cfg.Poll
	if p <= 0 {
		p = defaultPoll
	}
	return &GitLab{
		rest:    newClient(base, "GitLab", cfg.Client, cfg.Timeout, authorize, cfg.Token),
		actorID: cfg.ActorID,
		// An HONEST and incomplete translation of the git flow's vocabulary: in
		// GitLab the merge method is a PROJECT setting (merge / rebase_merge /
		// ff), not a call parameter. The only piece the call accepts is
		// `squash`. Forcing the three values here would be faking a control the
		// API does not give.
		squash:        cfg.MergeMethod == "squash",
		rebaseTimeout: rt,
		poll:          p,
	}
}

var _ delivery.GitProvider = (*GitLab)(nil)

func (g GitLab) String() string { return "gitprovider.GitLab{actor=" + g.actorID + "}" }

// ── GitLab's shapes (only what the port uses) ────────────────────────────────

type glMR struct {
	ID              int64  `json:"id"`
	IID             int64  `json:"iid"`
	Title           string `json:"title"`
	State           string `json:"state"` // opened | closed | locked | merged
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	WebURL          string `json:"web_url"`
	SHA             string `json:"sha"`
	MergeCommitSHA  string `json:"merge_commit_sha"`
	SquashCommitSHA string `json:"squash_commit_sha"`
	// MergeStatus is the LEGACY field (deprecated in 15.6) and
	// DetailedMergeStatus is its replacement. We read both: older self-hosted
	// installations — which are the cluster adapter's use case — do not have the
	// detailed one yet.
	MergeStatus         string  `json:"merge_status"`
	DetailedMergeStatus string  `json:"detailed_merge_status"`
	HasConflicts        bool    `json:"has_conflicts"`
	MergeError          *string `json:"merge_error"`
	RebaseInProgress    bool    `json:"rebase_in_progress"`
	CreatedAt           string  `json:"created_at"`
	MergedAt            *string `json:"merged_at"`
	DiffRefs            struct {
		BaseSHA string `json:"base_sha"`
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
}

// Whoami answers with the username the token belongs to — see GitHub's.
func (g *GitLab) Whoami(ctx context.Context) (string, error) {
	code, body, err := g.rest.do(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return "", err
	}
	if code >= 300 {
		return "", g.rest.fail(code, explainGL(body), "reading the token's user")
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := g.rest.decode(body, &me, "reading the token's user"); err != nil {
		return "", err
	}
	return me.Username, nil
}

// explainGL flattens GitLab's error body.
//
// Three different formats in the same API, all seen in production:
//
//	{"message":"404 Project Not Found"}                       // string
//	{"message":["Another open merge request already exists…"]} // lista
//	{"message":{"source_branch":["does not exist"]}}           // objeto
//	{"error":"insufficient_scope"}                             // OAuth
func explainGL(body []byte) string {
	var e struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	_ = tolerantUnmarshal(body, &e)
	if s := strings.TrimSpace(textOf(e.Message)); s != "" {
		return s
	}
	if s := strings.TrimSpace(e.Error); s != "" {
		return s
	}
	return strings.TrimSpace(string(body))
}

// project encodes the identifier for the route. GitLab accepts the numeric id
// OR the path, and the path has to come with the slash escaped
// ("group%2Fproject") — without that the route becomes another route and the
// answer is a 404 that looks like "it does not exist" when the problem is the
// encoding.
func project(external string) (string, error) {
	e := strings.Trim(strings.TrimSpace(external), "/")
	if e == "" {
		return "", errs.Invalid("empty GitLab project identifier")
	}
	return url.PathEscape(e), nil
}

func (g *GitLab) mrToPort(mr glMR) delivery.ProviderPR {
	return delivery.ProviderPR{
		// ExternalID is OPAQUE (guarantee 10): here it is the `iid`, which is
		// per project. GitLab's global `id` does NOT work in the MR routes —
		// using the wrong field gives a 404 on an MR that exists.
		ExternalID:   strconv.FormatInt(mr.IID, 10),
		URL:          mr.WebURL,
		HeadCommit:   mr.SHA,
		TargetBranch: mr.TargetBranch,
		CreatedAt:    instant(mr.CreatedAt),
	}
}

// ── OpenPullRequest ──────────────────────────────────────────────────────────

// OpenPullRequest opens the MR, and it is IDEMPOTENT by (project, source,
// target).
//
// The same strategy as GitHub's and for the same reason: the refusal for a
// duplicate MR is NOT documented — neither the code nor the text. So the adapter
// does not match strings: faced with a 409 or a 422, it LOOKS for the open MR of
// that pair of branches. Found it, it was a duplicate; did not, it was something
// else and it becomes an error with the text alongside.
func (g *GitLab) OpenPullRequest(ctx context.Context, spec delivery.OpenPRSpec) (delivery.ProviderPR, error) {
	if err := checkActor(g.actorID, spec.ActorID, "opening the MR"); err != nil {
		return delivery.ProviderPR{}, err
	}
	proj, err := project(spec.RepoExternalID)
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	target := spec.TargetBranch
	if target == "" {
		target = "main"
	}

	code, body, err := g.rest.do(ctx, http.MethodPost, "/projects/"+proj+"/merge_requests",
		map[string]any{
			"source_branch": spec.SourceBranch,
			"target_branch": target,
			"title":         spec.Title,
			"description":   spec.Body,
		})
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	switch {
	case code == http.StatusCreated || code == http.StatusOK:
		var mr glMR
		if err := g.rest.decode(body, &mr, "opening an MR"); err != nil {
			return delivery.ProviderPR{}, err
		}
		return g.mrToPort(mr), nil

	case code == http.StatusConflict || code == http.StatusUnprocessableEntity || code == http.StatusBadRequest:
		existing, err := g.openMR(ctx, proj, spec.SourceBranch, target)
		if err != nil {
			return delivery.ProviderPR{}, err
		}
		if existing != nil {
			// Guarantee 4: no PUT of title/description — ADR-0005 §4's evidence
			// package is not replaceable by a network retry.
			return g.mrToPort(*existing), nil
		}
		return delivery.ProviderPR{}, errs.Invalid(
			"GitLab refused to open the MR from %q to %q: %s",
			spec.SourceBranch, target, g.rest.redact(explainGL(body)))
	}
	return delivery.ProviderPR{}, g.rest.fail(code, explainGL(body),
		fmt.Sprintf("opening an MR in %s", spec.RepoExternalID))
}

// openMR returns (nil, nil) when there is no open MR for the pair of branches.
func (g *GitLab) openMR(ctx context.Context, proj, source, target string) (*glMR, error) {
	q := url.Values{}
	// No owner prefix, unlike GitHub's listing: here the filter is the branch's
	// bare name.
	q.Set("source_branch", source)
	q.Set("target_branch", target)
	q.Set("state", "opened")
	q.Set("per_page", "100")
	code, body, err := g.rest.do(ctx, http.MethodGet,
		"/projects/"+proj+"/merge_requests?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explainGL(body), "searching for an open MR")
	}
	var mrs []glMR
	if err := g.rest.decode(body, &mrs, "listing MRs"); err != nil {
		return nil, err
	}
	for i := range mrs {
		if mrs[i].SourceBranch == source && mrs[i].TargetBranch == target {
			return &mrs[i], nil
		}
	}
	return nil, nil
}

// mrByIID reads the MR. `rebase` turns on the parameter that makes GitLab report
// whether Sidekiq is still reapplying — without it the field simply does not
// come, and the absence would look like "it finished".
func (g *GitLab) mrByIID(ctx context.Context, proj string, iid int64, rebase bool) (*glMR, error) {
	route := fmt.Sprintf("/projects/%s/merge_requests/%d", proj, iid)
	if rebase {
		route += "?include_rebase_in_progress=true"
	}
	code, body, err := g.rest.do(ctx, http.MethodGet, route, nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explainGL(body), fmt.Sprintf("reading MR !%d", iid))
	}
	var mr glMR
	if err := g.rest.decode(body, &mr, "reading an MR"); err != nil {
		return nil, err
	}
	return &mr, nil
}

// ── Rebase ───────────────────────────────────────────────────────────────────

// Rebase reapplies the MR's branch on top of the target.
//
// The route exists (unlike GitHub's, which only has it in GraphQL), but it is
// ASYNCHRONOUS: GitLab answers 202 {"rebase_in_progress":true} and does the work
// in a worker. The port's guarantee 9 says the caller receives the OUTCOME, so
// the wait is here — and the outcome is read from the MR's (rebase_in_progress,
// merge_error) pair.
//
// This is the FIRM side of the normalization: the conflict's text is documented
// word for word ("Rebase failed. Please rebase locally"). Even so the
// classification goes through `looksLikeConflict`, and not through exact
// equality — the text is documented, not frozen, and the only thing worse than
// depending on a string is depending on it with `==`.
func (g *GitLab) Rebase(ctx context.Context, spec delivery.RebaseSpec) (delivery.RebaseResult, error) {
	proj, err := project(spec.RepoExternalID)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.rebaseTimeout)
	defer cancel()

	mr, err := g.openMR(ctx, proj, spec.Branch, spec.Onto)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if mr == nil {
		return delivery.RebaseResult{}, errs.Precondition(
			"there is no open MR from %q to %q in %s — neither GitLab nor GitHub "+
				"reapplies a loose branch: both only reapply an MR/PR's branch, and "+
				"always on top of ITS target",
			spec.Branch, spec.Onto, spec.RepoExternalID)
	}

	code, body, err := g.rest.do(ctx, http.MethodPut,
		fmt.Sprintf("/projects/%s/merge_requests/%d/rebase", proj, mr.IID), nil)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if code >= 300 {
		// GitLab's 403 here is documented with three texts, and all of them are
		// a PERMISSION failure or a missing branch — none is a conflict. Falling
		// into the common `fail` is the right thing: it becomes KindPermission
		// and does not pollute the attention box.
		return delivery.RebaseResult{}, g.rest.fail(code, explainGL(body),
			fmt.Sprintf("reapplying branch %q", spec.Branch))
	}

	for {
		current, err := g.mrByIID(ctx, proj, mr.IID, true)
		if err != nil {
			return delivery.RebaseResult{}, err
		}
		if !current.RebaseInProgress {
			detail := strings.TrimSpace(g.rest.redact(deref(current.MergeError)))
			if detail != "" {
				// Guarantees 1 and 2: a conflict is DATA, with a Detail. Files
				// empty — GitLab does NOT publish the list of conflicted files
				// on any v4 API route; what the interface uses is an internal
				// Rails route, outside /api/v4, with no version and no promise.
				if looksLikeConflict(detail) {
					return delivery.RebaseResult{
						HeadCommit: current.SHA,
						BaseCommit: current.DiffRefs.BaseSHA,
						Conflicted: true,
						Detail:     detail,
					}, nil
				}
				return delivery.RebaseResult{}, errs.Internal(
					"GitLab could not reapply branch %q: %s", spec.Branch, detail)
			}
			return delivery.RebaseResult{
				HeadCommit: current.SHA,
				BaseCommit: current.DiffRefs.BaseSHA,
			}, nil
		}
		// A cancelled context or a blown deadline comes out as KindUnavailable —
		// never as Conflicted=false, which would say "it did not conflict"
		// instead of "I do not know" (guarantee 9).
		if err := wait(ctx, g.poll, "waiting for the branch to be reapplied"); err != nil {
			return delivery.RebaseResult{}, err
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── Merge ────────────────────────────────────────────────────────────────────

// Merge integrates the MR.
//
// The same ambiguity as GitHub's, with different numbers: `405 Method Not
// Allowed` ("The merge request cannot merge") and `422 Branch cannot be merged`
// cover already merged, conflicted and blocked. As there, the refusal triggers a
// READ of the MR, and it is that read that separates guarantee 6 from guarantee
// 1 from guarantee 8.
func (g *GitLab) Merge(ctx context.Context, spec delivery.MergeSpec) (delivery.MergeResult, error) {
	if err := checkActor(g.actorID, spec.ActorID, "the merge"); err != nil {
		return delivery.MergeResult{}, err
	}
	proj, err := project(spec.RepoExternalID)
	if err != nil {
		return delivery.MergeResult{}, err
	}
	iid, err := strconv.ParseInt(strings.TrimSpace(spec.PRExternalID), 10, 64)
	if err != nil || iid <= 0 {
		return delivery.MergeResult{}, errs.Invalid(
			"a GitLab MR identifier must be the MR's iid, got %q", spec.PRExternalID)
	}

	code, body, err := g.rest.do(ctx, http.MethodPut,
		fmt.Sprintf("/projects/%s/merge_requests/%d/merge", proj, iid),
		map[string]any{"squash": g.squash})
	if err != nil {
		return delivery.MergeResult{}, err
	}
	if code == http.StatusOK {
		var mr glMR
		if err := g.rest.decode(body, &mr, "merging an MR"); err != nil {
			return delivery.MergeResult{}, err
		}
		return g.mergeResult(mr), nil
	}
	switch code {
	case http.StatusMethodNotAllowed, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusNotAcceptable:
		return g.classifyRefusalGL(ctx, proj, iid, explainGL(body))
	}
	return delivery.MergeResult{}, g.rest.fail(code, explainGL(body),
		fmt.Sprintf("merging MR !%d in %s", iid, spec.RepoExternalID))
}

// mergeResult translates a merged MR. Guarantee 7: Merged=true requires a
// commit.
//
// Note `squash_commit_sha`: with squash on, the commit that entered the base is
// THAT one, and `merge_commit_sha` may come back empty. Reading only the second
// would return Merged=true with an empty commit — guarantee 7 broken in silence,
// and ADR-0005's queue recording a merge without saying what went in.
func (g *GitLab) mergeResult(mr glMR) delivery.MergeResult {
	commit := mr.MergeCommitSHA
	if commit == "" {
		commit = mr.SquashCommitSHA
	}
	return delivery.MergeResult{
		Merged:       mr.State == "merged",
		MergeCommit:  commit,
		MergedAtUnix: unixOf(instantPtr(mr.MergedAt)),
	}
}

func (g *GitLab) classifyRefusalGL(ctx context.Context, proj string, iid int64, reason string) (delivery.MergeResult, error) {
	deadline := time.Now().Add(g.rebaseTimeout)
	for {
		mr, err := g.mrByIID(ctx, proj, iid, false)
		if err != nil {
			return delivery.MergeResult{}, err
		}
		if mr.State == "merged" {
			// Guarantee 6.
			r := g.mergeResult(*mr)
			r.Merged = true
			r.Detail = "the MR was already merged"
			return r, nil
		}
		if conflictGL(*mr) {
			// Guarantee 1.
			return delivery.MergeResult{
				Conflicted: true,
				Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
					"GitLab refused the merge over content that does not integrate (%s; "+
						"detailed_merge_status=%q; merge_status=%q)",
					reason, mr.DetailedMergeStatus, mr.MergeStatus))),
			}, nil
		}
		// `unchecked`, `checking` and `preparing` are GitLab's "I do not know":
		// it computes mergeability ASYNCHRONOUSLY, and it is the documentation
		// itself that tells you to ask again. Answering now would be asserting
		// "it is not a conflict" without anybody having looked.
		if !computing(*mr) {
			// Guarantee 8: it did not merge, it is not a conflict, and it says
			// what the reason is — with GitLab's vocabulary inside the Detail,
			// which is TEXT, and not promoted to a port field (guarantee 10).
			return delivery.MergeResult{
				Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
					"GitLab does not allow the merge yet (%s; detailed_merge_status=%q; merge_status=%q)",
					reason, mr.DetailedMergeStatus, mr.MergeStatus))),
			}, nil
		}
		if time.Now().After(deadline) {
			return delivery.MergeResult{}, errs.New(errs.KindUnavailable,
				"GitLab did not finish computing MR !%d's mergeability "+
					"(detailed_merge_status=%q) — without that answer there is no telling "+
					"a conflict from a block, and guessing sends the queue the wrong way",
				iid, mr.DetailedMergeStatus)
		}
		if err := wait(ctx, g.poll, "waiting for the MR's mergeability"); err != nil {
			return delivery.MergeResult{}, err
		}
	}
}

// conflictGL reads all THREE signals, because they are not redundant:
//
//   - `detailed_merge_status == "conflict"` is the current field and the only
//     one that names the conflict unambiguously;
//   - `merge_status == "cannot_be_merged"` is the legacy one, and it is the only
//     one that exists in self-hosted installations older than 15.6 — which are
//     exactly DOP's self-hosted audience;
//   - `has_conflicts` is derived from the legacy one (GitLab itself documents
//     that it is `false` unless `merge_status` is `cannot_be_merged`), so on its
//     own it adds nothing — but it confirms.
func conflictGL(mr glMR) bool {
	return mr.DetailedMergeStatus == "conflict" ||
		mr.MergeStatus == "cannot_be_merged" ||
		mr.HasConflicts
}

// computing covers the states in which GitLab still cannot answer.
func computing(mr glMR) bool {
	switch mr.DetailedMergeStatus {
	case "checking", "unchecked", "preparing", "approvals_syncing":
		return true
	}
	switch mr.MergeStatus {
	case "checking", "unchecked", "cannot_be_merged_recheck":
		return true
	}
	return false
}

// ── HasNativeQueue ───────────────────────────────────────────────────────────

// HasNativeQueue says whether this project has a native merge train
// (ADR-0005 §4).
//
// The source is the project's own `merge_trains_enabled`, and not the merge
// trains API: that one requires a Developer role or higher, is a paid-plan
// feature, and what it returns when the feature does not exist is NOT documented
// — 403, 404 and an empty list are all plausible, and branching on that is
// guesswork.
//
// The subtlety that decides guarantee 14 is the field's TRI-STATE. It is exposed
// by Enterprise Edition code under a licence condition: in an installation
// without the feature it does NOT come back `false` — it does NOT come back at
// all. That is why the adapter tells absent from false with a pointer:
//
//   - present and true  → it has a native queue; DOP's steps aside;
//   - present and false → it does not; DOP's orchestrates (ADR-0005 §4);
//   - ABSENT            → the installation offers no merge train at all, so
//     there is nothing to duplicate either: `false`. This is the adapter's only
//     inference, and it is conservative — it errs towards KEEPING DOP's queue,
//     which is the side that serializes the merges instead of leaving them
//     loose.
//
// The "I cannot look" case remains an ERROR: 401, 403 and 404 go up, because
// then you do not even know whether the project exists.
func (g *GitLab) HasNativeQueue(ctx context.Context, repoExternalID string) (bool, error) {
	proj, err := project(repoExternalID)
	if err != nil {
		return false, err
	}
	code, body, err := g.rest.do(ctx, http.MethodGet, "/projects/"+proj, nil)
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, g.rest.fail(code, explainGL(body),
			fmt.Sprintf("reading project %s", repoExternalID))
	}
	var p struct {
		MergeTrainsEnabled *bool `json:"merge_trains_enabled"`
	}
	if err := g.rest.decode(body, &p, "reading a project"); err != nil {
		return false, err
	}
	if p.MergeTrainsEnabled == nil {
		return false, nil
	}
	return *p.MergeTrainsEnabled, nil
}
