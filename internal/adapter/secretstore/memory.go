package secretstore

import (
	"context"
	"sync"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
)

// Memory is the in-memory adapter — used in the domain tests and as a living
// reference for the contract: it passes exactly the same set of tests as the k8s
// adapter and the GCP one.
type Memory struct {
	mu   sync.RWMutex
	data map[string]ports.SecretValue
}

func NewMemory() *Memory { return &Memory{data: map[string]ports.SecretValue{}} }

func key(r ports.SecretRef) string { return r.AccountID + "/" + r.Kind + "/" + r.OwnerID }

func (m *Memory) Put(_ context.Context, ref ports.SecretRef, v ports.SecretValue) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(ports.SecretValue, len(v))
	copy(cp, v)
	m.data[key(ref)] = cp
	return nil
}

func (m *Memory) Get(_ context.Context, ref ports.SecretRef) (ports.SecretValue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key(ref)]
	if !ok {
		return nil, nil
	}
	cp := make(ports.SecretValue, len(v))
	copy(cp, v)
	return cp, nil
}

func (m *Memory) Delete(_ context.Context, ref ports.SecretRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key(ref))
	return nil
}

func (m *Memory) Exists(ctx context.Context, ref ports.SecretRef) (bool, error) {
	v, err := m.Get(ctx, ref)
	return v != nil, err
}

var _ ports.SecretStore = (*Memory)(nil)
