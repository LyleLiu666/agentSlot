package standardagent

import (
	"context"
	"sort"

	"github.com/LyleLiu666/agentSlot/hook"
	"github.com/LyleLiu666/agentSlot/session"
)

// extensionDiagnosticsForEntries is the single projection read used by
// operation receipts and typed extension errors. Store failures are returned:
// callers must not silently omit diagnostics or synthesize facts from a
// possibly stale in-memory occurrence.
func (r *runtimeInstance) extensionDiagnosticsForEntries(entries []session.ExtensionJournalEntry) ([]session.ExtensionDiagnostic, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	page, err := r.components.store.ExtensionDiagnostics(context.Background(), session.ExtensionPageRequest{
		SessionID: r.id(), Limit: session.MaxExtensionPageLimit,
	})
	if err != nil {
		return nil, err
	}
	ids := make(map[hook.InvocationID]struct{}, len(entries))
	for _, entry := range entries {
		ids[entry.InvocationID] = struct{}{}
	}
	result := make([]session.ExtensionDiagnostic, 0, len(entries))
	for _, diagnostic := range page.Diagnostics {
		if _, ok := ids[diagnostic.InvocationID]; ok {
			result = append(result, diagnostic)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result, nil
}
