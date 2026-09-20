package catalog_test

import (
	"context"
	"sort"
	"testing"

	"github.com/barrosef/dop-core/internal/domain/catalog"
	"github.com/barrosef/dop-core/internal/platform/errs"
)

// The fake applies the same two rules the SQL will — active only, display
// order — so the service is proven against the contract the adapter has to
// honour, not against a list that happens to be sorted.
type fakeRepo struct{ plans []catalog.Plan }

func (f *fakeRepo) Plans(context.Context) ([]catalog.Plan, error) {
	var out []catalog.Plan
	for _, p := range f.plans {
		if p.Active {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sort < out[j].Sort })
	return out, nil
}
func (f *fakeRepo) Providers(context.Context) ([]catalog.Provider, error) { return nil, nil }
func (f *fakeRepo) PlanByKey(_ context.Context, key string) (*catalog.Plan, error) {
	for _, p := range f.plans {
		if p.Key == key && p.Active {
			return &p, nil
		}
	}
	return nil, errs.NotFound("plan %q", key)
}
func (f *fakeRepo) ProviderByKey(_ context.Context, key string) (*catalog.Provider, error) {
	return nil, errs.NotFound("provider %q", key)
}

func TestPlansReturnsOnlyActiveInOrder(t *testing.T) {
	svc := catalog.NewService(&fakeRepo{plans: []catalog.Plan{
		{Key: "b", Sort: 20, Active: true},
		{Key: "retired", Sort: 5, Active: false},
		{Key: "a", Sort: 10, Active: true},
	}})
	plans, err := svc.Plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Key != "a" || plans[1].Key != "b" {
		t.Errorf("expected [a b], got %+v", plans)
	}
}

func TestPlanByKeyRefusesUnknown(t *testing.T) {
	svc := catalog.NewService(&fakeRepo{plans: []catalog.Plan{{Key: "free", Active: true}}})
	if _, err := svc.PlanByKey(context.Background(), "gold"); errs.KindOf(err) != errs.KindNotFound {
		t.Errorf("an unknown plan should be NotFound, got %v", err)
	}
	ok, err := svc.PlanExists(context.Background(), "gold")
	if err != nil || ok {
		t.Errorf("PlanExists(gold) = %v, %v; want false, nil", ok, err)
	}
	if ok, _ := svc.PlanExists(context.Background(), "free"); !ok {
		t.Error("PlanExists(free) should be true")
	}
}
