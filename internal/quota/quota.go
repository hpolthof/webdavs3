package quota

import (
	"context"
	"errors"
	"fmt"

	"github.com/hpolthof/webdavs3/internal/meta"
)

// exceededError signals that the quota would be exceeded.
type exceededError struct {
	locationID      string
	currentBytes    int64
	additionalBytes int64
	quotaBytes      int64
}

func (e *exceededError) Error() string {
	return fmt.Sprintf("quota exceeded for location %s: current %d + additional %d > limit %d",
		e.locationID, e.currentBytes, e.additionalBytes, e.quotaBytes)
}

// IsExceeded reports whether err is a quota-exceeded error.
func IsExceeded(err error) bool {
	var e *exceededError
	return errors.As(err, &e)
}

// Service checks quota before writes.
type Service interface {
	Check(ctx context.Context, locationID string, additionalBytes int64) error
}

type service struct {
	structure meta.StructureDB
	stats     meta.StatsDB
}

// New creates a quota Service.
func New(structure meta.StructureDB, stats meta.StatsDB) Service {
	return &service{structure: structure, stats: stats}
}

// Check returns an error if adding additionalBytes to location locationID
// would exceed its quota. QuotaBytes == 0 means unlimited.
func (s *service) Check(ctx context.Context, locationID string, additionalBytes int64) error {
	loc, err := s.structure.GetLocation(locationID)
	if err != nil {
		return fmt.Errorf("get location %s: %w", locationID, err)
	}
	if loc.QuotaBytes == 0 {
		return nil // unlimited
	}

	current, err := s.stats.GetTotalUsage(locationID)
	if err != nil {
		return fmt.Errorf("get usage for %s: %w", locationID, err)
	}

	if current+additionalBytes > loc.QuotaBytes {
		return &exceededError{
			locationID:      locationID,
			currentBytes:    current,
			additionalBytes: additionalBytes,
			quotaBytes:      loc.QuotaBytes,
		}
	}
	return nil
}
