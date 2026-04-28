package store

import (
	"strconv"
	"time"

	"github.com/ztelliot/mtr/internal/model"
)

func normalizedHistoryLimit(limit int) int {
	if limit <= 0 {
		return 2000
	}
	if limit > 2000 {
		return 2000
	}
	return limit
}

func historyFilterMatches(value time.Time, filter ScheduledJobHistoryFilter) bool {
	if filter.HasFrom && value.Before(filter.From) {
		return false
	}
	if filter.HasTo && value.After(filter.To) {
		return false
	}
	return true
}

func historyRevisionMatches(job model.Job, filter ScheduledJobHistoryFilter) bool {
	if filter.Revision <= 0 {
		return true
	}
	return job.ScheduledRevision == filter.Revision
}

func historyRunAt(job model.Job) time.Time {
	if job.StartedAt != nil && !job.StartedAt.IsZero() {
		return *job.StartedAt
	}
	return job.CreatedAt
}

func historyBucketKey(job model.Job, filter ScheduledJobHistoryFilter) string {
	if filter.BucketSeconds <= 0 {
		return job.ID
	}
	return job.AgentID + ":" + strconv.FormatInt(historyRunAt(job).Unix()/filter.BucketSeconds, 10)
}
