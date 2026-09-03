package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/agentmetrics"
	"github.com/Digital-Business-One/dop-core/internal/platform/callauth"
	"github.com/Digital-Business-One/dop-core/internal/platform/config"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
	"github.com/Digital-Business-One/dop-core/internal/platform/logging"
)

// The COLLECTOR: the fifth mode of the binary, and the only one that does not
// run in the platform's namespace.
//
// It lives BESIDE the agent, in the sandbox's pod, and does one thing: follows
// the session file Claude Code writes and pushes what appears to the core. It is
// a container of ours, not the agent's — which is what makes it acceptable for
// it to hold a credential at all. The agent's container never sees the key: the
// Secret is mounted only here.
//
// ── Why a sidecar and not the core reading the file ─────────────────────────
//
// The core could pull it through `Exec`. Two things make pushing better: the
// port's exec caps each stream at 64 KiB while a session file reaches tens of
// megabytes, so pulling means a chunked read with a cursor race; and pulling is
// polling, which is either late or wasteful. Pushing is live, and "live" is the
// requirement — the cockpit shows consumption while the demand runs.
//
// ── What it must never become ───────────────────────────────────────────────
//
// It talks to the CORE, never to the database. Whoever reaches Postgres does not
// even need to forge an actor: they read everything. The collector's key opens
// exactly one service (ADR-0029), so a leak here is worth polluting telemetry.
func RunCollector(ctx context.Context, cfg *config.Config) error {
	log := logging.From(ctx)
	if strings.TrimSpace(cfg.CollectorSessionDir) == "" {
		return errs.Invalid("the collector has no session directory to follow")
	}
	if strings.TrimSpace(cfg.CallAuthKeyCollector) == "" {
		return errs.Invalid("the collector has no key to sign its calls with")
	}
	if strings.TrimSpace(cfg.CollectorAccountID) == "" {
		return errs.Invalid("the collector does not know which account it is collecting for")
	}

	conn, err := grpc.NewClient(cfg.CoreTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return errs.Wrap(errs.KindUnavailable, err, "the collector could not reach the core")
	}
	defer conn.Close()
	client := dopv1.NewAgentMetricsServiceClient(conn)

	c := &collector{
		cfg: cfg, client: client, log: log,
		offsets: map[string]int64{},
	}
	log.Info("collector following", "dir", cfg.CollectorSessionDir,
		"demand", cfg.CollectorDemandID, "interval", cfg.CollectorInterval.String())

	ticker := time.NewTicker(cfg.CollectorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// One last pass: a demand that ends between two ticks would lose its
			// tail, which is exactly the part that says how it finished.
			c.sweep(context.WithoutCancel(ctx))
			return ctx.Err()
		case <-ticker.C:
			c.sweep(ctx)
		}
	}
}

type collector struct {
	cfg    *config.Config
	client dopv1.AgentMetricsServiceClient
	log    *slog.Logger
	// offsets is the cursor per file, in memory. The core holds the durable one
	// and hands it back on the first push of a session, so a restarted collector
	// does not re-send a whole file.
	offsets map[string]int64
}

// sweep reads what appeared in every session file and pushes it.
//
// A failure on one file does not stop the others, and no failure stops the loop:
// the collector is telemetry, and telemetry that takes the demand down with it
// is worse than no telemetry.
func (c *collector) sweep(ctx context.Context) {
	files, err := filepath.Glob(filepath.Join(c.cfg.CollectorSessionDir, "*.jsonl"))
	if err != nil {
		c.log.Warn("could not list the sessions", "error", err.Error())
		return
	}
	sort.Strings(files)
	for _, path := range files {
		if err := c.follow(ctx, path); err != nil {
			c.log.Warn("session not collected", "file", filepath.Base(path), "error", err.Error())
		}
	}
}

func (c *collector) follow(ctx context.Context, path string) error {
	from := c.offsets[path]
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() <= from {
		return nil // nothing new
	}
	// A file that SHRANK was replaced: start over rather than read from the
	// middle of a record.
	if info.Size() < from {
		from = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	chunk, err := io.ReadAll(io.LimitReader(f, maxChunkBytes))
	if err != nil {
		return err
	}

	session, turns := agentmetrics.Parse(chunk, from)
	if session.ExternalID == "" && len(turns) == 0 {
		return nil
	}

	req := &dopv1.RecordTurnsRequest{
		Session:     session.ExternalID,
		DemandId:    c.cfg.CollectorDemandID,
		ProjectId:   c.cfg.CollectorProjectID,
		Cwd:         session.CWD,
		GitBranch:   session.GitBranch,
		ToolVersion: session.ToolVersion,
		ByteOffset:  session.ByteOffset,
		Turns:       turnsToProto(turns),
	}
	resp, err := c.client.RecordTurns(c.signed(ctx), req)
	if err != nil {
		return err
	}
	// The cursor moves only after the core ACCEPTED the batch. Moving it first
	// would lose a stretch on the first network hiccup, silently.
	c.offsets[path] = session.ByteOffset
	if resp.GetRecorded() > 0 {
		c.log.Info("turns collected", "turns", resp.GetRecorded(),
			"session", session.ExternalID, "offset", session.ByteOffset)
	}
	return nil
}

// maxChunkBytes bounds one pass. A collector starting on a session that has been
// running for hours would otherwise read tens of megabytes into one request; it
// catches up over the next ticks instead.
const maxChunkBytes = 2 << 20

// signed puts the assertion on the call (ADR-0029). The collector has no person
// behind it — the assertion is the only proof it can offer, and its caller name
// is what the core authorizes on.
func (c *collector) signed(ctx context.Context) context.Context {
	raw, err := callauth.Sign([]byte(c.cfg.CallAuthKeyCollector), callauth.Assertion{
		Caller:    agentmetrics.CollectorCaller,
		ActorKind: "system",
		AccountID: c.cfg.CollectorAccountID,
		ExpiresAt: time.Now().Add(2 * time.Minute),
	})
	if err != nil {
		c.log.Warn("could not sign the call", "error", err.Error())
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		"x-dop-assertion", raw,
		"x-account-id", c.cfg.CollectorAccountID)
}

func mustJSONString(v map[string]any) string {
	if len(v) == 0 {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func turnsToProto(turns []agentmetrics.Turn) []*dopv1.AgentTurn {
	out := make([]*dopv1.AgentTurn, 0, len(turns))
	for _, t := range turns {
		pt := &dopv1.AgentTurn{
			Uuid: t.UUID, ParentUuid: t.ParentUUID, Model: t.Model,
			ServiceTier: t.ServiceTier, StopReason: t.StopReason, Sidechain: t.Sidechain,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
			CacheCreationTokens: t.CacheCreationTokens, CacheReadTokens: t.CacheReadTokens,
			ToolUses: int32(t.ToolUses), Thinking: int32(t.Thinking), Texts: int32(t.Texts),
			Tools: t.Tools, RawUsage: mustJSONString(t.RawUsage),
		}
		if !t.OccurredAt.IsZero() {
			pt.OccurredAt = timestamppb.New(t.OccurredAt)
		}
		out = append(out, pt)
	}
	return out
}
