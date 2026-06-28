package repair

import (
	"errors"
	"fmt"
	"sync"
)

type Status string

const (
	Healthy   Status = "healthy"
	Repairing Status = "repairing"
	Degraded  Status = "degraded"
)

var ErrUnavailable = errors.New("bucket metadata unavailable")

type Manager struct {
	mu       sync.Mutex
	statuses map[string]Status
	reasons  map[string]string
}

func NewManager() *Manager {
	return &Manager{
		statuses: map[string]Status{},
		reasons:  map[string]string{},
	}
}

func (m *Manager) Status(bucketID string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(bucketID)
}

func (m *Manager) CheckWrite(bucketID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := m.statusLocked(bucketID)
	if status == Healthy {
		return nil
	}
	reason := m.reasons[bucketID]
	if reason == "" {
		reason = string(status)
	}
	return fmt.Errorf("%w: bucket %s metadata is %s: %s", ErrUnavailable, bucketID, status, reason)
}

func (m *Manager) MarkRepairing(bucketID, reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.statusLocked(bucketID) == Repairing {
		return false
	}
	m.statuses[bucketID] = Repairing
	m.reasons[bucketID] = reason
	return true
}

func (m *Manager) MarkDegraded(bucketID, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[bucketID] = Degraded
	m.reasons[bucketID] = reason
}

func (m *Manager) MarkHealthy(bucketID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.statuses, bucketID)
	delete(m.reasons, bucketID)
}

func (m *Manager) statusLocked(bucketID string) Status {
	status, ok := m.statuses[bucketID]
	if !ok {
		return Healthy
	}
	return status
}
