package sessions

import (
	"fmt"
	"os"
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
	result := RecentGroups{Current: []Summary{}, Global: []Summary{}}
	for _, item := range items {
		if matchesWorkspaceSummary(item, workspace) {
			result.Current = append(result.Current, item)
		} else {
			result.Global = append(result.Global, item)
		}
	}
	result.Current = summarizeSessions(result.Current, false, limit, failures)
	result.Global = summarizeSessions(result.Global, false, limit, failures)
	return result, failures.err()
}
