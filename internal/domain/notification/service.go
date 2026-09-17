package notification

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
	"github.com/barrosef/dop-core/internal/platform/logging"
)

// Config is the INSTALLATION's tuning, not domain vocabulary.
type Config struct {
	// BaseURL is this installation's cockpit address. Empty makes the notice go
	// out with no link — a declared degradation, not an oversight: a link to
	// "/attention" with no base is a broken link, and a broken link in an email
	// costs more trust than its absence.
	BaseURL string
	// DigestDelay overrides DefaultDigestDelay. See there for why it exists and
	// why it is a guess.
	DigestDelay time.Duration
	// MaxAttempts bounds the retry of what failed. Zero uses DefaultMaxAttempts.
	MaxAttempts int
	// DigestLimit is the ceiling of items ONE email carries. It exists because a
	// digest of two hundred items is not a digest — it is the attention box badly
	// printed, and nobody reads it.
	DigestLimit int
}

// DefaultDigestLimit is how many items fit in a digest before it becomes noise.
const DefaultDigestLimit = 20

// Service is the EXECUTOR: it takes the command the decider produced, claims the
// key, dispatches through the channel and records the outcome.
//
// It decides nothing — the entire decision lives in the table (rules.go). That
// separation is what P-29 will demand: when reaction becomes data, the decider
// is swapped and this file does not change.
type Service struct {
	repo   Repository
	mailer ports.Mailer
	clock  ports.Clock
	cfg    Config
}

func NewService(repo Repository, mailer ports.Mailer, clock ports.Clock, cfg Config) *Service {
	if repo == nil || mailer == nil {
		panic("notification.NewService: repository and mailer are required")
	}
	// The clock is a PORT and nil is refused, as in every service in this house:
	// accepting nil and falling back to time.Now() switches the abstraction off
	// without anyone noticing and hands the test back its wall-clock dependency.
	// And here it would be worse than usual — the digest's delay is measured BY
	// it, and a test that does not control the clock cannot prove that an item
	// resolved in five minutes never became an email.
	if clock == nil {
		panic("notification.NewService: clock is required — use clock.NewSystem()")
	}
	if cfg.DigestDelay <= 0 {
		cfg.DigestDelay = DefaultDigestDelay
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.DigestLimit <= 0 {
		cfg.DigestLimit = DefaultDigestLimit
	}
	return &Service{repo: repo, mailer: mailer, clock: clock, cfg: cfg}
}

// ── the transactional trigger: consumer of the event spine ──────────────────

// HandleEvent is the CONSUMER. It runs in the worker, next to the timeline and
// the attention box (ADR-0018: "it runs in the core").
//
// It is idempotent because it has to be: JetStream delivery is at-least-once
// (ADR-0014) and a duplicate email has no undo. The idempotency is not a check
// at the top — it is the CLAIM, which is atomic in the database.
func (s *Service) HandleEvent(ctx context.Context, e Event) error {
	cmds := Apply(e, func(spec recipientSpec, ev Event) []Recipient {
		return s.resolveRecipients(ctx, spec, ev)
	})
	for _, c := range cmds {
		if !c.Valid() {
			continue
		}
		if err := s.execute(ctx, c); err != nil {
			// Propagate the error: the caller is the bus, and redelivery is what
			// gives a second chance to what failed. The claim is already in
			// StateError, and only that is resumable — redelivery does not become
			// a duplicate email.
			return err
		}
	}
	return nil
}

// resolveRecipients turns the origin declared in the rule into real addresses.
// It is the only part of the decision that needs I/O, and that is why it lives
// here and not in the table: a table that runs a query stops being data.
func (s *Service) resolveRecipients(ctx context.Context, spec recipientSpec, e Event) []Recipient {
	switch spec.Source {
	case fromPayload:
		return payloadEmail(spec, e)
	case fromAccountMembers:
		dest, err := s.repo.Recipients(ctx, e.AccountID)
		if err != nil {
			// A failure to resolve recipients must not become a command with no
			// recipient: that would be the same output as "there is nobody to
			// notify", and the two situations demand opposite responses.
			logging.From(ctx).Error("failed to resolve the account recipients",
				logging.FieldError, err.Error())
			return nil
		}
		return dest
	}
	return nil
}

// ── the attention notice: a per-minute sweep, with no scheduler ─────────────

// SweepDigest is the delayed attention notice, running as SYSTEM.
//
// ── How the delay is implemented without inventing a scheduler ──────────────
//
// There is no timer, no future work queue and nothing to cancel when the item
// resolves. The scheduler already runs minute by minute (`RunScheduledTasks`),
// and the delay becomes a QUERY PREDICATE: "items STILL OPEN, opened before
// now−delay, that have not yet become a notice".
//
// An item resolved before the cutoff simply never enters the result — and that
// is exactly the behaviour the ADR asks for ("whoever was in the cockpit has
// already handled it"). A scheduler would give the same result with one more
// piece, one more state and one more cancellation to get wrong; and cancellation
// is where that kind of design fails, because it is the part that only runs in
// the rare case.
//
// The price is honest: the notice goes out with minute granularity, and a
// 15-minute delay becomes something between 15 and 16. For a digest whose whole
// purpose is to wait for the trivial to resolve, that is not an error — it is
// the unit itself.
func (s *Service) SweepDigest(ctx context.Context) (accounts, notices int, err error) {
	rule, ok := DigestRule()
	if !ok {
		// With no digest rule in the table there is no sweep. Not an error: it is
		// the policy saying the box generates no email in this installation.
		return 0, 0, nil
	}
	delay := rule.Delay
	if s.cfg.DigestDelay > 0 {
		delay = s.cfg.DigestDelay
	}
	if delay <= 0 {
		delay = DefaultDigestDelay
	}
	cutoff := s.clock.Now().Add(-delay)

	ids, err := s.repo.AccountsWithRipeAttention(ctx, rule.Name, rule.Action, cutoff, s.cfg.MaxAttempts)
	if err != nil {
		return 0, 0, err
	}
	for _, accountID := range ids {
		// A SYSTEM actor with the account at hand — the same Call the interceptor
		// would build for a user request. It is what keeps the per-account filter
		// in force here too, with no exception carved out in the domain.
		perAccount := ctxutil.Into(ctx, ctxutil.Call{
			AccountID: accountID,
			ActorID:   "scheduler",
			ActorKind: ctxutil.ActorSystem,
			ActorName: "notifier",
		})
		n, err := s.accountDigest(perAccount, rule, accountID, cutoff)
		if err != nil {
			// An error in one account does not interrupt the others: one account's
			// notice must not be held up because the previous account has a
			// problem.
			logging.From(ctx).Error("the box digest failed for this account",
				"account_id", accountID, logging.FieldError, err.Error())
			continue
		}
		accounts++
		notices += n
	}
	return accounts, notices, nil
}

func (s *Service) accountDigest(ctx context.Context, rule Rule, accountID string, cutoff time.Time) (int, error) {
	if _, err := ctxutil.MustAccount(ctx); err != nil {
		return 0, err
	}
	items, err := s.repo.RipeAttention(ctx, accountID, rule.Name, rule.Action, cutoff,
		s.cfg.MaxAttempts, s.cfg.DigestLimit)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	dest, err := s.repo.Recipients(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if len(dest) == 0 {
		// Nobody with a known address. It does NOT claim: claiming here would
		// record "notified" for items nobody saw, and the day a member gained an
		// address they would start already in debt to their own box.
		return 0, nil
	}

	// One claim PER ITEM — the key is (event, rule, action) and the event is what
	// OPENED the item. Grouping into one message comes later; idempotency is not
	// grouped, otherwise a new item would reopen the old ones.
	addresses := emails(dest)
	covered := make([]DeliveryKey, 0, len(items))
	taken := make([]AttentionNotice, 0, len(items))
	for _, it := range items {
		key := DeliveryKey{EventID: it.EventID, Rule: rule.Name, Action: rule.Action}
		ok, err := s.repo.Claim(ctx, Claim{
			DeliveryKey: key, AccountID: accountID, Kind: rule.Kind,
			Channel: string(rule.Action), Recipients: addresses,
		}, s.cfg.MaxAttempts)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue // already notified (or attempts exhausted)
		}
		covered = append(covered, key)
		taken = append(taken, it)
	}
	if len(covered) == 0 {
		return 0, nil
	}

	data := map[string]any{
		"account_id": accountID,
		"total":      len(taken),
		"items":      itemsToData(taken),
	}
	// It goes through the SAME resolver as the other trigger, even though the
	// digest rule has no placeholder today: two ways of building a link is how
	// one of them falls behind when the table changes.
	data["link"] = s.link(resolvePath(rule.LinkPath, data))
	receipt, sendErr := s.send(ctx, accountID, rule.Kind, dest, data)
	if _, err := s.repo.Settle(ctx, outcomeOf(accountID, covered, rule.Kind, addresses, receipt, sendErr)); err != nil {
		return 0, err
	}
	if sendErr != nil {
		return 0, sendErr
	}
	return len(covered), nil
}

// ── execution shared by both triggers ───────────────────────────────────────

// execute is claim, dispatch, settle — in that order and with no shortcut.
func (s *Service) execute(ctx context.Context, c Command) error {
	key := DeliveryKey{EventID: c.EventID, Rule: c.Rule, Action: c.Action}
	ok, err := s.repo.Claim(ctx, Claim{
		DeliveryKey: key, AccountID: c.AccountID, Kind: c.Kind,
		Channel: string(c.Action), Recipients: emails(c.Recipients),
	}, s.cfg.MaxAttempts)
	if err != nil {
		return err
	}
	if !ok {
		// A redelivery of the same event, or attempts exhausted. Silence here is
		// correct: the record already tells the story, and returning an error
		// would make the bus redeliver forever a message that was already
		// served.
		logging.From(ctx).Debug("notice already recorded, nothing to do",
			"rule", c.Rule, "action", string(c.Action), "kind", string(c.Kind))
		return nil
	}

	data := map[string]any{"account_id": c.AccountID}
	for k, v := range c.Data {
		data[k] = v
	}
	// The link resolves AFTER `data` is complete: `/invites/{invite_id}` needs the
	// event's payload, which only exists here.
	data["link"] = s.linkOfRule(c.Rule, data)

	receipt, sendErr := s.send(ctx, c.AccountID, c.Kind, c.Recipients, data)
	if _, err := s.repo.Settle(ctx, outcomeOf(c.AccountID, []DeliveryKey{key}, c.Kind,
		emails(c.Recipients), receipt, sendErr)); err != nil {
		return err
	}
	return sendErr
}

// send dispatches ONE message per recipient.
//
// One per recipient, and not one with everybody in copy, because copy hands the
// account's address list to every member — and an invite has a recipient who is
// not even part of it yet.
//
// The first error stops the loop: if the provider is down, insisting on the
// other nine only multiplies the time until the row reaches StateError, and the
// resume resends all of them anyway.
func (s *Service) send(ctx context.Context, accountID string, kind Kind, dest []Recipient, data map[string]any) (*ports.MailReceipt, error) {
	var last *ports.MailReceipt
	for _, r := range dest {
		rec, err := s.mailer.Send(ctx, ports.Mail{
			AccountID: accountID,
			Kind:      string(kind),
			To:        r.Email,
			ToName:    r.Name,
			Data:      data,
		})
		if err != nil {
			return last, err
		}
		last = rec
	}
	if last == nil {
		return nil, errs.Invalid("notice with no recipient")
	}
	return last, nil
}

// outcomeOf assembles the record from what the channel returned.
//
// A send error becomes StateError with the adapter's message — which already
// arrives REDACTED (the port's guarantee 4). The domain redacts nothing, because
// it does not know what the secret is; if it did, the secret would be in the
// wrong place.
func outcomeOf(accountID string, keys []DeliveryKey, kind Kind, addresses []string,
	rec *ports.MailReceipt, err error) Outcome {
	o := Outcome{
		AccountID: accountID, Keys: keys, Kind: kind, Recipients: addresses,
	}
	if err != nil {
		o.State = StateError
		o.Error = err.Error()
		if rec != nil {
			o.Provider = rec.Provider
		}
		return o
	}
	o.Provider = rec.Provider
	o.Reference = rec.Reference
	switch rec.State {
	case ports.MailSentLocal:
		o.State = StateSentLocal
	default:
		o.State = StateSent
	}
	return o
}

func (s *Service) link(path string) string {
	if s.cfg.BaseURL == "" || path == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (s *Service) linkOfRule(name string, data map[string]any) string {
	for _, r := range Rules() {
		if r.Name == name {
			return s.link(resolvePath(r.LinkPath, data))
		}
	}
	return ""
}

// resolvePath swaps each `{field}` for the value of `data[field]`.
//
// A missing or empty field returns an EMPTY path — and an empty path erases the
// link (the template hides the button). It is deliberate: sending the person to
// `/invites/{invite_id}` is worse than sending no link at all, because they
// click, it breaks, and they conclude the invite is worthless. The table test
// keeps that out of production; this is the net underneath.
func resolvePath(path string, data map[string]any) string {
	if !strings.Contains(path, "{") {
		return path
	}
	for _, field := range placeholders(path) {
		v, ok := data[field]
		if !ok {
			return ""
		}
		txt := strings.TrimSpace(fmt.Sprint(v))
		if txt == "" {
			return ""
		}
		path = strings.ReplaceAll(path, "{"+field+"}", url.PathEscape(txt))
	}
	return path
}

// placeholders extracts the names between braces. It lives here and not in the
// rules package because whoever validates the table (the test) and whoever
// resolves it (the service) need the SAME extractor — two extractors diverge in
// silence.
func placeholders(path string) []string {
	var out []string
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			return out
		}
		j := strings.Index(path[i:], "}")
		if j < 0 {
			return out
		}
		out = append(out, path[i+1:i+j])
		path = path[i+j+1:]
	}
}

func emails(rs []Recipient) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Email)
	}
	return out
}

// itemsToData flattens the items for the template. A map and not a struct: what
// crosses the port is template DATA, and a struct from here would force every
// adapter to know this package's type.
func itemsToData(items []AttentionNotice) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"kind": it.Kind, "title": it.Title, "summary": it.Summary,
			"opened_at": it.OpenedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}
