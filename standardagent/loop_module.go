package standardagent

import (
	"context"
	"errors"

	agentslot "github.com/LyleLiu666/agentSlot"
	agentloop "github.com/LyleLiu666/agentSlot/loop"
)

const standardLoopModuleID = "standardagent.loop"

// standardLoopModule is the standard Agent profile's explicit fallback. A
// product-supplied AgentLoop contribution replaces it during Build.
type standardLoopModule struct{}

func (standardLoopModule) ID() string { return standardLoopModuleID }

func (standardLoopModule) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.SetDefault(agentloop.AgentLoopSlot, agentloop.AgentLoop(standardLoop{})))
}

type standardLoop struct{}

func (standardLoop) Run(ctx context.Context, run agentloop.Run) (agentloop.Outcome, error) {
	if run == nil {
		return agentloop.OutcomeFailed, errors.New("standardagent: Loop Run is required")
	}
	state := run.State()
	for {
		action := agentloop.Action{Kind: agentloop.ActionRequestModel}
		switch state {
		case agentloop.StateToolsReady:
			action.Kind = agentloop.ActionExecuteTools
		case agentloop.StateContinueReady:
			action.Kind = agentloop.ActionContinue
		case agentloop.StateReadyForModel:
		default:
			outcome := loopOutcomeFromState(state)
			if outcome.Terminal() {
				if _, err := run.Act(ctx, agentloop.Action{Kind: agentloop.ActionFinish, Outcome: outcome}); err != nil {
					return agentloop.OutcomeFailed, err
				}
				return outcome, nil
			}
			return agentloop.OutcomeFailed, errors.New("standardagent: Runtime returned an unknown Loop state")
		}
		var err error
		state, err = run.Act(ctx, action)
		if err != nil {
			return agentloop.OutcomeFailed, err
		}
	}
}

func loopOutcomeFromState(state agentloop.State) agentloop.Outcome {
	switch state {
	case agentloop.StateCompleted:
		return agentloop.OutcomeCompleted
	case agentloop.StateCanceled:
		return agentloop.OutcomeCanceled
	case agentloop.StateBudgetExceeded:
		return agentloop.OutcomeBudgetExceeded
	case agentloop.StateWaiting:
		return agentloop.OutcomeWaiting
	case agentloop.StateFailed:
		return agentloop.OutcomeFailed
	default:
		return agentloop.OutcomeContinue
	}
}

var _ agentloop.AgentLoop = standardLoop{}
