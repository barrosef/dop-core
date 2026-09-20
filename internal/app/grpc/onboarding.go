package grpc

import (
	"context"
	"time"

	dopv1 "github.com/barrosef/dop-core/api/gen/dop/v1"
	"github.com/barrosef/dop-core/internal/domain/identity"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The onboarding journey's RPCs (spec 2026-09-20 §5), on the identity server.
// Translation only: every rule is in identity/onboarding.go and profile.go.

func (s *IdentityServer) UpdateProfile(ctx context.Context, req *dopv1.UpdateProfileRequest) (*dopv1.User, error) {
	patch := identity.ProfilePatch{
		Name:     req.Name,
		Locale:   req.Locale,
		Timezone: req.Timezone,
		Phone:    req.Phone,
	}
	if req.BirthDate != nil {
		if *req.BirthDate == "" {
			patch.ClearBirthDate = true
		} else {
			d, err := time.Parse("2006-01-02", *req.BirthDate)
			if err != nil {
				return nil, errs.Invalid("birth_date must be an ISO date (YYYY-MM-DD)").
					WithCode(identity.KeyBirthDateInvalid, nil)
			}
			patch.BirthDate = &d
		}
	}
	u, err := s.svc.UpdateProfile(ctx, patch)
	if err != nil {
		return nil, err
	}
	return userToProto(u), nil
}

func (s *IdentityServer) GetOnboarding(ctx context.Context, _ *dopv1.GetOnboardingRequest) (*dopv1.OnboardingState, error) {
	st, err := s.svc.Onboarding(ctx)
	if err != nil {
		return nil, err
	}
	return onboardingToProto(st), nil
}

func (s *IdentityServer) RecordOnboardingStep(ctx context.Context, req *dopv1.RecordOnboardingStepRequest) (*dopv1.OnboardingState, error) {
	st, err := s.svc.RecordStep(ctx, identity.Step(req.GetStep()), identity.StepStatus(req.GetStatus()))
	if err != nil {
		return nil, err
	}
	return onboardingToProto(st), nil
}

func (s *IdentityServer) CompleteOnboarding(ctx context.Context, _ *dopv1.CompleteOnboardingRequest) (*dopv1.OnboardingState, error) {
	st, err := s.svc.CompleteOnboarding(ctx)
	if err != nil {
		return nil, err
	}
	return onboardingToProto(st), nil
}

func (s *IdentityServer) CheckHandle(ctx context.Context, req *dopv1.CheckHandleRequest) (*dopv1.HandleAvailability, error) {
	h, err := s.svc.HandleAvailability(ctx, req.GetHandle())
	if err != nil {
		return nil, err
	}
	return &dopv1.HandleAvailability{Handle: h.Handle, Available: h.Available, Suggestion: h.Suggestion}, nil
}

func (s *IdentityServer) UpdatePersonalAccount(ctx context.Context, req *dopv1.UpdatePersonalAccountRequest) (*dopv1.Account, error) {
	a, err := s.svc.UpdatePersonalAccount(ctx, req.GetHandle(), req.GetDisplayName())
	if err != nil {
		return nil, err
	}
	return accountToProto(a), nil
}

func (s *IdentityServer) SetPlan(ctx context.Context, req *dopv1.SetPlanRequest) (*dopv1.Account, error) {
	a, err := s.svc.SetPlan(ctx, req.GetPlanKey())
	if err != nil {
		return nil, err
	}
	return accountToProto(a), nil
}

// onboardingToProto lists EVERY step in journey order, so the cockpit's rail
// never has to know the order: a step absent from the record is "pending".
func onboardingToProto(st *identity.OnboardingState) *dopv1.OnboardingState {
	out := &dopv1.OnboardingState{
		Current:           string(st.Current),
		Complete:          st.Complete,
		EmailVerified:     st.EmailVerified,
		Phone:             st.Phone,
		PhoneVerified:     st.PhoneVerified,
		CodeConnections:   int32(st.CodeConnections),
		TaskConnections:   int32(st.TaskConnections),
		PlanKey:           st.PlanKey,
		PersonalAccountId: st.PersonalAccountID,
	}
	for _, step := range identity.Steps() {
		status := "pending"
		if v := st.Steps[step]; v != "" {
			status = string(v)
		}
		out.Steps = append(out.Steps, &dopv1.OnboardingStep{Step: string(step), Status: status})
	}
	return out
}
