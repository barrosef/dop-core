// A delivery.GitProvider adapter over GitHub.
//
// ── Why REST **and** GraphQL ─────────────────────────────────────────────────
//
// Because GitHub HAS NO REBASE IN REST. The only similar endpoint is
// `PUT /pulls/{n}/update-branch`, and its documentation is explicit: it updates
// the PR's branch "merging HEAD from the base branch into the pull request
// branch". There is no `update_method` parameter, there is no other route, and
// the 2022 announcement that brought the rebase option is about the web
// interface only.
//
// That matters far more than it looks: using `update-branch` to fulfil `Rebase`
// would be performing a DIFFERENT operation from the one requested, with
// apparent success. ADR-0005's queue reapplies each PR on top of the updated
// `main` and RE-VERIFIES; a main→branch merge also "updates", but it produces a
// history and a head commit different from what the domain asked for, and
// nothing in the return would give the swap away. The honest alternative is
// GraphQL, where the rebase really exists:
//
//	updatePullRequestBranch(input: { pullRequestId: <node id>, updateMethod: REBASE })
//
// Note the price: the mutation takes the PR's NODE ID (not its number), and that
// is why the adapter has to locate the PR first. See Rebase.
//
// ── What GitHub hides ────────────────────────────────────────────────────────
//
// Two responses this adapter depends on are NOT published by GitHub: the 422's
// text when a PR already exists for the branch, and the GraphQL error's text
// when the rebase conflicts. Where it was possible, the adapter was designed not
// to need the text (see OpenPullRequest); where it was not, the comment says
// exactly where the bet is.
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

type GitHub struct {
	rest *client
	gql  *client
	// actorID is the actor THIS connection speaks for (guarantee 13). It is not
	// a credential: it is the name the check compares against.
	actorID       string
	mergeMethod   string
	rebaseTimeout time.Duration
	poll          time.Duration
}

type GitHubConfig struct {
	// APIBase is https://api.github.com on the public service and
	// https://<host>/api/v3 on GitHub Enterprise.
	APIBase string
	// GraphQLURL is separate because on Enterprise it is NOT APIBase+"/graphql":
	// REST lives at /api/v3 and GraphQL at /api/graphql. Deducing one from the
	// other would work on github.com and break in every self-hosted installation
	// — exactly the kind of difference that only shows up at the client's.
	GraphQLURL string
	// Token is the ALREADY RESOLVED value of the resource credential
	// (ADR-0009). This package does not know ports.SecretStore.
	Token   string
	ActorID string
	// MergeMethod is the git flow's policy (ADR-0009), not the domain's.
	MergeMethod   string // merge | squash | rebase
	Timeout       time.Duration
	RebaseTimeout time.Duration
	Poll          time.Duration
	Client        httpDoer
}

func NewGitHub(cfg GitHubConfig) *GitHub {
	base := cfg.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	base = strings.TrimRight(base, "/")
	gql := cfg.GraphQLURL
	if gql == "" {
		gql = base + "/graphql"
	}
	authorize := func(r *http.Request) {
		if cfg.Token != "" {
			r.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		// The version is PINNED in the adapter, not configurable. It is
		// GitHub's promise not to change the response's format under us;
		// leaving it out means accepting the default version, which changes on
		// its own.
		r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		r.Header.Set("Accept", "application/vnd.github+json")
		// GitHub REJECTS a request with no User-Agent with a 403 — it is one of
		// the few headers the documentation calls genuinely mandatory.
		r.Header.Set("User-Agent", "dop-core")
	}
	mm := cfg.MergeMethod
	if mm == "" {
		mm = "merge"
	}
	rt := cfg.RebaseTimeout
	if rt <= 0 {
		rt = DefaultRebaseTimeout
	}
	p := cfg.Poll
	if p <= 0 {
		p = defaultPoll
	}
	return &GitHub{
		rest:          newClient(base, "GitHub", cfg.Client, cfg.Timeout, authorize, cfg.Token),
		gql:           newClient(gql, "GitHub", cfg.Client, cfg.Timeout, authorize, cfg.Token),
		actorID:       cfg.ActorID,
		mergeMethod:   mm,
		rebaseTimeout: rt,
		poll:          p,
	}
}

var _ delivery.GitProvider = (*GitHub)(nil)

// String: a VALUE receiver, so it also applies to `%+v` of a value. Without it,
// fmt would walk the fields through reflection — including the unexported ones,
// whose String() it cannot call.
func (g GitHub) String() string { return "gitprovider.GitHub{actor=" + g.actorID + "}" }

// ── GitHub's shapes (only what the port uses) ────────────────────────────────

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type ghPR struct {
	Number         int     `json:"number"`
	NodeID         string  `json:"node_id"`
	HTMLURL        string  `json:"html_url"`
	State          string  `json:"state"`
	Draft          bool    `json:"draft"`
	Merged         bool    `json:"merged"`
	Mergeable      *bool   `json:"mergeable"` // nil while GitHub computes it
	MergeableState string  `json:"mergeable_state"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	CreatedAt      string  `json:"created_at"`
	MergedAt       *string `json:"merged_at"`
	Head           ghRef   `json:"head"`
	Base           ghRef   `json:"base"`
}

type ghError struct {
	Message string `json:"message"`
	Errors  []struct {
		Resource string `json:"resource"`
		Field    string `json:"field"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// explainGH flattens GitHub's error body into one sentence.
func explainGH(body []byte) string {
	var e ghError
	_ = tolerantUnmarshal(body, &e)
	parts := make([]string, 0, 1+len(e.Errors))
	if s := strings.TrimSpace(e.Message); s != "" {
		parts = append(parts, s)
	}
	for _, it := range e.Errors {
		if s := strings.TrimSpace(it.Message); s != "" {
			parts = append(parts, s)
			continue
		}
		if it.Field != "" || it.Code != "" {
			parts = append(parts, strings.TrimSpace(it.Resource+"."+it.Field+": "+it.Code))
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(string(body))
	}
	return strings.Join(parts, "; ")
}

// owner and name out of "owner/repo".
func repoParts(external string) (owner, name string, err error) {
	p := strings.SplitN(strings.Trim(external, "/"), "/", 3)
	if len(p) != 2 || p[0] == "" || p[1] == "" {
		return "", "", errs.Invalid(
			"a GitHub repository identifier must be \"owner/name\", got %q", external)
	}
	return p[0], p[1], nil
}

func (g *GitHub) prPath(owner, name string, n int) string {
	return fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(name), n)
}

func (g *GitHub) prToPort(pr ghPR) delivery.ProviderPR {
	return delivery.ProviderPR{
		// ExternalID is OPAQUE to the domain (guarantee 10): here it is the PR's
		// number, and nothing outside this file may count on that.
		ExternalID:   strconv.Itoa(pr.Number),
		URL:          pr.HTMLURL,
		HeadCommit:   pr.Head.SHA,
		TargetBranch: pr.Base.Ref,
		CreatedAt:    instant(pr.CreatedAt),
	}
}

// ── OpenPullRequest ──────────────────────────────────────────────────────────

// OpenPullRequest opens the PR, and it is IDEMPOTENT by (repo, source, target).
//
// The way guarantee 3 is delivered here deserves an explanation, because the
// obvious path is a trap: GitHub refuses the second PR with a 422 and a
// validation message — and that message is NOT DOCUMENTED. The published body of
// the 422 has only the schema; the text "A pull request already exists for …" is
// observational folklore, and GitHub never promised to keep it.
//
// So the adapter does NOT match text. Faced with ANY 422 it goes and LOOKS for
// the open PR for that pair of branches: if it exists, the 422 was a duplicate
// and the answer is the existing PR; if it does not, the 422 was something else
// (a branch with no commits, an invalid target) and it becomes an error with the
// provider's message alongside. Idempotency comes to depend on a queryable FACT
// instead of a string that may change without notice.
func (g *GitHub) OpenPullRequest(ctx context.Context, spec delivery.OpenPRSpec) (delivery.ProviderPR, error) {
	if err := checkActor(g.actorID, spec.ActorID, "opening the PR"); err != nil {
		return delivery.ProviderPR{}, err
	}
	owner, name, err := repoParts(spec.RepoExternalID)
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	target := spec.TargetBranch
	if target == "" {
		target = "main"
	}

	code, body, err := g.rest.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(name)),
		map[string]any{
			// `head` WITHOUT the owner prefix: in the creation BODY it is only
			// needed for a PR between forks. In the LISTING, just below, the
			// prefix is mandatory. The asymmetry is GitHub's, not ours.
			"head":  spec.SourceBranch,
			"base":  target,
			"title": spec.Title,
			"body":  spec.Body,
		})
	if err != nil {
		return delivery.ProviderPR{}, err
	}
	switch {
	case code == http.StatusCreated:
		var pr ghPR
		if err := g.rest.decode(body, &pr, "opening a PR"); err != nil {
			return delivery.ProviderPR{}, err
		}
		return g.prToPort(pr), nil

	case code == http.StatusUnprocessableEntity:
		existing, err := g.openPR(ctx, owner, name, spec.SourceBranch, target)
		if err != nil {
			return delivery.ProviderPR{}, err
		}
		if existing != nil {
			// Guarantee 4: return what exists, with the ORIGINAL title and
			// body. There is no PATCH here, on purpose — ADR-0005 §4's evidence
			// package must not be replaced by a retry.
			return g.prToPort(*existing), nil
		}
		return delivery.ProviderPR{}, errs.Invalid(
			"GitHub refused to open the PR from %q to %q: %s",
			spec.SourceBranch, target, g.rest.redact(explainGH(body)))
	}
	return delivery.ProviderPR{}, g.rest.fail(code, explainGH(body),
		fmt.Sprintf("opening a PR in %s/%s", owner, name))
}

// openPR locates the OPEN PR for a pair of branches. It returns (nil, nil) when
// there is none — absence is not an error here, it is the information asked for.
func (g *GitHub) openPR(ctx context.Context, owner, name, source, target string) (*ghPR, error) {
	// `head` in the listing REQUIRES the owner prefix ("owner:branch") — it is
	// the format the documentation describes, and the only one it describes.
	q := url.Values{}
	q.Set("head", owner+":"+source)
	q.Set("base", target)
	q.Set("state", "open")
	q.Set("per_page", "100")
	code, body, err := g.rest.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls?%s", url.PathEscape(owner), url.PathEscape(name), q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explainGH(body),
			fmt.Sprintf("searching for an open PR in %s/%s", owner, name))
	}
	var prs []ghPR
	if err := g.rest.decode(body, &prs, "listing PRs"); err != nil {
		return nil, err
	}
	for i := range prs {
		if prs[i].Head.Ref == source && prs[i].Base.Ref == target {
			return &prs[i], nil
		}
	}
	return nil, nil
}

func (g *GitHub) prByNumber(ctx context.Context, owner, name string, n int) (*ghPR, error) {
	code, body, err := g.rest.do(ctx, http.MethodGet, g.prPath(owner, name, n), nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, g.rest.fail(code, explainGH(body), fmt.Sprintf("reading PR #%d", n))
	}
	var pr ghPR
	if err := g.rest.decode(body, &pr, "reading a PR"); err != nil {
		return nil, err
	}
	return &pr, nil
}

// ── Rebase ───────────────────────────────────────────────────────────────────

// Rebase reapplies the PR's branch on top of the target, through the GraphQL
// mutation.
//
// Two things the port's signature hides and that are worth more than the code:
//
//  1. THERE IS NO "branch rebase" in the providers. What exists is "reapply THIS
//     PR's branch". Both — GitHub and GitLab — require an open PR/MR and reapply
//     on top of ITS target. That is why `Onto` is not free: if it does not match
//     the PR's target, the request is refused instead of being reapplied on top
//     of the wrong place in silence;
//
//  2. GitHub answers the mutation with HTTP 200 even when it FAILS — the failure
//     comes in the body's `errors` array. An adapter that looked only at the
//     status code would report "rebase done" for every conflict, and ADR-0005's
//     queue would merge on top of a branch that was not reapplied.
func (g *GitHub) Rebase(ctx context.Context, spec delivery.RebaseSpec) (delivery.RebaseResult, error) {
	owner, name, err := repoParts(spec.RepoExternalID)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.rebaseTimeout)
	defer cancel()

	pr, err := g.openPR(ctx, owner, name, spec.Branch, spec.Onto)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if pr == nil {
		return delivery.RebaseResult{}, errs.Precondition(
			"there is no open PR from %q to %q in %s/%s — neither GitHub nor GitLab "+
				"reapplies a loose branch: both only reapply a PR/MR's branch, and "+
				"always on top of ITS target",
			spec.Branch, spec.Onto, owner, name)
	}

	const mut = `mutation($pr:ID!,$head:GitObjectID!){` +
		`updatePullRequestBranch(input:{pullRequestId:$pr,expectedHeadOid:$head,updateMethod:REBASE})` +
		`{pullRequest{id}}}`
	code, body, err := g.gql.do(ctx, http.MethodPost, "", map[string]any{
		"query": mut,
		// expectedHeadOid is optimistic concurrency: if the branch moved
		// between the read and the mutation, GitHub refuses instead of
		// reapplying on top of a head the queue did not verify.
		"variables": map[string]any{"pr": pr.NodeID, "head": pr.Head.SHA},
	})
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	if code >= 300 {
		return delivery.RebaseResult{}, g.gql.fail(code, explainGH(body), "reapplying the branch")
	}
	var resp struct {
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := g.gql.decode(body, &resp, "reapplying the branch"); err != nil {
		return delivery.RebaseResult{}, err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		detail := g.gql.redact(strings.Join(msgs, "; "))
		// HERE IS THE BET, and it is written down for whoever comes next:
		// GitHub does NOT publish the text of a conflicted rebase's error. There
		// is no structured field saying "conflict" — `type` comes back generic.
		// So the classification is by a marker in the text, and the default is
		// ERROR, not conflict: classifying wrongly towards conflict would send a
		// human to solve a permission problem in the attention box, and the
		// error at least shows up in the right place with the whole message.
		if looksLikeConflict(detail) {
			return delivery.RebaseResult{
				BaseCommit: pr.Base.SHA,
				HeadCommit: pr.Head.SHA,
				Conflicted: true,
				// Files empty: GitHub does not publish the list (guarantee 2).
				Detail: detail,
			}, nil
		}
		return delivery.RebaseResult{}, errs.Internal(
			"GitHub refused to reapply branch %q: %s", spec.Branch, detail)
	}

	// Re-reading the PR for the head and base commits.
	//
	// An honest caveat: GitHub does NOT document whether the mutation finishes
	// before answering. If it is asynchronous, this SHA may be the one from
	// BEFORE. What the adapter does NOT do is pretend: it returns what it read,
	// and the queue re-verifies on top of that commit — which is ADR-0005 §1's
	// protection against exactly "I thought the code was in another state".
	current, err := g.prByNumber(ctx, owner, name, pr.Number)
	if err != nil {
		return delivery.RebaseResult{}, err
	}
	return delivery.RebaseResult{
		HeadCommit: current.Head.SHA,
		BaseCommit: current.Base.SHA,
	}, nil
}

// looksLikeConflict is shared by both adapters. The wording comes from the
// provider, not from us, so it matches on the phrases they publish.
func looksLikeConflict(s string) bool {
	l := strings.ToLower(s)
	for _, m := range []string{"conflict", "rebase failed", "not mergeable", "cannot be merged"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// ── Merge ────────────────────────────────────────────────────────────────────

// Merge integrates the PR.
//
// The real work is in the REFUSAL: GitHub answers `405 {"message":"Pull Request
// is not mergeable"}` — the SAME response — for an already merged PR, a
// conflicted PR, a PR blocked by a required check and a draft PR. Four facts
// with opposite consequences behind a single code.
//
// That is why the refusal triggers a READ of the PR, and it is that read that
// decides between guarantee 6 (already merged ⇒ idempotent success), guarantee 1
// (conflict ⇒ data) and guarantee 8 (blocked ⇒ it did not merge and it is not a
// conflict).
func (g *GitHub) Merge(ctx context.Context, spec delivery.MergeSpec) (delivery.MergeResult, error) {
	if err := checkActor(g.actorID, spec.ActorID, "the merge"); err != nil {
		return delivery.MergeResult{}, err
	}
	owner, name, err := repoParts(spec.RepoExternalID)
	if err != nil {
		return delivery.MergeResult{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(spec.PRExternalID))
	if err != nil || n <= 0 {
		return delivery.MergeResult{}, errs.Invalid(
			"a GitHub PR identifier must be the PR's number, got %q", spec.PRExternalID)
	}

	code, body, err := g.rest.do(ctx, http.MethodPut, g.prPath(owner, name, n)+"/merge",
		map[string]any{"merge_method": g.mergeMethod})
	if err != nil {
		return delivery.MergeResult{}, err
	}
	if code == http.StatusOK {
		var ok struct {
			SHA    string `json:"sha"`
			Merged bool   `json:"merged"`
		}
		if err := g.rest.decode(body, &ok, "merging a PR"); err != nil {
			return delivery.MergeResult{}, err
		}
		// One extra read: the merge's response does not carry the INSTANT, and
		// guarantee 7 asks for the commit AND the when. ADR-0005's queue records
		// both in the event that feeds the cockpit.
		pr, err := g.prByNumber(ctx, owner, name, n)
		if err != nil {
			return delivery.MergeResult{}, err
		}
		commit := ok.SHA
		if commit == "" {
			commit = pr.MergeCommitSHA
		}
		return delivery.MergeResult{
			Merged: ok.Merged || pr.Merged, MergeCommit: commit,
			MergedAtUnix: unixOf(instantPtr(pr.MergedAt)),
		}, nil
	}
	switch code {
	case http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity:
		return g.classifyRefusal(ctx, owner, name, n, explainGH(body))
	}
	return delivery.MergeResult{}, g.rest.fail(code, explainGH(body),
		fmt.Sprintf("merging PR #%d in %s/%s", n, owner, name))
}

// classifyRefusal undoes the 405's ambiguity.
func (g *GitHub) classifyRefusal(ctx context.Context, owner, name string, n int, reason string) (delivery.MergeResult, error) {
	pr, err := g.prByNumber(ctx, owner, name, n)
	if err != nil {
		return delivery.MergeResult{}, err
	}
	// `mergeable` is NIL while GitHub computes it. Nil is not "no conflict": it
	// is "I do not know", and guarantee 9 says "I do not know" never becomes
	// Conflicted=false. So we wait for the answer, and giving up becomes
	// KindUnavailable.
	deadline := time.Now().Add(g.rebaseTimeout)
	for pr.Mergeable == nil && !pr.Merged {
		if time.Now().After(deadline) {
			return delivery.MergeResult{}, errs.New(errs.KindUnavailable,
				"GitHub did not finish computing PR #%d's mergeability — without that "+
					"answer there is no telling a conflict from a block, and guessing "+
					"either one sends the queue the wrong way", n)
		}
		if err := wait(ctx, g.poll, "waiting for the PR's mergeability"); err != nil {
			return delivery.MergeResult{}, err
		}
		if pr, err = g.prByNumber(ctx, owner, name, n); err != nil {
			return delivery.MergeResult{}, err
		}
	}
	if pr.Merged {
		// Guarantee 6. The queue reprocesses the position after a crash and has
		// to recognize what already went in: returning an error here would send
		// the merged entry back to the attention box as a problem. This check is
		// SINGLE on purpose — there used to be a second one, before the loop,
		// and the duplicate made the break experiment pass: deleting either one
		// changed no behaviour at all, and a guarantee that survives being
		// deleted is not being verified.
		return delivery.MergeResult{
			Merged: true, MergeCommit: pr.MergeCommitSHA,
			MergedAtUnix: unixOf(instantPtr(pr.MergedAt)),
			Detail:       "the PR was already merged",
		}, nil
	}
	if pr.Mergeable != nil && !*pr.Mergeable {
		// Guarantee 1: a conflict is DATA.
		return delivery.MergeResult{
			Conflicted: true,
			Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
				"GitHub refused the merge over content that does not integrate (%s; mergeable_state=%q)",
				reason, pr.MergeableState))),
		}, nil
	}
	// Guarantee 8: it did not merge, and it is not a conflict. A draft, a
	// pending required check, a missing review, a branch protection rule.
	return delivery.MergeResult{
		Detail: g.rest.redact(strings.TrimSpace(fmt.Sprintf(
			"GitHub does not allow the merge yet (%s; mergeable_state=%q; draft=%v)",
			reason, pr.MergeableState, pr.Draft))),
	}, nil
}

// ── HasNativeQueue ───────────────────────────────────────────────────────────

// HasNativeQueue says whether this repository has a native merge queue
// (ADR-0005 §4).
//
// A divergence the normalization had to absorb: GitHub's merge queue is PER
// BRANCH — it is a ruleset rule matching a ref pattern — and GitLab's merge
// trains are PER PROJECT. The port asks per REPOSITORY, and the honest answer
// for GitHub is about the branch ADR-0005's queue actually contends for: the
// DEFAULT branch. Hence the two calls — find the default and ask for its rules.
//
// Guarantee 14 in two lines: a 403 becomes an ERROR. With no permission to read
// the rules, the adapter does not KNOW — and answering `false` would be
// asserting "you may orchestrate on top" without having looked, with two queues
// merging the same repository as the prize.
// Whoami answers with the login the token belongs to — the credential check
// of the onboarding journey (spec 2026-09-20 §5). It is the cheapest call a
// token can make and it fails the way the port promises: a 401 is
// KindUnauthorized, with no provider text.
func (g *GitHub) Whoami(ctx context.Context) (string, error) {
	code, body, err := g.rest.do(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return "", err
	}
	if code >= 300 {
		return "", g.rest.fail(code, explainGH(body), "reading the token's user")
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := g.rest.decode(body, &me, "reading the token's user"); err != nil {
		return "", err
	}
	return me.Login, nil
}

func (g *GitHub) HasNativeQueue(ctx context.Context, repoExternalID string) (bool, error) {
	owner, name, err := repoParts(repoExternalID)
	if err != nil {
		return false, err
	}
	code, body, err := g.rest.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name)), nil)
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, g.rest.fail(code, explainGH(body), fmt.Sprintf("reading repository %s/%s", owner, name))
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.rest.decode(body, &repo, "reading a repository"); err != nil {
		return false, err
	}
	if repo.DefaultBranch == "" {
		return false, errs.New(errs.KindUnavailable,
			"GitHub did not report the default branch of %s/%s — without it there is no "+
				"branch to ask about the native queue for", owner, name)
	}

	// This route resolves repository AND organization rulesets, already filtered
	// to the ones in force. The alternative (listing rulesets and opening them
	// one by one) returns a summary with no rules and would force matching the
	// ref globs by hand.
	code, body, err = g.rest.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/rules/branches/%s",
			url.PathEscape(owner), url.PathEscape(name), url.PathEscape(repo.DefaultBranch)), nil)
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, g.rest.fail(code, explainGH(body),
			fmt.Sprintf("reading the rules of branch %q of %s/%s", repo.DefaultBranch, owner, name))
	}
	var rules []struct {
		Type string `json:"type"`
	}
	if err := g.rest.decode(body, &rules, "branch rules"); err != nil {
		return false, err
	}
	for _, r := range rules {
		if r.Type == "merge_queue" {
			return true, nil
		}
	}
	return false, nil
}
