package quota_test

import (
	"context"
	"testing"
	"time"

	"github.com/hpolthof/webdav3s/internal/meta"
	"github.com/hpolthof/webdav3s/internal/quota"
)

func TestQuota_UnderLimit(t *testing.T) {
	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	statsDB, _ := meta.OpenStatsDB(":memory:", "d1")
	defer statsDB.Close()

	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "x", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 30, // 1 GiB
		CreatedAt: time.Now(),
	})
	statsDB.AddDelta("loc-1", "u-1", "bkt-1", 500<<20, 10) // 500 MiB used

	svc := quota.New(structDB, statsDB)
	if err := svc.Check(context.Background(), "loc-1", 100<<20); err != nil {
		t.Errorf("expected no error under quota, got: %v", err)
	}
}

func TestQuota_ExceedsLimit(t *testing.T) {
	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	statsDB, _ := meta.OpenStatsDB(":memory:", "d1")
	defer statsDB.Close()

	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "x", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 1 << 20, // 1 MiB
		CreatedAt: time.Now(),
	})
	statsDB.AddDelta("loc-1", "u-1", "bkt-1", 900<<10, 5) // 900 KiB used

	svc := quota.New(structDB, statsDB)
	// Requesting 200 KiB more would exceed 1 MiB
	err := svc.Check(context.Background(), "loc-1", 200<<10)
	if err == nil {
		t.Fatal("expected quota exceeded error, got nil")
	}
	if !quota.IsExceeded(err) {
		t.Errorf("expected IsExceeded(err) == true, got false for: %v", err)
	}
}

func TestQuota_ZeroQuota_Unlimited(t *testing.T) {
	structDB, _ := meta.OpenStructureDB(":memory:")
	defer structDB.Close()
	statsDB, _ := meta.OpenStatsDB(":memory:", "d1")
	defer statsDB.Close()

	structDB.AddLocation(meta.Location{
		ID: "loc-1", URL: "x", Username: "u", PasswordEnc: "e",
		DisplayName: "L", QuotaBytes: 0, // unlimited
		CreatedAt: time.Now(),
	})
	statsDB.AddDelta("loc-1", "u-1", "bkt-1", 10<<30, 1000) // 10 GiB

	svc := quota.New(structDB, statsDB)
	if err := svc.Check(context.Background(), "loc-1", 10<<30); err != nil {
		t.Errorf("expected no error for unlimited quota, got: %v", err)
	}
}
