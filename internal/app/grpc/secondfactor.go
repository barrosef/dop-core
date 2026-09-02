package grpc

import (
	"context"

	dopv1 "github.com/Digital-Business-One/dop-core/api/gen/dop/v1"
	"github.com/Digital-Business-One/dop-core/internal/domain/secondfactor"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type SecondFactorServer struct {
	dopv1.UnimplementedSecondFactorServiceServer
	svc *secondfactor.Service
}

func NewSecondFactorServer(svc *secondfactor.Service) *SecondFactorServer {
	return &SecondFactorServer{svc: svc}
}

func (s *SecondFactorServer) ListSecondFactors(ctx context.Context, _ *dopv1.ListSecondFactorsRequest) (*dopv1.ListSecondFactorsResponse, error) {
	factors, err := s.svc.List(ctx)
	if err != nil {
		return nil, err
	}
	return &dopv1.ListSecondFactorsResponse{Factors: factorsToProto(factors)}, nil
}

func (s *SecondFactorServer) EnrollSecondFactor(ctx context.Context, req *dopv1.EnrollSecondFactorRequest) (*dopv1.EnrollSecondFactorResponse, error) {
	kind := kindFromProto(req.GetKind())
	if kind == secondfactor.KindTOTP {
		enr, err := s.svc.EnrollTOTP(ctx, req.GetLabel())
		if err != nil {
			return nil, err
		}
		return &dopv1.EnrollSecondFactorResponse{
			Factor:      factorToProto(*enr.Factor),
			ChallengeId: enr.ChallengeID,
			Secret:      enr.Secret,
			Uri:         enr.URI,
		}, nil
	}
	f, challengeID, err := s.svc.EnrollCode(ctx, kind, req.GetLabel(), req.GetDestination())
	if err != nil {
		return nil, err
	}
	return &dopv1.EnrollSecondFactorResponse{Factor: factorToProto(*f), ChallengeId: challengeID}, nil
}

func (s *SecondFactorServer) ConfirmSecondFactor(ctx context.Context, req *dopv1.ConfirmSecondFactorRequest) (*dopv1.ConfirmSecondFactorResponse, error) {
	codes, err := s.svc.Confirm(ctx, req.GetFactorId(), req.GetChallengeId(), req.GetCode())
	if err != nil {
		return nil, err
	}
	factors, err := s.svc.List(ctx)
	if err != nil {
		return nil, err
	}
	out := &dopv1.ConfirmSecondFactorResponse{RecoveryCodes: codes}
	for _, f := range factors {
		if f.ID == req.GetFactorId() {
			out.Factor = factorToProto(f)
		}
	}
	return out, nil
}

func (s *SecondFactorServer) RevokeSecondFactor(ctx context.Context, req *dopv1.RevokeSecondFactorRequest) (*dopv1.SecondFactor, error) {
	if err := s.svc.Revoke(ctx, req.GetId()); err != nil {
		return nil, err
	}
	// The revoked factor is not read back: the listing no longer returns it, and
	// answering with the row would suggest it is still worth something.
	return &dopv1.SecondFactor{Id: req.GetId(), Status: dopv1.SecondFactor_STATUS_REVOKED}, nil
}

func (s *SecondFactorServer) ChallengeSecondFactor(ctx context.Context, req *dopv1.ChallengeSecondFactorRequest) (*dopv1.ChallengeSecondFactorResponse, error) {
	ch, err := s.svc.Challenge(ctx, req.GetFactorId())
	if err != nil {
		return nil, err
	}
	// The kind and the masked destination come back so the screen can say WHERE
	// the code went — "check your e-mail" and "check your phone" are different
	// screens, and guessing is what makes people wait for a message that is not
	// coming.
	out := &dopv1.ChallengeSecondFactorResponse{
		ChallengeId: ch.ID,
		ExpiresAt:   timestamppb.New(ch.ExpiresAt),
	}
	factors, err := s.svc.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range factors {
		if f.ID == ch.FactorID {
			out.Kind = kindToProto(f.Kind)
			out.MaskedDestination = f.Destination // already masked by List
		}
	}
	return out, nil
}

func (s *SecondFactorServer) VerifySecondFactor(ctx context.Context, req *dopv1.VerifySecondFactorRequest) (*dopv1.StepUp, error) {
	su, err := s.svc.Verify(ctx, req.GetChallengeId(), req.GetCode())
	if err != nil {
		return nil, err
	}
	return stepUpToProto(su), nil
}

func (s *SecondFactorServer) VerifyRecoveryCode(ctx context.Context, req *dopv1.VerifyRecoveryCodeRequest) (*dopv1.StepUp, error) {
	su, err := s.svc.VerifyRecoveryCode(ctx, req.GetCode())
	if err != nil {
		return nil, err
	}
	return stepUpToProto(su), nil
}

func (s *SecondFactorServer) GetSecondFactorState(ctx context.Context, _ *dopv1.GetSecondFactorStateRequest) (*dopv1.GetSecondFactorStateResponse, error) {
	st, err := s.svc.State(ctx)
	if err != nil {
		return nil, err
	}
	out := &dopv1.GetSecondFactorStateResponse{
		Required:          st.Required,
		Enrolled:          st.Enrolled,
		SteppedUp:         st.SteppedUp,
		NeedsSetup:        st.NeedsSetup,
		Factors:           factorsToProto(st.Factors),
		RecoveryCodesLeft: int32(st.Recovery),
	}
	for _, k := range st.Allowed {
		out.Allowed = append(out.Allowed, kindToProto(k))
	}
	if st.ExpiresAt != nil {
		out.StepUpExpiresAt = timestamppb.New(*st.ExpiresAt)
	}
	return out, nil
}

func (s *SecondFactorServer) RegenerateRecoveryCodes(ctx context.Context, _ *dopv1.RegenerateRecoveryCodesRequest) (*dopv1.RegenerateRecoveryCodesResponse, error) {
	codes, err := s.svc.RegenerateRecoveryCodes(ctx)
	if err != nil {
		return nil, err
	}
	return &dopv1.RegenerateRecoveryCodesResponse{Codes: codes}, nil
}

// ── conversion ──────────────────────────────────────────────────────────────

func factorsToProto(in []secondfactor.Factor) []*dopv1.SecondFactor {
	out := make([]*dopv1.SecondFactor, 0, len(in))
	for _, f := range in {
		out = append(out, factorToProto(f))
	}
	return out
}

func factorToProto(f secondfactor.Factor) *dopv1.SecondFactor {
	p := &dopv1.SecondFactor{
		Id:   f.ID,
		Kind: kindToProto(f.Kind),
		// The destination arrives here ALREADY masked (the service masks it in
		// List and in the enrolment). Masking in this layer would put a security
		// rule in the translator, which is where nobody looks for it.
		MaskedDestination: f.Destination,
		Label:             f.Label,
		Status:            statusToProto(f.Status),
	}
	if f.ConfirmedAt != nil {
		p.ConfirmedAt = timestamppb.New(*f.ConfirmedAt)
	}
	if f.LastUsedAt != nil {
		p.LastUsedAt = timestamppb.New(*f.LastUsedAt)
	}
	return p
}

func stepUpToProto(su *secondfactor.StepUp) *dopv1.StepUp {
	return &dopv1.StepUp{
		Method:     kindToProto(su.Method),
		Recovery:   su.Method == "recovery_code",
		VerifiedAt: timestamppb.New(su.VerifiedAt),
		ExpiresAt:  timestamppb.New(su.ExpiresAt),
	}
}

func kindToProto(k secondfactor.Kind) dopv1.SecondFactorKind {
	switch k {
	case secondfactor.KindTOTP:
		return dopv1.SecondFactorKind_SECOND_FACTOR_KIND_TOTP
	case secondfactor.KindEmail:
		return dopv1.SecondFactorKind_SECOND_FACTOR_KIND_EMAIL
	case secondfactor.KindSMS:
		return dopv1.SecondFactorKind_SECOND_FACTOR_KIND_SMS
	}
	// A recovery code is not a Kind — it is the way back. UNSPECIFIED here, with
	// the `recovery` flag saying what happened.
	return dopv1.SecondFactorKind_SECOND_FACTOR_KIND_UNSPECIFIED
}

func kindFromProto(k dopv1.SecondFactorKind) secondfactor.Kind {
	switch k {
	case dopv1.SecondFactorKind_SECOND_FACTOR_KIND_TOTP:
		return secondfactor.KindTOTP
	case dopv1.SecondFactorKind_SECOND_FACTOR_KIND_EMAIL:
		return secondfactor.KindEmail
	case dopv1.SecondFactorKind_SECOND_FACTOR_KIND_SMS:
		return secondfactor.KindSMS
	}
	// UNSPECIFIED does NOT become a default: the domain refuses an unknown kind,
	// and choosing one here would be the edge deciding which factor the person
	// enrolled.
	return ""
}

func statusToProto(s secondfactor.Status) dopv1.SecondFactor_Status {
	switch s {
	case secondfactor.StatusPending:
		return dopv1.SecondFactor_STATUS_PENDING
	case secondfactor.StatusActive:
		return dopv1.SecondFactor_STATUS_ACTIVE
	case secondfactor.StatusRevoked:
		return dopv1.SecondFactor_STATUS_REVOKED
	}
	return dopv1.SecondFactor_STATUS_UNSPECIFIED
}
