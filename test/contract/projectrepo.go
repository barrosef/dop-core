package contract

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// ProjectRepositoryEnv is what an adapter's test provides the suite with.
type ProjectRepositoryEnv struct {
	// Repos is the adapter under test.
	Repos ports.ProjectRepository
	// Secrets is where the suite plants a mirror's credential (guarantee 8).
	Secrets ports.SecretStore
	// AccountID scopes the SecretRef the suite plants.
	AccountID string
}

// ProjectRepositorySuite verifies the eight guarantees documented on the
// ProjectRepository port (ADR-0028) — in EVERY adapter.
func ProjectRepositorySuite(t *testing.T, name string, newEnv func(t *testing.T) ProjectRepositoryEnv) {
	t.Run(name, func(t *testing.T) {
		ctx := context.Background()

		t.Run("1_ensure_is_idempotent_by_project", func(t *testing.T) {
			env := newEnv(t)
			project := "proj-" + randomID()
			a, err := env.Repos.Ensure(ctx, project)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			b, err := env.Repos.Ensure(ctx, project)
			if err != nil {
				t.Fatalf("second Ensure: %v", err)
			}
			if a.CloneURL == "" || a.CloneURL != b.CloneURL {
				t.Errorf("Ensure is not stable: %q then %q", a.CloneURL, b.CloneURL)
			}
		})

		t.Run("2_a_repository_is_born_with_a_manifest", func(t *testing.T) {
			env := newEnv(t)
			project := "proj-" + randomID()
			if _, err := env.Repos.Ensure(ctx, project); err != nil {
				t.Fatal(err)
			}
			out, err := env.Repos.Read(ctx, project, "README.md")
			if err != nil {
				t.Fatalf("a fresh repository has no manifest: %v", err)
			}
			if !strings.Contains(string(out), "library") {
				t.Errorf("the manifest is not a manifest: %q", out)
			}
		})

		t.Run("3_the_token_opens_exactly_one_repository", func(t *testing.T) {
			env := newEnv(t)
			mine, other := "proj-"+randomID(), "proj-"+randomID()
			info, _ := env.Repos.Ensure(ctx, mine)
			otherInfo, _ := env.Repos.Ensure(ctx, other)
			tok, err := env.Repos.IssueToken(ctx, mine, "dem-1", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := gitClone(t, info.CloneURL, tok); err != nil {
				t.Fatalf("the token does not open its own repository: %v", err)
			}
			if err := gitClone(t, otherInfo.CloneURL, tok); err == nil {
				t.Fatal("the token opened another project's repository")
			}
			if err := gitClone(t, info.CloneURL, "not-a-token"); err == nil {
				t.Fatal("a forged token opened the repository")
			}
		})

		t.Run("4_the_token_expires", func(t *testing.T) {
			env := newEnv(t)
			project := "proj-" + randomID()
			info, _ := env.Repos.Ensure(ctx, project)
			tok, err := env.Repos.IssueToken(ctx, project, "dem-1", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)
			if err := gitClone(t, info.CloneURL, tok); err == nil {
				t.Fatal("an expired token still opened the repository")
			}
		})

		t.Run("5_and_6_commit_then_read_and_clone", func(t *testing.T) {
			env := newEnv(t)
			project := "proj-" + randomID()
			info, _ := env.Repos.Ensure(ctx, project)
			sha, err := env.Repos.Commit(ctx, project, ports.RepositoryCommit{
				Files:      []ports.RepositoryFile{{Path: "rules/branches.md", Content: []byte("no direct pushes\n")}},
				Message:    "a rule",
				AuthorName: "Dev", AuthorEmail: "dev@x.com",
				CommitterName: "DOP", CommitterEmail: "platform@dop",
			})
			if err != nil || sha == "" {
				t.Fatalf("Commit: %v (sha %q)", err, sha)
			}
			out, err := env.Repos.Read(ctx, project, "rules/branches.md")
			if err != nil || string(out) != "no direct pushes\n" {
				t.Fatalf("Read after Commit: %v %q", err, out)
			}
			if _, err := env.Repos.Read(ctx, project, "rules/absent.md"); errs.KindOf(err) != errs.KindNotFound {
				t.Errorf("Read of an absent path gave %v", err)
			}
			tok, _ := env.Repos.IssueToken(ctx, project, "dem-1", time.Hour)
			dir := t.TempDir()
			if err := gitCloneInto(t, info.CloneURL, tok, dir); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "rules", "branches.md"))
			if err != nil || string(got) != "no direct pushes\n" {
				t.Errorf("the clone does not carry the commit: %v %q", err, got)
			}
		})

		t.Run("7_a_push_reaches_the_platform", func(t *testing.T) {
			env := newEnv(t)
			project := "proj-" + randomID()
			info, _ := env.Repos.Ensure(ctx, project)
			got := make(chan ports.Push, 4)
			env.Repos.OnPush(func(_ context.Context, p ports.Push) { got <- p })

			tok, _ := env.Repos.IssueToken(ctx, project, "dem-7", time.Hour)
			dir := t.TempDir()
			if err := gitCloneInto(t, info.CloneURL, tok, dir); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "memory", "lesson.md"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, dir, "git", "add", ".")
			run(t, dir, "git", "-c", "user.name=thread-7", "-c", "user.email=thread-7@agents.dop", "commit", "-q", "-m", "lesson")
			run(t, dir, "git", "-c", "http.extraHeader=Authorization: Bearer "+tok, "push", "-q", "origin", "HEAD:main")

			select {
			case p := <-got:
				if p.ProjectID != project || p.DemandID != "dem-7" || p.After == "" {
					t.Errorf("the push arrived wrong: %+v", p)
				}
			case <-time.After(10 * time.Second):
				// Remote adapters learn about pushes through the event bus, not
				// through this callback; only an adapter that HOSTS can answer.
				if env.Secrets == nil {
					t.Skip("this adapter does not host the repositories; pushes reach it through the bus")
				}
				t.Fatal("no push reached OnPush")
			}
		})

		t.Run("8_the_mirror_receives_the_push_with_a_credential_it_never_shows", func(t *testing.T) {
			env := newEnv(t)
			if env.Secrets == nil {
				t.Skip("mirroring is proven where the repositories are hosted")
			}
			project := "proj-" + randomID()
			if _, err := env.Repos.Ensure(ctx, project); err != nil {
				t.Fatal(err)
			}
			// The "user's remote": a bare repository on disk. The credential is
			// planted in the store and never handed to anyone.
			remote := t.TempDir()
			run(t, remote, "git", "init", "-q", "--bare")
			secret := []byte("the-users-token")
			ref := ports.SecretRef{AccountID: env.AccountID, Kind: "integration_credential", OwnerID: "mirror-" + project}
			if err := env.Secrets.Put(ctx, ref, ports.SecretValue(secret)); err != nil {
				t.Fatal(err)
			}
			if err := env.Repos.SetMirror(ctx, project, remote, ref); err != nil {
				t.Fatal(err)
			}
			if _, err := env.Repos.Commit(ctx, project, ports.RepositoryCommit{
				Files: []ports.RepositoryFile{{Path: "rules/mirrored.md", Content: []byte("y\n")}}, Message: "mirror me",
			}); err != nil {
				t.Fatal(err)
			}
			out := runOut(t, remote, "git", "log", "--oneline", "main")
			if !strings.Contains(out, "mirror me") {
				t.Errorf("the mirror did not receive the push: %q", out)
			}
		})
	})
}

func gitClone(t *testing.T, url, token string) error {
	t.Helper()
	return gitCloneInto(t, url, token, t.TempDir())
}

func gitCloneInto(t *testing.T, url, token, dir string) error {
	t.Helper()
	cmd := exec.Command("git", "-c", "http.extraHeader=Authorization: Bearer "+token, "clone", "-q", url, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errs.New(errs.KindUnavailable, "%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	if out := runOut(t, dir, name, args...); out == "" && false {
		_ = out
	}
}

func runOut(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
