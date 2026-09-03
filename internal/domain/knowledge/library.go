// The LIBRARY: the shape in which the project's knowledge reaches the agent.
//
// It is the contract between the platform and the agent about WHERE things are,
// and it exists because of one requirement: at startup the content is simply
// there. The agent does not fetch, does not download and does not ask — from its
// point of view the library always existed at that path. Everything that travels
// travels before the agent exists.
//
// ── Why a tree and not one file ─────────────────────────────────────────────
//
// A single concatenated file would be simpler to produce and worse to use: the
// agent would have to read everything to find one thing, and reading everything
// is exactly the cost context engineering exists to avoid. A tree with a
// manifest lets the agent open the manifest — small, always read — and then only
// what the task asks for.
//
// That is also why the manifest carries SIZES: choosing what to open is a
// decision about budget (ADR-0011/0012), and a decision made without the numbers
// is a guess.
package knowledge

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// The library's directories. They are a CONTRACT: the agent's brief names these
// paths, so renaming one here without renaming it there leaves the agent
// looking in a place that no longer exists.
const (
	// LibraryManifest is the first thing the agent reads. Its name is
	// `README.md` on purpose — it is the name every convention in the world
	// already points at, and the agent does not need to be taught it.
	LibraryManifest = "README.md"

	// LibraryRules holds what the agent OBEYS. First in the manifest because a
	// rule that arrived after the decision arrived late.
	LibraryRules = "rules"
	// LibraryIndex holds one map per repository: what lives where, how to
	// build, how to test.
	LibraryIndex = "index"
	// LibraryMemory holds findings and lessons from past demands.
	LibraryMemory = "memory"
	// LibraryDemand holds what belongs to THIS demand — spec, plan, context.
	// It is under the same root because from the agent's side there is one
	// library, not two; the separation is in the directory, where it is
	// visible, and not in the mount, where it would be one more thing to
	// explain.
	LibraryDemand = "demand"
)

// Document is one file of the library, before it becomes a file.
//
// It is not `Artifact`: an artifact is a row with version, scope and embedding,
// and none of that crosses into the sandbox. What crosses is a name, a body and
// enough about the origin for the manifest to be worth reading.
type Document struct {
	// Section is one of the Library* directories.
	Section string
	// Name is the file's, without a directory. It comes from the artifact's
	// name and is normalized here: what names a file cannot depend on somebody
	// having typed a well-behaved title.
	Name string
	// Title is what the manifest shows — the original, unnormalized.
	Title string
	// Body is the content.
	Body string
	// Origin says where it came from, for the manifest: "the account's rule",
	// "the project's rule". A rule inherited from the account and one written
	// for the project read the same and mean different things.
	Origin string
}

// Path is where the document lands inside the mount.
func (d Document) Path() string { return path.Join(d.Section, d.Name) }

// FileName turns a title into a file name that survives a filesystem.
//
// A title is text somebody wrote: it has spaces, accents, slashes and colons. A
// slash would create a directory nobody asked for, and that is the one that
// actually breaks — the others are only ugly.
func FileName(title string) string {
	clean := strings.ToLower(strings.TrimSpace(title))
	// A `.md` already there is not part of the name to be slugged, or `spec.md`
	// would come out as `spec-md.md`.
	clean = strings.TrimSuffix(clean, ".md")

	var b strings.Builder
	prevDash := false
	for _, r := range clean {
		if base, ok := unaccented[r]; ok {
			r = base
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '.' || r == '_' || r == '-' || r == ' ' || r == '/':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
		// Anything else — a colon, an emoji — is dropped. The title itself
		// survives whole in the manifest.
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		// A title made only of characters that do not survive is rare and real.
		// A file with no name would be worse than one with an opaque name.
		name = "untitled"
	}
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	return name
}

// unaccented folds the accents this platform actually sees.
//
// It is not a Unicode normalization library, and it does not need to be:
// dropping the accent turns "padrões" into "padres", which is a different word
// and a worse file name. Folding turns it into "padroes", which anybody
// recognizes. Adding `golang.org/x/text` to normalize the whole of Unicode would
// be a dependency for a table of thirty runes.
var unaccented = map[rune]rune{
	'á': 'a', 'à': 'a', 'ã': 'a', 'â': 'a', 'ä': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'õ': 'o', 'ô': 'o', 'ö': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'ç': 'c', 'ñ': 'n',
}

// Library assembles the documents into the files that will be mounted, with the
// manifest at the front.
//
// It DEDUPLICATES by path: two rules with the same title, one from the account
// and one from the project, would land on the same file and the second would
// silently win. Here the second gets a suffix, and the manifest says where each
// came from — the inheritance is visible instead of being resolved by accident.
func Library(docs []Document) []ports.SandboxFile {
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].Section != docs[j].Section {
			return sectionOrder(docs[i].Section) < sectionOrder(docs[j].Section)
		}
		return docs[i].Name < docs[j].Name
	})

	taken := make(map[string]int, len(docs))
	files := make([]ports.SandboxFile, 0, len(docs)+1)
	placed := make([]Document, 0, len(docs))
	for _, d := range docs {
		p := d.Path()
		if n := taken[p]; n > 0 {
			ext := path.Ext(d.Name)
			d.Name = strings.TrimSuffix(d.Name, ext) + fmt.Sprintf("-%d", n+1) + ext
			p = d.Path()
		}
		taken[d.Path()]++
		placed = append(placed, d)
		files = append(files, ports.SandboxFile{Path: p, Content: []byte(d.Body)})
	}

	manifest := Manifest(placed)
	return append([]ports.SandboxFile{
		{Path: LibraryManifest, Content: []byte(manifest)},
	}, files...)
}

// sectionOrder is the order the sections appear in the manifest, and it is not
// alphabetical: it is the order in which they matter to whoever is about to
// work. Rules first because they constrain everything after.
func sectionOrder(section string) int {
	switch section {
	case LibraryRules:
		return 0
	case LibraryDemand:
		return 1
	case LibraryIndex:
		return 2
	case LibraryMemory:
		return 3
	}
	return 4
}

// Manifest is the text the agent reads first.
//
// It is written FOR a model: no decoration, one line per document, the size in
// front so that choosing what to open is a decision with a number behind it. The
// header explains the sections in one line each, because an agent that arrives
// at a directory called `memory` and does not know what it holds either ignores
// it or reads all of it, and both are wrong.
func Manifest(docs []Document) string {
	var b strings.Builder
	b.WriteString("# The project's library\n\n")
	b.WriteString("Everything here was available before you started. Nothing needs to be fetched.\n\n")
	b.WriteString("- `" + LibraryRules + "/` — conventions this project OBEYS. Read them before deciding anything.\n")
	b.WriteString("- `" + LibraryDemand + "/` — what belongs to the demand you are working on: spec, plan, context.\n")
	b.WriteString("- `" + LibraryIndex + "/` — one map per repository: what lives where, how to build, how to test.\n")
	b.WriteString("- `" + LibraryMemory + "/` — findings and lessons from past demands. Consult them; they are not orders.\n")

	current := ""
	for _, d := range docs {
		if d.Section != current {
			current = d.Section
			b.WriteString("\n## " + current + "\n\n")
		}
		line := fmt.Sprintf("- `%s` (%s) — %s", d.Path(), humanSize(len(d.Body)), d.Title)
		if d.Origin != "" {
			line += " [" + d.Origin + "]"
		}
		b.WriteString(line + "\n")
	}
	if len(docs) == 0 {
		b.WriteString("\nThis project has no documents recorded yet.\n")
	}
	return b.String()
}

func humanSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
