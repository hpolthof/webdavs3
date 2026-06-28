package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpolthof/webdavs3/internal/meta"
)

func TestExportBucketDBForRepairFallsBackWhenCheckpointFails(t *testing.T) {
	cacheDir := t.TempDir()
	localPath := filepath.Join(cacheDir, "bucket-b1.db")
	localDB, err := meta.OpenBucketDB(localPath)
	if err != nil {
		t.Fatalf("open local db: %v", err)
	}
	if err := localDB.PutObject(meta.Object{
		ID:             "obj-1",
		Key:            "doc.txt",
		HashPath:       "/_data/ab/hash",
		SizeBytes:      3,
		ETag:           "etag",
		ContentType:    "text/plain",
		LastModified:   time.Now(),
		UploadComplete: true,
	}); err != nil {
		t.Fatalf("put object: %v", err)
	}
	if err := localDB.Close(); err != nil {
		t.Fatalf("close local db: %v", err)
	}

	original := saveBucketDBFile
	saveBucketDBFile = func(_, _ string) error {
		return fmt.Errorf("checkpoint bucket db: database disk image is malformed (11)")
	}
	t.Cleanup(func() { saveBucketDBFile = original })

	exportPath := filepath.Join(cacheDir, "repair-export.db")
	if err := exportBucketDBForRepair(localPath, exportPath); err != nil {
		t.Fatalf("exportBucketDBForRepair: %v", err)
	}
	if _, err := os.Stat(localPath + ".before-repair"); err == nil {
		t.Fatal("export helper should not create repair backups")
	}
	if err := meta.ValidateBucketDBFile(exportPath); err != nil {
		t.Fatalf("exported db invalid: %v", err)
	}
	exportedDB, err := meta.OpenBucketDB(exportPath)
	if err != nil {
		t.Fatalf("open exported db: %v", err)
	}
	defer exportedDB.Close()
	if _, err := exportedDB.GetObject("doc.txt"); err != nil {
		t.Fatalf("exported db missing object: %v", err)
	}
}
