// Package cli provides a line-oriented GatewayChannel backed only by the fixed
// GatewayAccess contract. It is a renderer and transport adapter, not an
// alternate Agent backend.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/session"
)

const defaultMaxLineBytes = 1 << 20

type Config struct {
	AgentID      agent.AgentID
	WorkspaceID  agent.WorkspaceID
	SessionID    agent.SessionID
	Actor        agent.ActorIdentity
	Input        io.ReadCloser
	Output       io.Writer
	ErrorOutput  io.Writer
	MaxLineBytes int
}

type Channel struct {
	mu         sync.Mutex
	config     Config
	access     interaction.GatewayAccess
	bound      bool
	starting   bool
	started    bool
	session    agent.SessionID
	revision   agent.Revision
	cancel     context.CancelFunc
	done       chan struct{}
	err        error
	closeInput sync.Once
}

var _ interaction.GatewayChannel = (*Channel)(nil)

func New(config Config) (*Channel, error) {
	if config.Input == nil || config.Output == nil || config.ErrorOutput == nil {
		return nil, errors.New("cli: input, output, and error output are required")
	}
	resuming := config.SessionID.Valid()
	creating := config.AgentID.Valid() && config.WorkspaceID.Valid()
	if resuming == creating {
		return nil, errors.New("cli: configure either one SessionID or one AgentID/WorkspaceID pair")
	}
	if config.MaxLineBytes == 0 {
		config.MaxLineBytes = defaultMaxLineBytes
	}
	if config.MaxLineBytes <= 0 {
		return nil, errors.New("cli: MaxLineBytes must be positive")
	}
	if config.Actor == (agent.ActorIdentity{}) {
		config.Actor = agent.ActorIdentity{Kind: agent.ActorLocalUser, ID: "cli"}
	}
	if !config.Actor.Valid() {
		return nil, errors.New("cli: Actor must be a valid local, remote, service, or agent identity")
	}
	return &Channel{config: config, done: make(chan struct{})}, nil
}

func (e *Channel) Bind(access interaction.GatewayAccess) error {
	if nilGateway(access) {
		return errors.New("cli: GatewayAccess is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.bound {
		return errors.New("cli: Channel is already bound")
	}
	e.access = access
	e.bound = true
	return nil
}

func (e *Channel) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if !e.bound || nilGateway(e.access) {
		e.mu.Unlock()
		return errors.New("cli: Channel is not bound")
	}
	if e.started || e.starting {
		e.mu.Unlock()
		return errors.New("cli: Channel is already starting or started")
	}
	e.starting = true
	access := e.access
	e.mu.Unlock()

	var opened interaction.SessionOpened
	var err error
	if e.config.SessionID.Valid() {
		opened, err = access.ResumeSession(ctx, interaction.ResumeSessionRequest{SessionID: e.config.SessionID})
	} else {
		opened, err = access.CreateSession(ctx, interaction.CreateSessionRequest{AgentID: e.config.AgentID, WorkspaceID: e.config.WorkspaceID})
	}
	if err != nil {
		e.mu.Lock()
		e.starting = false
		e.mu.Unlock()
		return fmt.Errorf("cli: open Session: %w", err)
	}
	if _, err := fmt.Fprintf(e.config.Output, "session %s revision %d\n", opened.SessionID, opened.Revision); err != nil {
		e.mu.Lock()
		e.starting = false
		e.mu.Unlock()
		return fmt.Errorf("cli: write Session header: %w", err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.starting = false
	e.session = opened.SessionID
	e.revision = opened.Revision
	e.cancel = cancel
	e.started = true
	e.mu.Unlock()
	go e.run(runContext)
	return nil
}

func (e *Channel) Stop(ctx context.Context) error {
	e.mu.Lock()
	if !e.started {
		e.mu.Unlock()
		return nil
	}
	cancel := e.cancel
	done := e.done
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.closeInput.Do(func() { _ = e.config.Input.Close() })
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Channel) Done() <-chan struct{} { return e.done }

func (e *Channel) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func (e *Channel) run(ctx context.Context) {
	defer close(e.done)
	scanner := bufio.NewScanner(e.config.Input)
	scanner.Buffer(make([]byte, 64<<10), e.config.MaxLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		quit, err := e.handleLine(ctx, line)
		if err != nil {
			fmt.Fprintf(e.config.ErrorOutput, "error: %v\n", err)
		}
		if quit || ctx.Err() != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		e.mu.Lock()
		e.err = fmt.Errorf("cli: read input: %w", err)
		e.mu.Unlock()
	}
}

func (e *Channel) handleLine(ctx context.Context, line string) (bool, error) {
	if !strings.HasPrefix(line, "/") {
		return false, e.send(ctx, line)
	}
	commandLine := strings.TrimPrefix(line, "/")
	key, argumentsText, hasArguments := strings.Cut(commandLine, " ")
	if key == "" || strings.ContainsAny(key, "\t\r\n") {
		return false, errors.New("invalid slash command")
	}
	argumentsText = strings.TrimSpace(argumentsText)
	switch key {
	case "quit":
		if hasArguments && argumentsText != "" {
			return false, errors.New("/quit does not accept arguments")
		}
		return true, nil
	case "help":
		if hasArguments && argumentsText != "" {
			return false, errors.New("/help does not accept arguments")
		}
		return false, e.help(ctx)
	case "cancel":
		if hasArguments && argumentsText != "" {
			return false, errors.New("/cancel does not accept arguments")
		}
		return false, e.cancelRun(ctx)
	case "pending":
		if hasArguments && argumentsText != "" {
			return false, errors.New("/pending does not accept arguments")
		}
		return false, e.runPending(ctx)
	default:
		return false, e.invokeCommand(ctx, key, argumentsText)
	}
}

func (e *Channel) send(ctx context.Context, text string) error {
	result, err := e.access.SendAndWait(ctx, interaction.SendRequest{
		SessionID: e.session, ExpectedRevision: e.revision, Actor: e.config.Actor,
		Input: agent.MessageInput{Parts: []agent.MessagePart{{Kind: agent.PartText, Text: text}}},
	})
	if err != nil {
		return err
	}
	e.revision = result.Revision
	e.renderMessages(result.AssistantMessages)
	return nil
}

func (e *Channel) renderMessages(messages []agent.Message) {
	for _, message := range messages {
		for _, part := range message.Parts {
			switch part.Kind {
			case agent.PartText:
				fmt.Fprintln(e.config.Output, part.Text)
			case agent.PartAttachment:
				fmt.Fprintf(e.config.Output, "[attachment id=%s media_type=%s name=%s]\n", part.AttachmentID, part.MediaType, part.Name)
			}
		}
	}
}

func (e *Channel) invokeCommand(ctx context.Context, key, argumentsText string) error {
	var arguments json.RawMessage
	if argumentsText != "" {
		arguments = json.RawMessage(argumentsText)
		if !json.Valid(arguments) {
			return errors.New("slash command arguments must be valid JSON")
		}
	}
	result, err := e.access.InvokeCommand(ctx, interaction.CommandInvocation{
		Scope: interaction.CommandScope{AgentID: e.config.AgentID, WorkspaceID: e.config.WorkspaceID, SessionID: e.session},
		Key:   key, ExpectedRevision: e.revision, Actor: e.config.Actor, Arguments: arguments,
	})
	if err != nil {
		return err
	}
	if result.Revision > 0 {
		e.revision = result.Revision
	}
	if len(result.Data) > 0 {
		fmt.Fprintln(e.config.Output, string(result.Data))
	}
	return nil
}

func (e *Channel) help(ctx context.Context) error {
	descriptors, err := e.access.Commands(ctx, interaction.CommandScope{
		AgentID: e.config.AgentID, WorkspaceID: e.config.WorkspaceID, SessionID: e.session,
	})
	if err != nil {
		return err
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Key < descriptors[j].Key })
	fmt.Fprintln(e.config.Output, "/cancel - cancel the active Run")
	fmt.Fprintln(e.config.Output, "/pending - execute queued normal input")
	fmt.Fprintln(e.config.Output, "/quit - close this CLI")
	for _, descriptor := range descriptors {
		fmt.Fprintf(e.config.Output, "/%s - %s\n", descriptor.Key, descriptor.Description)
	}
	return nil
}

func (e *Channel) cancelRun(ctx context.Context) error {
	if err := e.access.Cancel(ctx, interaction.CancelRequest{SessionID: e.session, ExpectedRevision: e.revision, Actor: e.config.Actor}); err != nil {
		return err
	}
	_, err := e.refreshAfterIdle(ctx)
	return err
}

func (e *Channel) runPending(ctx context.Context) error {
	receipt, err := e.access.RunPending(ctx, interaction.RunPendingRequest{SessionID: e.session, ExpectedRevision: e.revision, Actor: e.config.Actor})
	if err != nil {
		return err
	}
	e.revision = receipt.Revision
	snapshot, err := e.refreshAfterIdle(ctx)
	if err != nil {
		return err
	}
	history, err := e.historyThroughRunStart(ctx, snapshot, receipt.RunID)
	if err != nil {
		return err
	}
	messages := make([]agent.Message, 0)
	for _, fact := range history {
		if fact.Message != nil && fact.Message.RunID == receipt.RunID && fact.Message.Role == agent.RoleAssistant {
			messages = append(messages, *fact.Message)
		}
	}
	e.renderMessages(messages)
	return nil
}

func (e *Channel) historyThroughRunStart(ctx context.Context, view interaction.SessionView, runID agent.RunID) ([]session.HistoryFact, error) {
	history := append([]session.HistoryFact(nil), view.RecentHistory...)
	if runStarted(history, runID) || !view.HasMoreHistory {
		return history, nil
	}
	if len(history) == 0 || history[0].Sequence == 0 {
		return nil, errors.New("cli: SessionView cannot page older History without a sequence cursor")
	}
	before := history[0].Sequence
	for {
		page, err := e.access.History(ctx, interaction.HistoryRequest{
			SessionID: e.session, BeforeHistorySequence: before, StepLimit: 100,
		})
		if err != nil {
			return nil, err
		}
		if len(page.Facts) == 0 {
			return history, nil
		}
		next := page.Facts[0].Sequence
		if next == 0 || next >= before {
			return nil, errors.New("cli: Gateway returned a non-decreasing History cursor")
		}
		history = append(append([]session.HistoryFact(nil), page.Facts...), history...)
		if runStarted(page.Facts, runID) || !page.HasMore {
			return history, nil
		}
		before = next
	}
}

func runStarted(history []session.HistoryFact, runID agent.RunID) bool {
	for _, fact := range history {
		if fact.Run != nil && fact.Run.RunID == runID && fact.Run.Kind == session.RunStarted {
			return true
		}
	}
	return false
}

func (e *Channel) refreshAfterIdle(ctx context.Context) (interaction.SessionView, error) {
	if err := e.access.WhenIdle(ctx, interaction.WhenIdleRequest{SessionID: e.session}); err != nil {
		return interaction.SessionView{}, err
	}
	snapshot, err := e.access.View(ctx, interaction.SessionViewRequest{SessionID: e.session})
	if err != nil {
		return interaction.SessionView{}, err
	}
	e.revision = snapshot.Revision
	return snapshot, nil
}

func nilGateway(value interaction.GatewayAccess) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
