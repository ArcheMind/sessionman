package app

import (
	"context"
	"strings"
)

func selectorItemIndices(rows []contentRow) []int {
	var indices []int
	for _, row := range rows {
		if row.itemIndex >= 0 && (len(indices) == 0 || indices[len(indices)-1] != row.itemIndex) {
			indices = append(indices, row.itemIndex)
		}
	}
	return indices
}

func filterSessionPrompts(ctx context.Context, rows []contentRow, query string) []contentRow {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return rows
	}
	var result []contentRow
	var heading *contentRow
	for _, row := range rows {
		if ctx.Err() != nil {
			return nil
		}
		if row.kind == contentRowHeading {
			value := row
			heading = &value
		}
		if row.kind != contentRowSession {
			continue
		}
		matches := true
		for _, term := range terms {
			found := false
			for _, prompt := range row.userPrompts {
				if ctx.Err() != nil {
					return nil
				}
				if strings.Contains(prompt, term) {
					found = true
					break
				}
			}
			if !found {
				matches = false
				break
			}
		}
		if matches {
			if heading != nil {
				result = append(result, *heading)
				heading = nil
			}
			result = append(result, row)
		}
	}
	if len(result) == 0 {
		result = append(result, contentRow{text: "No matching sessions", kind: contentRowEmpty, itemIndex: -1})
	}
	return result
}
