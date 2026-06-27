package object

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// GarbageCollect removes orphaned files under _data/ and orphaned directories
// under _parts/ on the given WebDAV location.
// A _data/ file is orphaned when no object on the location references its HashPath.
// A _parts/{uploadID}/ directory is orphaned when no active multipart upload and
// no completed object's chunks reference that uploadID.
// Returns the number of files/directories deleted.
//
// Avoid running during concurrent CreateMultipartUpload calls to prevent false positives.
func (s *objectService) GarbageCollect(ctx context.Context, locationID string) (int, error) {
	loc, err := s.structure.GetLocation(locationID)
	if err != nil {
		return 0, fmt.Errorf("get location: %w", err)
	}

	allBuckets, err := s.structure.ListBuckets()
	if err != nil {
		return 0, fmt.Errorf("list buckets: %w", err)
	}

	referenced := make(map[string]struct{})
	referencedUploads := make(map[string]struct{})

	for _, bkt := range allBuckets {
		if bkt.WebDAVLocationID != locationID {
			continue
		}
		bdb, release, err := s.cache.Get(bkt.ID)
		if err != nil {
			return 0, fmt.Errorf("open bucket db %s: %w", bkt.ID, err)
		}

		objs, _, listErr := bdb.ListObjects("", "", "", 0)
		if listErr != nil {
			release()
			return 0, fmt.Errorf("list objects in bucket %s: %w", bkt.ID, listErr)
		}
		partsPrefix := loc.BaseDir + "_parts/"
		for _, obj := range objs {
			if obj.HashPath != "" {
				referenced[obj.HashPath] = struct{}{}
				continue // single-part object, no chunks
			}
			// Multipart object: chunks not loaded by ListObjects — fetch full record.
			full, err := bdb.GetObject(obj.Key)
			if err != nil {
				slog.Warn("gc: get object failed", "key", obj.Key, "err", err)
				continue
			}
			for _, chunk := range full.Chunks {
				rel := strings.TrimPrefix(chunk.Path, partsPrefix)
				if rel == chunk.Path {
					continue // not a _parts/ path
				}
				if i := strings.Index(rel, "/"); i >= 0 {
					referencedUploads[rel[:i]] = struct{}{}
				}
			}
		}

		uploads, uploadsErr := bdb.ListMultipartUploads()
		release()
		if uploadsErr != nil {
			return 0, fmt.Errorf("list multipart uploads in bucket %s: %w", bkt.ID, uploadsErr)
		}
		for _, up := range uploads {
			referencedUploads[up.ID] = struct{}{}
		}
	}

	deleted := 0

	// _data/ pass
	dataDir := fmt.Sprintf("%s_data/", loc.BaseDir)
	prefixDirs, err := s.wdc.ReadDir(ctx, dataDir)
	if err != nil {
		slog.Debug("gc: _data/ not listable", "locationID", locationID, "err", err)
	} else {
		for _, prefix := range prefixDirs {
			if prefix == ".tmp" {
				continue
			}
			dirPath := dataDir + prefix
			files, err := s.wdc.ReadDir(ctx, dirPath)
			if err != nil {
				slog.Warn("gc: readdir failed", "path", dirPath, "err", err)
				continue
			}
			for _, name := range files {
				hashPath := dirPath + "/" + name
				if _, ok := referenced[hashPath]; !ok {
					if err := s.wdc.Delete(ctx, hashPath); err != nil {
						slog.Warn("gc: delete failed", "path", hashPath, "err", err)
						continue
					}
					slog.Info("gc: deleted orphan", "path", hashPath)
					deleted++
				}
			}
		}
	}

	// _parts/ pass
	partsDir := fmt.Sprintf("%s_parts/", loc.BaseDir)
	uploadDirs, err := s.wdc.ReadDir(ctx, partsDir)
	if err != nil {
		slog.Debug("gc: _parts/ not listable", "locationID", locationID, "err", err)
		return deleted, nil
	}
	for _, dir := range uploadDirs {
		if _, ok := referencedUploads[dir]; !ok {
			dirPath := partsDir + dir
			if err := s.wdc.Delete(ctx, dirPath); err != nil {
				slog.Warn("gc: delete parts dir failed", "path", dirPath, "err", err)
				continue
			}
			slog.Info("gc: deleted orphan parts dir", "path", dirPath)
			deleted++
		}
	}

	return deleted, nil
}
