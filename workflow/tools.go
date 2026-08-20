package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/tool"
)

const (
	ToolSpawn   = "agent.spawn"
	ToolStatus  = "agent.status"
	ToolWait    = "agent.wait"
	ToolSend    = "agent.send"
	ToolClose   = "agent.close"
	ToolMailbox = "agent.mailbox"
)

func ToolKeys() []string {
	return []string{ToolSpawn, ToolStatus, ToolWait, ToolSend, ToolClose, ToolMailbox}
}

func NewToolModule(id string) (agentslot.Module, error) {
	if id == "" {
		return nil, errors.New("workflow: tool module ID is required")
	}
	return workflowToolModule{id: id}, nil
}

type workflowToolModule struct{ id string }

func (m workflowToolModule) ID() string { return m.id }
func (m workflowToolModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{agentslot.RequireOne(SchedulerSlot), agentslot.RequireOne(MailboxSlot)}
}
func (m workflowToolModule) Register(reg agentslot.Registrar) error {
	contributions := make([]agentslot.Contribution, 0, len(ToolKeys()))
	for _, key := range ToolKeys() {
		key := key
		contributions = append(contributions, agentslot.AddWith(tool.ToolSlot, key,
			func(resolver agentslot.Resolver) (tool.Tool, error) {
				scheduler, err := agentslot.ResolveOne(resolver, SchedulerSlot)
				if err != nil {
					return nil, err
				}
				mailbox, err := agentslot.ResolveOne(resolver, MailboxSlot)
				if err != nil {
					return nil, err
				}
				return newWorkflowTool(key, scheduler, mailbox)
			}))
	}
	return reg.Contribute(contributions...)
}

type workflowTool struct {
	definition tool.Definition
	scheduler  Scheduler
	mailbox    Mailbox
}

func newWorkflowTool(name string, scheduler Scheduler, mailbox Mailbox) (*workflowTool, error) {
	raw, description, err := workflowToolSchema(name)
	if err != nil {
		return nil, err
	}
	schema, err := tool.ParseInputSchema([]byte(raw))
	if err != nil {
		return nil, err
	}
	return &workflowTool{definition: tool.Definition{Name: name, Description: description, InputSchema: schema}, scheduler: scheduler, mailbox: mailbox}, nil
}

func (t *workflowTool) Definition() tool.Definition       { return t.definition }
func (*workflowTool) ParallelSafety() tool.ParallelSafety { return tool.Serial }

func (t *workflowTool) Invoke(ctx context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	var value any
	var err error
	switch t.definition.Name {
	case ToolSpawn:
		var args struct {
			ProviderKey string            `json:"provider_key"`
			Instruction string            `json:"instruction"`
			Metadata    map[string]string `json:"metadata,omitempty"`
		}
		if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
			value, err = t.scheduler.Spawn(ctx, SpawnRequest{
				ProviderKey: args.ProviderKey, Instruction: args.Instruction, Metadata: args.Metadata,
				Parent: Parent{SessionID: invocation.SessionID, RunID: invocation.RunID, StepID: invocation.StepID},
			})
		}
	case ToolStatus:
		var args struct {
			JobID string `json:"job_id"`
		}
		if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
			var ok bool
			value, ok, err = t.scheduler.Get(ctx, args.JobID)
			if err == nil && !ok {
				err = errors.New("job not found")
			}
		}
	case ToolWait:
		var args struct {
			JobID        string `json:"job_id"`
			AfterVersion uint64 `json:"after_version"`
		}
		if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
			value, err = t.scheduler.Wait(ctx, args.JobID, args.AfterVersion)
		}
	case ToolSend:
		var args struct {
			JobID string `json:"job_id"`
			Body  string `json:"body"`
		}
		if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
			value, err = t.scheduler.Send(ctx, SendRequest{JobID: args.JobID, Body: args.Body})
		}
	case ToolClose:
		var args struct {
			JobID  string `json:"job_id"`
			Reason string `json:"reason"`
		}
		if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
			value, err = t.scheduler.Close(ctx, CloseRequest{JobID: args.JobID, Reason: args.Reason})
		}
	case ToolMailbox:
		var args struct {
			AfterSequence uint64 `json:"after_sequence"`
		}
		if err = json.Unmarshal(invocation.Call.Arguments, &args); err == nil {
			value, err = t.mailbox.List(ctx, Address{Kind: AddressSession, ID: string(invocation.SessionID)}, args.AfterSequence)
		}
	default:
		err = errors.New("unknown workflow tool")
	}
	if err != nil {
		return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed,
			Error: &tool.StructuredError{Code: "workflow_error", Message: "workflow operation failed"}}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed,
			Error: &tool.StructuredError{Code: "workflow_encoding_failed", Message: "workflow result could not be encoded"}}
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: encoded}
}

func workflowToolSchema(name string) (string, string, error) {
	switch name {
	case ToolSpawn:
		return `{"type":"object","additionalProperties":false,"properties":{"provider_key":{"type":"string","minLength":1},"instruction":{"type":"string","minLength":1},"metadata":{"type":"object","additionalProperties":{"type":"string"}}},"required":["provider_key","instruction"]}`, "Start an asynchronous child-agent job.", nil
	case ToolStatus:
		return jobIDSchema(), "Read one child-agent job status.", nil
	case ToolWait:
		return `{"type":"object","additionalProperties":false,"properties":{"job_id":{"type":"string","minLength":1},"after_version":{"type":"integer","minimum":0}},"required":["job_id","after_version"]}`, "Wait for a child-agent job change.", nil
	case ToolSend:
		return `{"type":"object","additionalProperties":false,"properties":{"job_id":{"type":"string","minLength":1},"body":{"type":"string","minLength":1}},"required":["job_id","body"]}`, "Send an addressed message to a running child-agent job.", nil
	case ToolClose:
		return `{"type":"object","additionalProperties":false,"properties":{"job_id":{"type":"string","minLength":1},"reason":{"type":"string","minLength":1}},"required":["job_id","reason"]}`, "Cancel a running child-agent job.", nil
	case ToolMailbox:
		return `{"type":"object","additionalProperties":false,"properties":{"after_sequence":{"type":"integer","minimum":0}},"required":["after_sequence"]}`, "Read durable child-agent results addressed to this Session.", nil
	default:
		return "", "", fmt.Errorf("workflow: unknown tool %q", name)
	}
}

func jobIDSchema() string {
	return `{"type":"object","additionalProperties":false,"properties":{"job_id":{"type":"string","minLength":1}},"required":["job_id"]}`
}
