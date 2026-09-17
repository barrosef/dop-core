// Package projectrepo hosts the projects' root repositories (ADR-0021).
//
// ── What this is ────────────────────────────────────────────────────────────
//
// A git server. Bare repositories on a directory, served over git's smart HTTP
// protocol by `git http-backend` — the CGI program git itself ships — behind a
// handler that does the two things git's CGI does not: authenticate the caller
// with a bearer token that opens exactly one repository, and tell the platform
// when a push landed.
//
// ── Why `git http-backend` and not a Go implementation of the protocol ──────
//
// The smart protocol has two halves — `upload-pack` and `receive-pack` — and
// both are the reference implementation's business: pack negotiation, delta
// windows, ref advertisement with capabilities, hooks. Reimplementing them
// would be a second git; wrapping the CGI is a hundred lines and it is what
// every self-hosted git server does underneath. The one dependency it costs is
// the `git` binary in the core's image, which the mirror needs anyway.
//
// ── The token ───────────────────────────────────────────────────────────────
//
// HMAC over (project, demand, expiry), base64. It is not a session and it is
// not stored: the server verifies it with the same key that minted it. A token
// for project X presented on project Y's path fails the check on the path, not
// on the signature — guarantee 3 of the port, 21 of the sandbox's.
package projectrepo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
)

// Server serves the repositories under Root at `<prefix>/<project>.git`.
type Server struct {
	root    string
	key     []byte
	gitBin  string
	prefix  string
	onPush  func(ctx context.Context, p ports.Push)
	mirrors sync.Map // projectID → mirror
	mu      sync.Mutex
	secrets ports.SecretStore
}

// WithSecrets gives the server the store it resolves a mirror's credential
// from. Without it a mirror is accepted and never pushed — loudly, in the log.
func (s *Server) WithSecrets(st ports.SecretStore) *Server {
	s.secrets = st
	return s
}

type mirror struct {
	url        string
	credential ports.SecretRef
}

// Config of the server. Root is the directory of the bare repositories; Key is
// the HMAC key the tokens are minted and verified with; Prefix is the URL path
// the repositories hang from (`/git`).
type Config struct {
	Root   string
	Key    []byte
	Prefix string
	GitBin string
}

func NewServer(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errs.Invalid("the git server needs a root directory")
	}
	if len(cfg.Key) < 16 {
		return nil, errs.Invalid("the git server needs a key of at least 16 bytes to mint tokens")
	}
	if err := os.MkdirAll(cfg.Root, 0o750); err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "the git server cannot create its root")
	}
	bin := cfg.GitBin
	if bin == "" {
		bin = "git"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "the git server needs the git binary")
	}
	prefix := strings.TrimRight(cfg.Prefix, "/")
	if prefix == "" {
		prefix = "/git"
	}
	return &Server{root: cfg.Root, key: cfg.Key, gitBin: bin, prefix: prefix}, nil
}

// Prefix is where the server expects to be mounted.
func (s *Server) Prefix() string { return s.prefix + "/" }

var projectIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (s *Server) repoPath(projectID string) (string, error) {
	if !projectIDRe.MatchString(projectID) {
		return "", errs.Invalid("invalid project id for a repository: %q", projectID)
	}
	return filepath.Join(s.root, projectID+".git"), nil
}

// ── the repositories ────────────────────────────────────────────────────────

// ensure creates the bare repository with its first commit (guarantee 2).
func (s *Server) ensure(ctx context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.repoPath(projectID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	if err := s.git(ctx, "", "init", "--bare", "--initial-branch=main", dir); err != nil {
		return err
	}
	// http-backend refuses to serve a repository that does not declare itself
	// exportable — this file is the declaration.
	if err := os.WriteFile(filepath.Join(dir, "git-daemon-export-ok"), nil, 0o640); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to mark the repository exportable")
	}
	if err := s.git(ctx, dir, "config", "http.receivepack", "true"); err != nil {
		return err
	}
	// The first commit: the manifest. An empty clone would tell the agent "this
	// project knows nothing", which is a different statement from "it is new".
	_, err = s.commit(ctx, projectID, ports.RepositoryCommit{
		Files: []ports.RepositoryFile{{
			Path:    "README.md",
			Content: []byte(initialManifest),
		}},
		Message:    "The project's root repository is born (ADR-0021)",
		AuthorName: "DOP", AuthorEmail: "platform@dop",
		CommitterName: "DOP", CommitterEmail: "platform@dop",
	})
	return err
}

const initialManifest = `# The project's library

Everything here is available to every agent of this project. Nothing needs to be fetched.

- ` + "`rules/`" + ` — conventions this project OBEYS. Read them before deciding anything.
- ` + "`demand/<id>/`" + ` — what belongs to one demand: spec, plan, context.
- ` + "`index/`" + ` — one map per repository: what lives where, how to build, how to test.
- ` + "`memory/`" + ` — findings and lessons from past demands. Consult them; they are not orders.

This project has no documents recorded yet.
`

// commit writes files into the bare repository WITHOUT a working copy: a
// temporary index, `hash-object`, `update-index`, `write-tree`, `commit-tree`,
// `update-ref`. It is the plumbing, and it is what makes a platform-side write
// atomic and free of checkouts.
func (s *Server) commit(ctx context.Context, projectID string, c ports.RepositoryCommit) (string, error) {
	dir, err := s.repoPath(projectID)
	if err != nil {
		return "", err
	}
	if len(c.Files) == 0 {
		return "", errs.Invalid("a commit with no files")
	}
	for _, f := range c.Files {
		if err := validPath(f.Path); err != nil {
			return "", err
		}
	}
	index, err := os.CreateTemp("", "dop-index-*")
	if err != nil {
		return "", errs.Wrap(errs.KindUnavailable, err, "failed to create the index")
	}
	indexPath := index.Name()
	_ = index.Close()
	_ = os.Remove(indexPath)
	defer os.Remove(indexPath)
	env := []string{"GIT_INDEX_FILE=" + indexPath}

	parent, _ := s.gitOut(ctx, dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	parent = strings.TrimSpace(parent)
	if parent != "" {
		if err := s.gitEnv(ctx, dir, env, "read-tree", parent); err != nil {
			return "", err
		}
	}
	for _, f := range c.Files {
		if f.Delete {
			if err := s.gitEnv(ctx, dir, env, "update-index", "--force-remove", f.Path); err != nil {
				return "", err
			}
			continue
		}
		blob, err := s.gitIn(ctx, dir, env, f.Content, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		blob = strings.TrimSpace(blob)
		if err := s.gitEnv(ctx, dir, env, "update-index", "--add", "--cacheinfo",
			"100644,"+blob+","+f.Path); err != nil {
			return "", err
		}
	}
	tree, err := s.gitOut(ctx, dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)

	author := c.AuthorName + " <" + c.AuthorEmail + ">"
	committer := c.CommitterName + " <" + c.CommitterEmail + ">"
	if c.AuthorName == "" {
		author = "DOP <platform@dop>"
	}
	if c.CommitterName == "" {
		committer = "DOP <platform@dop>"
	}
	cenv := []string{
		"GIT_AUTHOR_NAME=" + nameOf(author), "GIT_AUTHOR_EMAIL=" + emailOf(author),
		"GIT_COMMITTER_NAME=" + nameOf(committer), "GIT_COMMITTER_EMAIL=" + emailOf(committer),
	}
	args := []string{"commit-tree", tree, "-m", c.Message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	sha, err := s.gitOutEnv(ctx, dir, cenv, args...)
	if err != nil {
		return "", err
	}
	sha = strings.TrimSpace(sha)
	if err := s.git(ctx, dir, "update-ref", "refs/heads/main", sha, parent); err != nil {
		return "", err
	}
	s.notify(ctx, ports.Push{ProjectID: projectID, Ref: "refs/heads/main", Before: parent, After: sha})
	return sha, nil
}

func (s *Server) read(ctx context.Context, projectID, path string) ([]byte, error) {
	dir, err := s.repoPath(projectID)
	if err != nil {
		return nil, err
	}
	if err := validPath(path); err != nil {
		return nil, err
	}
	out, err := s.gitOut(ctx, dir, nil, "show", "HEAD:"+path)
	if err != nil {
		if errs.KindOf(err) == errs.KindUnavailable && strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "exists on disk, but not in") ||
			strings.Contains(err.Error(), "fatal: path") ||
			strings.Contains(err.Error(), "invalid object name") {
			return nil, errs.NotFound("file")
		}
		return nil, err
	}
	return []byte(out), nil
}

func validPath(p string) error {
	clean := filepath.ToSlash(filepath.Clean(p))
	switch {
	case p == "" || strings.HasPrefix(p, "/"):
		return errs.Invalid("a repository path has to be relative: %q", p)
	case clean != p:
		return errs.Invalid("a repository path has to be normalized: %q", p)
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return errs.Invalid("a repository path cannot escape: %q", p)
	}
	return nil
}

// ── tokens ──────────────────────────────────────────────────────────────────

// mint signs (project, demand, expiry). The demand rides along so a push can
// be attributed to the demand whose token made it (guarantee 7's DemandID).
func (s *Server) mint(projectID, demandID string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", errs.Invalid("a token needs a positive ttl")
	}
	exp := time.Now().Add(ttl).Unix()
	payload := projectID + "|" + demandID + "|" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + sig)), nil
}

// verify answers which project and demand the token opens, if it is valid and
// unexpired. The PATH check against the project is the caller's — that is
// where "a token of X on Y" is refused.
func (s *Server) verify(token string) (projectID, demandID string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return "", "", false
	}
	payload := strings.Join(parts[:3], "|")
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[3])) {
		return "", "", false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ── HTTP: the smart protocol behind the token ───────────────────────────────

// ServeHTTP handles `<prefix>/<project>.git/...` and hands the rest to git's
// CGI. Authentication happens HERE, once, before git sees the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, s.prefix+"/")
	slash := strings.Index(rest, "/")
	if slash <= 0 || !strings.HasSuffix(rest[:slash], ".git") {
		http.NotFound(w, r)
		return
	}
	projectID := strings.TrimSuffix(rest[:slash], ".git")
	dir, err := s.repoPath(projectID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		http.NotFound(w, r)
		return
	}

	tokenProject, demandID, ok := s.verify(bearer(r))
	if !ok || tokenProject != projectID {
		// The same answer for "no token", "bad token", "expired" and "another
		// project's": telling them apart would tell the caller which
		// repositories exist.
		// BASIC, not Bearer, and the choice is load-bearing: git only knows how
		// to answer a Basic challenge — it invokes the credential helper and
		// resends with the token as the password. A Bearer challenge makes git
		// give up without ever asking the helper, and the sandbox comes up with
		// no shelf for a reason nothing in the log explains.
		w.Header().Set("WWW-Authenticate", `Basic realm="dop"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	isPush := r.URL.Query().Get("service") == "git-receive-pack" ||
		strings.HasSuffix(r.URL.Path, "/git-receive-pack")
	var before string
	if isPush && r.Method == http.MethodPost {
		before, _ = s.gitOut(r.Context(), dir, nil, "rev-parse", "--verify", "-q", "refs/heads/main")
		before = strings.TrimSpace(before)
	}

	h := &cgi.Handler{
		Path: s.gitPath(),
		// Root is what the CGI strips off the URL to make PATH_INFO. Stripping
		// the prefix leaves `/<project>.git/info/refs`, which http-backend
		// resolves against GIT_PROJECT_ROOT — exactly the bare repository.
		Root: s.prefix,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=" + demandID,
		},
	}
	h.ServeHTTP(w, r)

	if isPush && r.Method == http.MethodPost {
		after, _ := s.gitOut(r.Context(), dir, nil, "rev-parse", "--verify", "-q", "refs/heads/main")
		after = strings.TrimSpace(after)
		if after != "" && after != before {
			s.notify(r.Context(), ports.Push{
				ProjectID: projectID, DemandID: demandID,
				Ref: "refs/heads/main", Before: before, After: after,
			})
		}
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return h[7:]
	}
	// Basic is how git answers the challenge above: the token is the password
	// and the user name is ignored.
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	return ""
}

func (s *Server) gitPath() string {
	p, err := exec.LookPath(s.gitBin)
	if err != nil {
		return s.gitBin
	}
	return p
}

// ── push fan-out and the mirror ─────────────────────────────────────────────

func (s *Server) notify(ctx context.Context, p ports.Push) {
	if m, ok := s.mirrors.Load(p.ProjectID); ok {
		s.pushMirror(ctx, p.ProjectID, m.(mirror))
	}
	if s.onPush != nil {
		s.onPush(ctx, p)
	}
}

// pushMirror pushes onward with the user's credential resolved from the
// SecretStore at push time — it lives in memory for the length of one push and
// in no file, no URL and no sandbox (guarantee 8).
func (s *Server) pushMirror(ctx context.Context, projectID string, m mirror) {
	dir, err := s.repoPath(projectID)
	if err != nil {
		return
	}
	log := logging.From(ctx)
	secret, err := s.resolve(ctx, m.credential)
	if err != nil {
		log.Warn("mirror push skipped: credential not resolved", "project_id", projectID)
		return
	}
	askpass, err := writeAskpass(secret)
	if err != nil {
		log.Warn("mirror push skipped", "project_id", projectID, "error", err.Error())
		return
	}
	defer os.Remove(askpass)
	env := []string{"GIT_ASKPASS=" + askpass, "GIT_TERMINAL_PROMPT=0"}
	if err := s.gitEnv(ctx, dir, env, "push", "--mirror", m.url); err != nil {
		log.Warn("mirror push failed", "project_id", projectID, "error", err.Error())
	}
}

// resolve is set by whoever builds the server with a SecretStore; without one,
// a mirror cannot be pushed.
func (s *Server) resolve(ctx context.Context, ref ports.SecretRef) ([]byte, error) {
	if s.secrets == nil {
		return nil, errors.New("no secret store")
	}
	return s.secrets.Get(ctx, ref)
}

// writeAskpass materializes a one-shot askpass program that answers git's
// credential prompt with the secret. It is a file for the length of the push.
func writeAskpass(secret []byte) (string, error) {
	f, err := os.CreateTemp("", "dop-askpass-*")
	if err != nil {
		return "", err
	}
	script := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(string(secret), "'", "'\\''") + "'\n"
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = f.Close()
	return f.Name(), os.Chmod(f.Name(), 0o700)
}

// ── git plumbing ────────────────────────────────────────────────────────────

func (s *Server) git(ctx context.Context, dir string, args ...string) error {
	_, err := s.gitOutEnv(ctx, dir, nil, args...)
	return err
}

func (s *Server) gitEnv(ctx context.Context, dir string, env []string, args ...string) error {
	_, err := s.gitOutEnv(ctx, dir, env, args...)
	return err
}

func (s *Server) gitOut(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return s.gitOutEnv(ctx, dir, env, args...)
}

func (s *Server) gitIn(ctx context.Context, dir string, env []string, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.gitBin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(string(stdin))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errs.Wrap(errs.KindUnavailable, err, "git %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (s *Server) gitOutEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.gitBin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errs.Wrap(errs.KindUnavailable, err, "git %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func nameOf(ident string) string {
	if i := strings.Index(ident, " <"); i > 0 {
		return ident[:i]
	}
	return ident
}

func emailOf(ident string) string {
	if i := strings.Index(ident, "<"); i >= 0 {
		return strings.TrimSuffix(ident[i+1:], ">")
	}
	return ""
}

var _ = fmt.Sprintf
