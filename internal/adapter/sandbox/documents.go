package sandbox

import (
	"archive/tar"
	"bytes"
	"path"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// The project's documents, in the part that is the SAME in every substrate:
// validating the paths and turning the list into a tar.
//
// What differs — a ConfigMap on Kubernetes, a volume on Docker — stays in each
// adapter. What is here is what would be wrong to write twice.

// validateDocuments refuses a path that would land outside the mount.
//
// The list comes from a project's data, and a document named `../../etc/passwd`
// is not a hypothesis: it is what whoever names a file gets to choose. The check
// lives before the substrate because after it the damage is already the
// substrate's, and each one would fail in its own way.
func validateDocuments(files []ports.SandboxFile) error {
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		switch {
		case p == "":
			return errs.Invalid("document with no path")
		case strings.HasPrefix(p, "/"):
			return errs.Invalid("document with an absolute path: %q", f.Path)
		case p != path.Clean(p):
			// `./a`, `a//b` and `a/../b` all normalize to something else. What
			// is stored has to be what was asked for, or the audit trail and the
			// file stop being the same thing.
			return errs.Invalid("document with a non-normalized path: %q", f.Path)
		case p == ".." || strings.HasPrefix(p, "../"):
			return errs.Invalid("document with a path that escapes the mount: %q", f.Path)
		case seen[p]:
			return errs.Invalid("document repeated: %q", f.Path)
		}
		seen[p] = true
	}
	return nil
}

// documentsTar builds the archive, under `prefix`, with the entries owned by
// ROOT and with no write bit.
//
// That is how read-only is achieved on a substrate with no read-only volume for
// this (guarantee 19): the sandbox runs as an unprivileged user, so a file owned
// by root with mode 0444 cannot be written or chmodded from inside. It is the
// same outcome Kubernetes gets from a ConfigMap volume, by another road — which
// is the point of a contract suite testing the outcome and not the mechanism.
//
// The directories are not emitted as their own entries: `tar -x` and Docker's
// extractor both create the parent of a file. Emitting them would mean sorting
// them before their children, which is one more thing to get wrong for a gain of
// zero.
func documentsTar(files []ports.SandboxFile, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		mode := int64(0o444)
		if f.Executable {
			mode = 0o555
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: path.Join(prefix, path.Clean(f.Path)),
			Mode: mode,
			Uid:  0,
			Gid:  0,
			Size: int64(len(f.Content)),
		}); err != nil {
			return nil, errs.Wrap(errs.KindInternal, err, "failed to assemble the documents")
		}
		if _, err := tw.Write(f.Content); err != nil {
			return nil, errs.Wrap(errs.KindInternal, err, "failed to assemble the documents")
		}
	}
	if err := tw.Close(); err != nil {
		return nil, errs.Wrap(errs.KindInternal, err, "failed to close the documents")
	}
	return buf.Bytes(), nil
}
