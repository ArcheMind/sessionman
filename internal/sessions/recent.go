package sessions

import (
	"fmt"
	"os"
	"time"
)

type RecentGroups struct {
	Current []Summary
	Global  []Summary
}

func (s Service) RecentGroups(workspace string, limit int, includeSubagents bool) (RecentGroups, error) {
	if limit <= 0 {
		return RecentGroups{}, fmt.Errorf("recent session limit must be positive")
	}
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return RecentGroups{}, fmt.Errorf("read working directory: %w", err)
		}
	}
	workspace = cleanPath(workspace)

	items, err := s.catalog("", includeSubagents)
	failures := catalogFailure(err)
	closed, hasClosed, closedErr := s.LatestClosed()
	if closedErr != nil {
		failures.Causes = append(failures.Causes, closedErr)
	}
	result := RecentGroups{Current: []Summary{}, Global: []Summary{}}
	var pinned *Summary
	for _, item := range items {
		if hasClosed && item.Agent == closed.Agent && item.ID == closed.ID {
			item.ClosedAt = closed.ClosedAt.UTC().Format(time.RFC3339Nano)
			pinned = &item
			continue
		}
		if matchesWorkspaceSummary(item, workspace) {
			result.Current = append(result.Current, item)
		} else {
			result.Global = append(result.Global, item)
		}
	}
	if pinned != nil {
		result.Current = append([]Summary{*pinned}, result.Current...)
	}
	for i := range result.Current {
		result.Current[i].collectUserPrompts = true
	}
	for i := range result.Global {
		result.Global[i].collectUserPrompts = true
	}
	result.Current = summarizeSessions(result.Current, false, limit, failures)
	result.Global = summarizeSessions(result.Global, false, limit, failures)
	if pinned != nil && len(result.Current) > 0 {
		result.Current[0].ClosedAt = pinned.ClosedAt
	}
	return result, failures.err()
}
