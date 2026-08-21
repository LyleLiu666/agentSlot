package session

import "sort"

func sortSessionSummaries(summaries []SessionSummary) {
	sort.Slice(summaries, func(left, right int) bool {
		if !summaries[left].UpdatedAt.Equal(summaries[right].UpdatedAt) {
			return summaries[left].UpdatedAt.After(summaries[right].UpdatedAt)
		}
		return summaries[left].SessionID < summaries[right].SessionID
	})
}
