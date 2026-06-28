package repair_test

import (
	"errors"
	"testing"

	"github.com/hpolthof/webdavs3/internal/repair"
)

func TestManager_DefaultAllowsWrites(t *testing.T) {
	m := repair.NewManager()

	if err := m.CheckWrite("bucket-1"); err != nil {
		t.Fatalf("CheckWrite default status: %v", err)
	}
	if got := m.Status("bucket-1"); got != repair.Healthy {
		t.Fatalf("Status = %s, want %s", got, repair.Healthy)
	}
}

func TestManager_BlocksWritesWhileRepairing(t *testing.T) {
	m := repair.NewManager()
	first := m.MarkRepairing("bucket-1", "remote metadata corrupt")
	second := m.MarkRepairing("bucket-1", "same repair")

	if !first {
		t.Fatal("first MarkRepairing should claim the repair")
	}
	if second {
		t.Fatal("second MarkRepairing should not claim an active repair")
	}
	if err := m.CheckWrite("bucket-1"); !errors.Is(err, repair.ErrUnavailable) {
		t.Fatalf("CheckWrite err = %v, want ErrUnavailable", err)
	}
}

func TestManager_DegradedBlocksUntilHealthy(t *testing.T) {
	m := repair.NewManager()
	m.MarkDegraded("bucket-1", "local metadata missing")

	if err := m.CheckWrite("bucket-1"); !errors.Is(err, repair.ErrUnavailable) {
		t.Fatalf("CheckWrite degraded err = %v, want ErrUnavailable", err)
	}

	m.MarkHealthy("bucket-1")
	if err := m.CheckWrite("bucket-1"); err != nil {
		t.Fatalf("CheckWrite after healthy: %v", err)
	}
}
