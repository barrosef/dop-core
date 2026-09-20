package identity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/domain/ports"
	"github.com/barrosef/dop-core/internal/platform/ctxutil"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

func str(s string) *string { return &s }

func TestUpdateProfileValidates(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	ctx, _, _ := signedUp(t, svc, "sub-1")
	future := now.Add(24 * time.Hour)
	cases := map[string]struct {
		patch identity.ProfilePatch
		code  string
	}{
		"locale":     {identity.ProfilePatch{Locale: str("portuguese")}, identity.KeyLocaleInvalid},
		"phone":      {identity.ProfilePatch{Phone: str("11999")}, identity.KeyPhoneInvalid},
		"birth date": {identity.ProfilePatch{BirthDate: &future}, identity.KeyBirthDateInvalid},
		"empty name": {identity.ProfilePatch{Name: str("   ")}, identity.KeyNameRequired},
	}
	for name, c := range cases {
		_, err := svc.UpdateProfile(ctx, c.patch)
		if code, _ := errs.CodeOf(err); errs.KindOf(err) != errs.KindInvalid || code != c.code {
			t.Errorf("%s: want Invalid with %s, got %v", name, c.code, err)
		}
	}
	u, err := svc.UpdateProfile(ctx, identity.ProfilePatch{
		Name: str("  Ed  "), Locale: str("pt-BR"), Timezone: str("America/Sao_Paulo"), Phone: str("+55 11 99999 9999"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "Ed" || u.Locale != "pt-BR" || u.Phone != "+5511999999999" {
		t.Errorf("profile not normalized: %+v", u)
	}
}

func TestUpdateProfileClearsVerificationWhenPhoneChanges(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	ctx, u, _ := signedUp(t, svc, "sub-1")
	if _, err := svc.UpdateProfile(ctx, identity.ProfilePatch{Phone: str("+5511999999999")}); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkPhoneVerified(ctx, u.ID, "+5511999999999"); err != nil {
		t.Fatal(err)
	}
	state, _ := svc.Onboarding(ctx)
	if !state.PhoneVerified {
		t.Fatal("the phone should be verified after the factor's confirmation")
	}
	// A factor on ANOTHER number proves nothing about the contact phone.
	svc.MarkPhoneVerified(ctx, u.ID, "+5511888888888")
	if _, err := svc.UpdateProfile(ctx, identity.ProfilePatch{Phone: str("+5511777777777")}); err != nil {
		t.Fatal(err)
	}
	state, _ = svc.Onboarding(ctx)
	if state.PhoneVerified {
		t.Error("a changed phone must arrive unverified")
	}
}

func TestHandleAvailabilitySuggestsWhenTaken(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	ctx, _, acct := signedUp(t, svc, "dev") // handle "dev"
	other := ctxutil.Into(context.Background(), ctxutil.Call{ActorID: "somebody-else"})

	got, err := svc.HandleAvailability(other, "DEV")
	if err != nil {
		t.Fatal(err)
	}
	if got.Available || !strings.HasPrefix(got.Suggestion, "dev-") {
		t.Errorf("taken handle should suggest an alternative: %+v", got)
	}
	// Typing my own handle back is not a conflict.
	mine, _ := svc.HandleAvailability(ctx, acct.Handle)
	if !mine.Available {
		t.Errorf("my own handle should read as available to me: %+v", mine)
	}
	if _, err := svc.HandleAvailability(ctx, "a"); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("a 1-character handle should be invalid, got %v", err)
	}
}

func TestUpdatePersonalAccountEditsOnlyThePersonalOne(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now})
	ctx, _, personal := signedUp(t, svc, "dev")
	org, err := svc.CreateOrganization(ctx, "acme", "ACME", "00.000.000/0001-00")
	if err != nil {
		t.Fatal(err)
	}
	// Acting inside the organization still edits the PERSONAL account.
	inOrg := ctxutil.Into(context.Background(), ctxutil.Call{AccountID: org.ID, ActorID: mustActor(ctx)})
	a, err := svc.UpdatePersonalAccount(inOrg, "ed-barros", "Ed Barros")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != personal.ID || a.Handle != "ed-barros" || a.DisplayName != "Ed Barros" {
		t.Errorf("expected the personal account edited: %+v", a)
	}
	if _, err := svc.UpdatePersonalAccount(ctx, "acme", ""); errs.KindOf(err) != errs.KindConflict {
		t.Errorf("taking an organization's handle should conflict, got %v", err)
	}
}

func TestSetPlanRefusesUnknown(t *testing.T) {
	svc := identity.NewService(newFakeRepo(), fixedClock{now}).
		WithPlans(fakePlans{known: map[string]bool{"pro": true}})
	ctx, _, _ := signedUp(t, svc, "dev")
	if _, err := svc.SetPlan(ctx, "gold"); errs.KindOf(err) != errs.KindInvalid {
		t.Errorf("unknown plan should be invalid, got %v", err)
	}
	a, err := svc.SetPlan(ctx, "pro")
	if err != nil || a.PlanKey != "pro" {
		t.Errorf("SetPlan(pro) = %+v, %v", a, err)
	}
}

func mustActor(ctx context.Context) string {
	c, _ := ctxutil.From(ctx)
	return c.ActorID
}

var _ = ports.Principal{}
