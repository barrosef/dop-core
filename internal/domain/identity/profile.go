package identity

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The person's own profile and personal account, as the onboarding journey
// edits them (spec 2026-09-20 §3.1, §3.5). Nothing here touches an
// organization: the personal account is the only one a person edits without
// being an admin of something.

const (
	nameMaxLen     = 120
	timezoneMaxLen = 64
)

var (
	localeRe = regexp.MustCompile(`^[a-z]{2}(-[A-Z]{2})?$`)
	phoneRe  = regexp.MustCompile(`^\+[1-9]\d{6,14}$`) // E.164
)

// Translation keys for what the profile refuses.
const (
	KeyNameRequired       = "identity.profile.name_required"
	KeyNameTooLong        = "identity.profile.name_too_long"
	KeyLocaleInvalid      = "identity.profile.locale_invalid"
	KeyTimezoneInvalid    = "identity.profile.timezone_invalid"
	KeyPhoneInvalid       = "identity.profile.phone_invalid"
	KeyBirthDateInvalid   = "identity.profile.birth_date_invalid"
	KeyPlanUnknown        = "identity.plan.unknown"
	KeyAccountNotPersonal = "identity.account.not_personal"
)

// ProfilePatch is a partial update: a nil field is untouched, an empty string
// clears (phone, locale, timezone) or is refused (name).
type ProfilePatch struct {
	Name           *string
	BirthDate      *time.Time
	ClearBirthDate bool
	Locale         *string
	Timezone       *string
	Phone          *string
}

// UpdateProfile validates and writes the actor's profile. A changed phone
// arrives unverified: the adapter clears `phone_verified_at` and emits
// `user.phone_added` into the personal account, which is what raises the
// reminder (D-8).
func (s *Service) UpdateProfile(ctx context.Context, patch ProfilePatch) (*User, error) {
	u, err := s.actorUser(ctx)
	if err != nil {
		return nil, err
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return nil, errs.Invalid("the name cannot be empty").WithCode(KeyNameRequired, nil)
		}
		if len(name) > nameMaxLen {
			return nil, errs.Invalid("the name may have at most %d characters", nameMaxLen).
				WithCode(KeyNameTooLong, map[string]any{"max": nameMaxLen})
		}
		u.Name = name
	}
	if patch.ClearBirthDate {
		u.BirthDate = nil
	} else if patch.BirthDate != nil {
		d := patch.BirthDate.UTC()
		if d.After(s.now()) || d.Year() < 1900 {
			return nil, errs.Invalid("the birth date is not plausible").WithCode(KeyBirthDateInvalid, nil)
		}
		u.BirthDate = &d
	}
	if patch.Locale != nil {
		l := strings.TrimSpace(*patch.Locale)
		if l != "" && !localeRe.MatchString(l) {
			return nil, errs.Invalid("invalid locale: %q (use pt-BR, en)", l).
				WithCode(KeyLocaleInvalid, map[string]any{"locale": l})
		}
		u.Locale = l
	}
	if patch.Timezone != nil {
		tz := strings.TrimSpace(*patch.Timezone)
		if len(tz) > timezoneMaxLen {
			return nil, errs.Invalid("invalid time zone").WithCode(KeyTimezoneInvalid, nil)
		}
		u.Timezone = tz
	}
	if patch.Phone != nil {
		ph := strings.ReplaceAll(strings.TrimSpace(*patch.Phone), " ", "")
		if ph != "" && !phoneRe.MatchString(ph) {
			return nil, errs.Invalid("invalid phone: use the international form, +5511999999999").
				WithCode(KeyPhoneInvalid, nil)
		}
		u.Phone = ph
	}
	personalID, err := s.PersonalAccountOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return s.repo.UpdateProfile(ctx, u, personalID)
}

// MarkPhoneVerified is called by the second factor when an SMS factor is
// confirmed; it is not reachable through any RPC. The adapter only writes when
// the destination IS the contact phone — a factor on another number proves
// nothing about this one.
func (s *Service) MarkPhoneVerified(ctx context.Context, userID, phone string) error {
	if userID == "" || phone == "" {
		return errs.Invalid("user and phone are required")
	}
	personalID, err := s.PersonalAccountOf(ctx, userID)
	if err != nil {
		return err
	}
	return s.repo.SetPhoneVerified(ctx, userID, phone, s.now(), personalID)
}

// HandleAvailability is what the profile step asks as the person types.
type HandleAvailability struct {
	Handle     string
	Available  bool
	Suggestion string
}

// HandleAvailability normalizes, validates and looks the handle up. The
// actor's own personal account does not make its handle "taken" — typing the
// current value back is not a conflict.
func (s *Service) HandleAvailability(ctx context.Context, handle string) (HandleAvailability, error) {
	h := NormalizeHandle(handle)
	if err := ValidateHandle(h); err != nil {
		return HandleAvailability{}, err
	}
	found, err := s.repo.AccountByHandle(ctx, h)
	if err != nil && errs.KindOf(err) != errs.KindNotFound {
		return HandleAvailability{}, err
	}
	if found == nil {
		return HandleAvailability{Handle: h, Available: true}, nil
	}
	if call, ok := ctxutil.From(ctx); ok && call.ActorID != "" {
		if mine, err := s.PersonalAccountOf(ctx, call.ActorID); err == nil && mine == found.ID {
			return HandleAvailability{Handle: h, Available: true}, nil
		}
	}
	return HandleAvailability{Handle: h, Available: false, Suggestion: h + "-" + randomSuffix(4)}, nil
}

// UpdatePersonalAccount edits the actor's PERSONAL account — handle and
// display name. Either may be empty, meaning untouched. It is the personal
// account by construction, not the active one: the journey runs before the
// person has picked anything, and an organization is edited elsewhere by its
// admins.
func (s *Service) UpdatePersonalAccount(ctx context.Context, handle, displayName string) (*Account, error) {
	u, err := s.actorUser(ctx)
	if err != nil {
		return nil, err
	}
	personalID, err := s.PersonalAccountOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	handle = strings.TrimSpace(handle)
	if handle != "" {
		handle = NormalizeHandle(handle)
		if err := ValidateHandle(handle); err != nil {
			return nil, err
		}
	}
	displayName = strings.TrimSpace(displayName)
	if len(displayName) > nameMaxLen {
		return nil, errs.Invalid("the name may have at most %d characters", nameMaxLen).
			WithCode(KeyNameTooLong, map[string]any{"max": nameMaxLen})
	}
	return s.repo.UpdateAccountProfile(ctx, personalID, handle, displayName)
}

// Plans is the one question identity asks the catalogue before recording a
// choice. Declared here, satisfied by catalog.Service in the composition root.
type Plans interface {
	PlanExists(ctx context.Context, key string) (bool, error)
}

// WithPlans wires the catalogue. Without it SetPlan refuses every key: an
// unvalidated plan is a foreign-key error waiting at the adapter.
func (s *Service) WithPlans(p Plans) *Service {
	s.plans = p
	return s
}

// SetPlan records the chosen plan on the actor's personal account (D-6).
func (s *Service) SetPlan(ctx context.Context, planKey string) (*Account, error) {
	planKey = strings.TrimSpace(planKey)
	if planKey == "" {
		return nil, errs.Invalid("plan not provided").WithCode(KeyPlanRequired, nil)
	}
	if s.plans == nil {
		return nil, errs.Precondition("no plan catalogue is wired")
	}
	ok, err := s.plans.PlanExists(ctx, planKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.Invalid("unknown plan: %q", planKey).
			WithCode(KeyPlanUnknown, map[string]any{"plan": planKey})
	}
	u, err := s.actorUser(ctx)
	if err != nil {
		return nil, err
	}
	personalID, err := s.PersonalAccountOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return s.repo.SetAccountPlan(ctx, personalID, planKey)
}
