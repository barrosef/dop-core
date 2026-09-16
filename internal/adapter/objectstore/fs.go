// An ObjectStore adapter over the local filesystem.
//
// Why it is a first-class adapter and not a test toy: it is what makes the
// self-hosted installation run with no GCS at all — one volume in the cluster is
// enough. It is also gcs.go's honest pair: two adapters over technologies with
// no kinship at all passing the SAME contract is what proves the port is an
// abstraction and not a facade over Google's SDK (ADR-0001).
//
// The key is OPAQUE and FLAT, as in GCS: "a/b" is a NAME that happens to have a
// slash, not a path. That is why the name goes escaped to disk instead of
// becoming a directory — three consequences the contract suite demands:
//   - "a/b" and "a/b/c" coexist (with a directory tree, one would be a folder
//     and the other a file, and the second write would fail);
//   - "../outside" is a literal key INSIDE the bucket, not an escape;
//   - a long key does not blow the 255-byte filename limit.
package objectstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"context"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// FS keeps each bucket in a directory under root, with content and metadata in
// sibling subtrees — never in the same directory, or else one key's metadata
// could collide with the content of another one called "x.meta".
type FS struct{ root string }

const (
	objectsDir  = "obj"
	metadataDir = "meta"

	// 255 is the filename limit on most systems; the rest of the budget is left
	// for the hash suffix.
	maxEncodedName = 200

	permDir  fs.FileMode = 0o700
	permFile fs.FileMode = 0o600
)

// NewFS prepares the root. A failure here, at boot, is better than a failure on
// the first upload — which is always far from the cause.
func NewFS(root string) (*FS, error) {
	if root == "" {
		return nil, errs.Invalid("no root provided for the file storage")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, errs.Wrap(errs.KindInvalid, err, "invalid root: %q", root)
	}
	if err := os.MkdirAll(abs, permDir); err != nil {
		return nil, errs.Wrap(errs.KindUnavailable, err, "could not create the root %q", abs)
	}
	return &FS{root: abs}, nil
}

// encode turns an opaque name into a safe and UNIQUE filename.
//
// url.PathEscape handles the slash, the percent and the space, but it does not
// handle "." and ".." (which need no escaping in a URL and are poison in a path)
// nor the length limit. In those two cases the hash steps in and injectivity
// still holds: two different names never collide on disk.
func encode(name string) string {
	e := url.PathEscape(name)
	if e != "." && e != ".." && len(e) <= maxEncodedName {
		return e
	}
	sum := sha256.Sum256([]byte(name))
	prefix := e
	if len(prefix) > maxEncodedName-40 {
		prefix = prefix[:maxEncodedName-40]
	}
	return prefix + "~" + hex.EncodeToString(sum[:])[:32]
}

func (f *FS) paths(ref ports.ObjectRef) (obj, meta string, err error) {
	if ref.Bucket == "" {
		return "", "", errs.Invalid("no bucket provided")
	}
	if ref.Key == "" {
		return "", "", errs.Invalid("no object key provided")
	}
	b := encode(ref.Bucket)
	k := encode(ref.Key)
	return filepath.Join(f.root, b, objectsDir, k),
		filepath.Join(f.root, b, metadataDir, k), nil
}

type metadata struct {
	ContentType string `json:"content_type"`
}

// writeAtomic writes through a temporary file + rename.
//
// A rename within the same directory is atomic on POSIX: a concurrent reader
// sees the old version OR the new one, never half an object. The port promises
// atomic replacement and this is where it is delivered — writing straight to the
// destination would truncate the file in front of whoever was reading.
func writeAtomic(destination string, content []byte) error {
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, permDir); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "could not prepare %q", dir)
	}
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to open a temporary file")
	}
	name := tmp.Name()
	defer os.Remove(name) // harmless once the rename has taken the file away

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return errs.Wrap(errs.KindUnavailable, err, "failed to write the object")
	}
	if err := tmp.Chmod(permFile); err != nil {
		tmp.Close()
		return errs.Wrap(errs.KindUnavailable, err, "failed to set the object's permissions")
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to close the object")
	}
	if err := os.Rename(name, destination); err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "failed to publish the object")
	}
	return nil
}

func (f *FS) Put(_ context.Context, ref ports.ObjectRef, content []byte, contentType string) error {
	obj, meta, err := f.paths(ref)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream" // the same choice as gcs.go
	}
	if err := writeAtomic(obj, content); err != nil {
		return err
	}
	m, _ := json.Marshal(metadata{ContentType: contentType})
	return writeAtomic(meta, m)
}

func (f *FS) Get(_ context.Context, ref ports.ObjectRef) ([]byte, error) {
	obj, _, err := f.paths(ref)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(obj)
	if err != nil {
		if missing(err) {
			return nil, errs.NotFound("object %s/%s", ref.Bucket, ref.Key)
		}
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to read the object")
	}
	return b, nil
}

func (f *FS) Delete(_ context.Context, ref ports.ObjectRef) error {
	obj, meta, err := f.paths(ref)
	if err != nil {
		return err
	}
	// Idempotent: removing what does not exist is a success, as in GCS.
	for _, p := range []string{obj, meta} {
		if err := os.Remove(p); err != nil && !missing(err) {
			return errs.Wrap(errs.KindUnavailable, err, "failed to remove the object")
		}
	}
	return nil
}

func (f *FS) Stat(_ context.Context, ref ports.ObjectRef) (*ports.ObjectMeta, error) {
	obj, meta, err := f.paths(ref)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(obj)
	if err != nil || fi.IsDir() {
		if err == nil || missing(err) {
			return nil, errs.NotFound("object %s/%s", ref.Bucket, ref.Key)
		}
		return nil, errs.Wrap(errs.KindUnavailable, err, "failed to stat the object")
	}
	out := &ports.ObjectMeta{
		Size:      fi.Size(),
		UpdatedAt: fi.ModTime().UTC(),
		// Lost metadata (a crash between the two writes) does not invalidate the
		// object: it falls back to the same default Put uses when nobody
		// provides a type.
		ContentType: "application/octet-stream",
	}
	if b, err := os.ReadFile(meta); err == nil {
		var m metadata
		if json.Unmarshal(b, &m) == nil && m.ContentType != "" {
			out.ContentType = m.ContentType
		}
	}
	return out, nil
}

// SignedPutURL and SignedGetURL do not exist here, and silence would be worse.
//
// A signed URL is an upload that does not go through the BFF; with file storage
// there is no endpoint to sign. Returning "file:///..." would be lying to the
// caller, who would hand the browser a URL nobody can use. KindUnavailable is
// the honest answer and makes the caller fall back to uploading through the BFF
// — which is the correct path in self-hosted. The port documents that
// alternative and the contract suite demands it of both adapters.
func (f *FS) SignedPutURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable,
		"file storage issues no signed URL; the upload goes through the BFF")
}

func (f *FS) SignedGetURL(context.Context, ports.ObjectRef, time.Duration) (string, error) {
	return "", errs.New(errs.KindUnavailable,
		"file storage issues no signed URL; the read goes through the BFF")
}

func missing(err error) bool { return errors.Is(err, fs.ErrNotExist) }

var _ ports.ObjectStore = (*FS)(nil)
