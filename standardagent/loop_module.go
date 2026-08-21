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
	for {
		outcome, err := run.Step(ctx)
		if err != nil || outcome != agentloop.OutcomeContinue {
			return outcome, err
		}
	}
}

var _ agentloop.AgentLoop = standardLoop{}
