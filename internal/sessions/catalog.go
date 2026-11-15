package sessions

import (
	"errors"
	"fmt"
	"sort"
)

// CatalogError reports unreadable native entries alongside the usable catalog.
type CatalogError struct{ Causes []error }

func (e *CatalogError) Error() string {
	if err := errors.Join(e.Causes...); err != nil {
		return err.Error()
	}
	return ""
}
func (e *CatalogError) Unwrap() []error { return e.Causes }
func IsCatalogError(err error) bool     { var partial *CatalogError; return errors.As(err, &partial) }
func catalogFailure(err error) *CatalogError {
	var partial *CatalogError
	if errors.As(err, &partial) {
		return partial
	}
	if err != nil {
		return &CatalogError{Causes: []error{err}}
	}
	return &CatalogError{}
}
func (e *CatalogError) err() error {
	if len(e.Causes) == 0 {
		return nil
	}
	return e
}

func (s Service) catalog(agent string, includeSubagents bool) ([]Summary, error) {
	var items []Summary
	failures := &CatalogError{}
	for _, spec := range agentSpecs {
		if agent != "" && spec.id != agent {
			continue
		}
		found, err := spec.discover(s.HomeDir, includeSubagents)
		if err != nil {
			failures.Causes = append(failures.Causes, fmt.Errorf("load %s sessions: %w", spec.id, err))
		}
		items = append(items, found...)
	}
	items = filterSessions(items, "", includeSubagents)
	sortSessions(items, "date")
	return items, failures.err()
}

func filterSessions(items []Summary, workspace string, includeSubagents bool) []Summary {
	filtered := make([]Summary, 0, len(items))
	for _, item := range items {
		if (includeSubagents || !item.IsSubagent) && matchesWorkspaceSummary(item, workspace) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func sortSessions(items []Summary, order string) {
	sort.SliceStable(items, func(i, j int) bool {
		if order == "messages" && items[i].MessageCount != items[j].MessageCount {
			return items[i].MessageCount > items[j].MessageCount
		}
		if order == "agent" && items[i].Agent != items[j].Agent {
			return items[i].Agent < items[j].Agent
		}
		if items[i].UpdatedAt != items[j].UpdatedAt {
			return items[i].UpdatedAt > items[j].UpdatedAt
		}
		if items[i].Agent != items[j].Agent {
			return items[i].Agent < items[j].Agent
		}
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		return items[i].Source < items[j].Source
	})
}

func limitSessions(items []Summary, limit int) []Summary {
	if limit == 0 {
		return items
	}
	counts := make(map[string]int)
	limited := make([]Summary, 0, len(items))
	for _, item := range items {
		if counts[item.Agent] < limit {
			counts[item.Agent]++
			limited = append(limited, item)
		}
	}
	return limited
}

func summarizeSessions(items []Summary, includeCount bool, limit int, failures *CatalogError) []Summary {
	counts := make(map[string]int)
	result := make([]Summary, 0, len(items))
	for _, item := range items {
		if limit > 0 && counts[item.Agent] >= limit {
			continue
		}
		summary, err := agentSpecByID(item.Agent).summarize(item, includeCount)
		if err != nil {
			failures.Causes = append(failures.Causes, fmt.Errorf("summarize %s session %s: %w", item.Agent, item.Source, err))
			if includeCount {
				continue
			}
			summary = item
		}
		counts[item.Agent]++
		result = append(result, summary)
	}
	return result
}

func matchesWorkspaceSummary(item Summary, workspace string) bool {
	return workspace == "" || cleanPath(item.Workspace) == workspace
}
