// Package hook defines the constrained extension points around a fixed
// AgentRuntime. Hooks propose or observe; they do not control runtime state.
package hook

import (
	"context"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
)

// HookSlot is the ordered, optional AgentHook ecosystem.
var HookSlot = agentslot.Chain[AgentHook]("agent.hook")

// AgentHook can propose follow-on input before a Run is closed and observe
// facts after a commit. The Runtime remains the sole state authority.
type AgentHook interface {
	BeforeRunComplete(context.Context, RunCompleteView) (FollowOnProposal, error)
	AfterCommit(context.Context, CommitView) error
}

// RunCompleteView is read-only evidence presented to a hook.
type RunCompleteView struct {
	SessionID agent.SessionID
	RunID     agent.RunID
	Revision  agent.Revision
	Messages  []agent.Message
}

// FollowOnProposal contains only additional input; it cannot directly mutate
// Queue, History, Context, Run state, or cancellation.
type FollowOnProposal struct {
	Messages []agent.Message
}

// CommitView is read-only post-commit evidence.
type CommitView struct {
	SessionID agent.SessionID
	RunID     agent.RunID
	Revision  agent.Revision
}
