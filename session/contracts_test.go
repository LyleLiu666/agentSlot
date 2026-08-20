package session_test

import (
	"context"
	"errors"
	"testing"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/session"
)

type manager struct{}

type testSession struct{}

func (testSession) ID() agent.SessionID                            { return "session-1" }
func (testSession) Revision() agent.Revision                       { return 1 }
func (testSession) View(context.Context) (session.Snapshot, error) { return session.Snapshot{}, nil }

func (manager) Create(context.Context, session.CreateRequest) (session.Session, error) {
	return testSession{}, nil
}

func TestRequiredSessionSlotsRejectMissingProviders(t *testing.T) {
	for name, requirement := range map[string]agentslot.Requirement{
		"manager": agentslot.RequireOne(session.ManagerSlot),
		"store":   agentslot.RequireOne(session.StoreSlot),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := agentslot.NewBuilder().Build(requirement)
			if !errors.Is(err, agentslot.ErrRequirementUnsatisfied) {
				t.Fatalf("Build() error = %v, want ErrRequirementUnsatisfied", err)
			}
		})
	}
}
func (manager) Resume(context.Context, session.ResumeRequest) (session.Session, error) {
	return testSession{}, nil
}
func (manager) Fork(context.Context, session.ForkRequest) (session.Session, error) {
	return testSession{}, nil
}
func (manager) StartFromSummary(context.Context, session.SummaryRequest) (session.Session, error) {
	return testSession{}, nil
}

type store struct{}

func (store) Create(context.Context, session.NewSession) (session.Snapshot, error) {
	return session.Snapshot{}, nil
}
func (store) Load(context.Context, session.SessionRef) (session.Snapshot, error) {
	return session.Snapshot{}, nil
}
func (store) Recover(context.Context, session.SessionRef) (session.Snapshot, error) {
	return session.Snapshot{}, nil
}
func (store) Commit(context.Context, session.CommitRequest) (session.Commit, error) {
	return session.Commit{}, nil
}
func (store) HistoryPage(context.Context, session.HistoryPageRequest) (session.HistoryPage, error) {
	return session.HistoryPage{}, nil
}

type module struct{}

func (module) ID() string { return "session.contracts" }
func (module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(
		agentslot.Set(session.ManagerSlot, session.SessionManager(manager{})),
		agentslot.Set(session.StoreSlot, session.SessionStore(store{})),
	)
}

func TestSessionContractsExposeTypedSlots(t *testing.T) {
	builder := agentslot.NewBuilder()
	if err := builder.Install(module{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assembly, err := builder.Build(
		agentslot.RequireOne(session.ManagerSlot),
		agentslot.RequireOne(session.StoreSlot),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := agentslot.Get(assembly, session.ManagerSlot); !ok {
		t.Fatal("session.manager contribution missing")
	}
	if _, ok := agentslot.Get(assembly, session.StoreSlot); !ok {
		t.Fatal("session.store contribution missing")
	}
}

func TestCommitValidationRequiresCASIdentityAndTypedMessageChange(t *testing.T) {
	request := session.CommitRequest{
		SessionID:        "session-1",
		ExpectedRevision: agent.Revision(4),
		IdempotencyKey:   "request-1",
		Changes: []session.Change{{
			Kind: session.AppendMessage,
			Message: &agent.Message{
				ID: "message-1", SessionID: "session-1", Role: agent.RoleUser,
				Parts: []agent.MessagePart{{Kind: agent.PartText, Text: "hello"}},
			},
		}},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid commit rejected: %v", err)
	}
	request.IdempotencyKey = ""
	if err := request.Validate(); err == nil {
		t.Fatal("commit without idempotency key accepted")
	}
}
