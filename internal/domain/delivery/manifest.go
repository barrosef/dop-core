package delivery

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/barrosef/dop-core/internal/platform/errs"
	"gopkg.in/yaml.v3"
)

// ManifestPath is where a project declares how it is built and verified
// (ADR-0023). It is a constant of the DOMAIN and not configuration: a path each
// installation could change is a path no error message can name.
const ManifestPath = ".dop/verification.yml"

// Manifest is what the project says about itself: which third parties it needs,
// how to build it, how to start it, and what to run against it.
//
// **There is no discovery, and there is no default.** A missing manifest is a
// refusal that names the file (ParseManifest → KindPrecondition), never a guess
// at how somebody's application builds. A wrong guess does not fail loudly — it
// produces a green that proves nothing, or a red nobody can explain, which is
// the exact class of silent lie the runner exists to remove.
type Manifest struct {
	Dependencies []Dependency `yaml:"dependencies"`
	Build        string       `yaml:"build"`
	Start        StartSpec    `yaml:"start"`
	Checks       []Check      `yaml:"checks"`
	Cache        []string     `yaml:"cache"`
}

// Dependency is a third party the application needs — a database, a cache, a
// broker.
//
// It is ALWAYS a published image, pulled and never built. What is built from
// source is the application, and only it (ADR-0023 §1): a dependency built per
// run puts the slow path back exactly where the decision took it out.
type Dependency struct {
	Name  string            `yaml:"name"`
	Image string            `yaml:"image"`
	Port  int32             `yaml:"port"`
	Env   map[string]string `yaml:"env"`
	// Ready is an OPTIONAL command, run in the RUNNER, that has to succeed
	// before the application starts. Without it, readiness is the TCP port
	// accepting a connection — which is enough for most images and lies for a
	// few (Postgres accepts connections during recovery). The command lives on
	// the runner's side because that is the only place both executors agree
	// on: on Kubernetes the dependency is a container of the same pod and its
	// binaries are not ours to call.
	Ready string `yaml:"ready"`
	// Build exists ONLY to be refused. Without the field, `build:` under a
	// dependency would be silently ignored by the parser, and the author would
	// spend an afternoon wondering why their change had no effect. A field that
	// exists to produce a good error message is worth more than its absence.
	Build string `yaml:"build"`
}

// StartSpec is the application: the command that starts it and the port it
// answers on.
//
// It is optional. A project with only unit tests has nothing to start, and
// demanding a fake `start:` from it would be the tax pretending to be a rule.
// What is NOT optional is starting it for a dev session — see Manifest.Validate
// and RunSteps.
type StartSpec struct {
	Command string `yaml:"command"`
	Port    int32  `yaml:"port"`
}

func (s StartSpec) empty() bool { return strings.TrimSpace(s.Command) == "" }

// Check is one suite the runner executes against the started application.
type Check struct {
	Kind    string `yaml:"kind"`
	Command string `yaml:"command"`
}

// checkKinds maps the manifest's vocabulary to the domain's.
//
// The manifest speaks the WORKFLOW's words (ADR-0010 §1: a `test` stage has the
// subtypes `aaa`, `e2e`, `integration`), because that is what the person writing
// the file has already read. `aaa` and `unit` are the same thing under two
// names, and only one of them reaches the evidence: keeping both in the enum is
// how a projection ends up counting one run twice.
var checkKinds = map[string]CheckKind{
	"aaa":         CheckUnit,
	"unit":        CheckUnit,
	"e2e":         CheckE2E,
	"integration": CheckIntegration,
}

// dnsLabel is the stricter of the two executors' rules, applied to both.
//
// A dependency's name becomes a HOSTNAME — a container alias on Docker, and on
// Kubernetes nothing at all, because containers of one pod share `localhost`.
// The application's configuration has to work unchanged in both, so the name is
// held to what DNS accepts even where DNS is not involved.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ParseManifest reads the file's bytes. `found` says whether the project has one
// at all — the caller is what knows how to look in a repository, and the refusal
// for a project with no manifest belongs here so that every caller words it the
// same way.
func ParseManifest(content []byte, found bool) (*Manifest, error) {
	if !found {
		return nil, errs.Precondition(
			"this project does not declare %s: the platform does not guess how an application builds, "+
				"because a wrong guess produces a green that proves nothing (ADR-0023)", ManifestPath)
	}
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(content)))
	dec.KnownFields(true) // an unknown key is a typo, and a silently ignored typo is a bug
	if err := dec.Decode(&m); err != nil {
		return nil, errs.Invalid("%s is not readable: %v", ManifestPath, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate refuses a manifest that would produce a run nobody can trust.
func (m Manifest) Validate() error {
	seen := make(map[string]bool, len(m.Dependencies))
	for i, d := range m.Dependencies {
		where := d.Name
		if where == "" {
			where = fmt.Sprintf("#%d", i+1)
		}
		if strings.TrimSpace(d.Build) != "" {
			return errs.Invalid(
				"dependency %q declares `build`: a dependency is a published image, pulled and never built — "+
					"what is built from source is the application, and only it (ADR-0023)", where)
		}
		if !dnsLabel.MatchString(d.Name) {
			return errs.Invalid(
				"dependency %q has no usable name: it becomes a hostname, so it takes lowercase letters, "+
					"digits and hyphens", where)
		}
		if seen[d.Name] {
			return errs.Invalid("dependency %q declared twice", d.Name)
		}
		seen[d.Name] = true
		if strings.TrimSpace(d.Image) == "" {
			return errs.Invalid("dependency %q has no image", d.Name)
		}
		if d.Port <= 0 || d.Port > 65535 {
			return errs.Invalid(
				"dependency %q has no port: it is how the runner knows the dependency came up before "+
					"starting the application", d.Name)
		}
	}

	if len(m.Checks) == 0 && m.Start.empty() {
		return errs.Invalid(
			"%s declares neither `start` nor `checks`: there is nothing to run", ManifestPath)
	}
	if !m.Start.empty() && (m.Start.Port <= 0 || m.Start.Port > 65535) {
		return errs.Invalid("`start` has no port: without it nothing knows when the application came up")
	}
	for i, c := range m.Checks {
		if _, ok := checkKinds[c.Kind]; !ok {
			return errs.Invalid(
				"check #%d has an unknown kind %q: use aaa (or unit), e2e, integration", i+1, c.Kind)
		}
		if strings.TrimSpace(c.Command) == "" {
			return errs.Invalid("check #%d (%s) has no command", i+1, c.Kind)
		}
	}
	for _, p := range m.Cache {
		if !path.IsAbs(p) {
			return errs.Invalid(
				"cache path %q is not absolute: it is mounted inside the runner, where the working "+
					"directory is not the author's", p)
		}
	}
	return nil
}

// KindOf translates a check's declared kind into the evidence's vocabulary.
// Only call it on a validated manifest.
func (c Check) KindOf() CheckKind { return checkKinds[c.Kind] }

// SupportsSession answers whether a dev session can be opened on this project
// (verification-runner spec §4).
//
// A session is somebody wanting to click through the application. A project that
// declares no `start` has no application to click through, and saying so up
// front is better than raising an environment that answers nothing.
func (m Manifest) SupportsSession() bool { return !m.Start.empty() }
