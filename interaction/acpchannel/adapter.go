package acpchannel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/session"
	acp "github.com/coder/acp-go-sdk"
)

type adapter struct {
	config       Config
	access       interaction.GatewayAccess
	lifecycle    context.Context
	connectionMu sync.RWMutex
	connection   *acp.AgentSideConnection
	turnsMu      sync.Mutex
	turns        map[agent.SessionID]*sync.Mutex
}

func newAdapter(config Config, access interaction.GatewayAccess, lifecycle context.Context) *adapter {
	return &adapter{config: config, access: access, lifecycle: lifecycle, turns: make(map[agent.SessionID]*sync.Mutex)}
}

func (a *adapter) setConnection(connection *acp.AgentSideConnection) {
	a.connectionMu.Lock()
	a.connection = connection
	a.connectionMu.Unlock()
}

func (a *adapter) authorize(ctx context.Context, operation Operation, sessionID agent.SessionID) error {
	return a.config.Authorize(ctx, a.config.Actor, RequestScope{Operation: operation, AgentID: a.config.AgentID, WorkspaceID: a.config.WorkspaceID, SessionID: sessionID})
}

func (a *adapter) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	if params.ProtocolVersion != acp.ProtocolVersionNumber {
		return acp.InitializeResponse{}, acp.NewInvalidParams(map[string]any{"protocolVersion": params.ProtocolVersion})
	}
	if err := a.authorize(ctx, OperationInitialize, ""); err != nil {
		return acp.InitializeResponse{}, err
	}
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo:       &acp.Implementation{Name: a.config.AgentName, Version: a.config.AgentVersion},
		AuthMethods:     []acp.AuthMethod{},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession:         false,
			PromptCapabilities:  acp.PromptCapabilities{Image: false, Audio: false, EmbeddedContext: false},
			SessionCapabilities: acp.SessionCapabilities{List: &acp.SessionListCapabilities{}, Resume: &acp.SessionResumeCapabilities{}, Close: &acp.SessionCloseCapabilities{}},
		},
	}, nil
}

func (*adapter) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, acp.NewMethodNotFound(acp.AgentMethodAuthenticate)
}
func (*adapter) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}
func (*adapter) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}
func (*adapter) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func (a *adapter) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if err := a.validateSetup(params.Cwd, params.AdditionalDirectories, len(params.McpServers)); err != nil {
		return acp.NewSessionResponse{}, err
	}
	if err := a.authorize(ctx, OperationCreateSession, ""); err != nil {
		return acp.NewSessionResponse{}, err
	}
	opened, err := a.access.CreateSession(ctx, interaction.CreateSessionRequest{AgentID: a.config.AgentID, WorkspaceID: a.config.WorkspaceID})
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	return acp.NewSessionResponse{SessionId: acp.SessionId(opened.SessionID)}, nil
}

func (a *adapter) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	sessionID := agent.SessionID(params.SessionId)
	if !sessionID.Valid() {
		return acp.ResumeSessionResponse{}, acp.NewInvalidParams("sessionId is required")
	}
	if err := a.validateSetup(params.Cwd, params.AdditionalDirectories, len(params.McpServers)); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if err := a.authorize(ctx, OperationResumeSession, sessionID); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	_, err := a.access.ResumeSession(ctx, interaction.ResumeSessionRequest{SessionID: sessionID})
	return acp.ResumeSessionResponse{}, err
}

func (a *adapter) validateSetup(cwd string, additional []string, mcpCount int) error {
	if cwd != a.config.WorkingDirectory {
		return acp.NewInvalidParams(map[string]any{"cwd": "does not match the configured workspace"})
	}
	if len(additional) != 0 {
		return acp.NewInvalidParams("additionalDirectories are not supported")
	}
	if mcpCount != 0 {
		return acp.NewInvalidParams("ACP-managed MCP servers are not supported")
	}
	return nil
}

func (a *adapter) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if params.Cwd != nil && *params.Cwd != a.config.WorkingDirectory {
		return acp.ListSessionsResponse{}, acp.NewInvalidParams("cwd does not match the configured workspace")
	}
	if err := a.authorize(ctx, OperationListSessions, ""); err != nil {
		return acp.ListSessionsResponse{}, err
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	result, err := a.access.ListSessions(ctx, interaction.ListSessionsRequest{AgentID: a.config.AgentID, WorkspaceID: a.config.WorkspaceID, Cursor: cursor})
	if err != nil {
		return acp.ListSessionsResponse{}, err
	}
	response := acp.ListSessionsResponse{Sessions: make([]acp.SessionInfo, 0, len(result.Sessions))}
	for _, item := range result.Sessions {
		updated := item.UpdatedAt.UTC().Format(time.RFC3339Nano)
		response.Sessions = append(response.Sessions, acp.SessionInfo{SessionId: acp.SessionId(item.SessionID), Cwd: a.config.WorkingDirectory, UpdatedAt: &updated})
	}
	if result.NextCursor != "" {
		response.NextCursor = &result.NextCursor
	}
	return response, nil
}

func (a *adapter) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	sessionID := agent.SessionID(params.SessionId)
	if !sessionID.Valid() {
		return acp.PromptResponse{}, acp.NewInvalidParams("sessionId is required")
	}
	input, err := projectPrompt(params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.authorize(ctx, OperationPrompt, sessionID); err != nil {
		return acp.PromptResponse{}, err
	}
	turn := a.turnLock(sessionID)
	turn.Lock()
	defer turn.Unlock()
	view, err := a.access.View(ctx, interaction.SessionViewRequest{SessionID: sessionID})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	result, err := a.access.SendAndWait(a.lifecycle, interaction.SendRequest{SessionID: sessionID, ExpectedRevision: view.Revision, Actor: a.config.Actor, Input: input})
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.publish(result); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: stopReason(result.Outcome)}, nil
}

func (a *adapter) turnLock(sessionID agent.SessionID) *sync.Mutex {
	a.turnsMu.Lock()
	defer a.turnsMu.Unlock()
	if a.turns[sessionID] == nil {
		a.turns[sessionID] = &sync.Mutex{}
	}
	return a.turns[sessionID]
}

func projectPrompt(blocks []acp.ContentBlock) (agent.MessageInput, error) {
	parts := make([]agent.MessagePart, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			if block.Text.Text == "" {
				return agent.MessageInput{}, acp.NewInvalidParams("empty text content")
			}
			parts = append(parts, agent.MessagePart{Kind: agent.PartText, Text: block.Text.Text})
		case block.ResourceLink != nil:
			link := block.ResourceLink
			name := strings.TrimSpace(link.Name)
			if name == "" {
				name = link.Uri
			}
			name = strings.NewReplacer("[", "\\[", "]", "\\]").Replace(name)
			text := fmt.Sprintf("[%s](%s)", name, link.Uri)
			if link.Description != nil && strings.TrimSpace(*link.Description) != "" {
				text += " — " + strings.TrimSpace(*link.Description)
			}
			parts = append(parts, agent.MessagePart{Kind: agent.PartText, Text: text})
		default:
			return agent.MessageInput{}, acp.NewInvalidParams("content type is not supported by this ACP profile")
		}
	}
	input := agent.MessageInput{Parts: parts}
	if !input.Valid() {
		return agent.MessageInput{}, acp.NewInvalidParams("prompt is empty or invalid")
	}
	return input, nil
}

func (a *adapter) publish(result interaction.RunResult) error {
	a.connectionMu.RLock()
	connection := a.connection
	a.connectionMu.RUnlock()
	if connection == nil {
		return fmt.Errorf("acpchannel: connection is not ready")
	}
	for _, message := range result.AssistantMessages {
		for _, part := range message.Parts {
			text := part.Text
			if part.Kind == agent.PartAttachment {
				text = fmt.Sprintf("[attachment: %s; id=%s; media_type=%s]", part.Name, part.AttachmentID, part.MediaType)
			}
			if text == "" {
				continue
			}
			err := connection.SessionUpdate(a.lifecycle, acp.SessionNotification{SessionId: acp.SessionId(result.SessionID), Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: text}}, SessionUpdate: "agent_message_chunk"}}})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func stopReason(outcome session.RunFactKind) acp.StopReason {
	switch outcome {
	case session.RunCompleted:
		return acp.StopReasonEndTurn
	case session.RunCanceled:
		return acp.StopReasonCancelled
	default:
		return acp.StopReasonRefusal
	}
}

func (a *adapter) Cancel(ctx context.Context, params acp.CancelNotification) error {
	sessionID := agent.SessionID(params.SessionId)
	if !sessionID.Valid() {
		return acp.NewInvalidParams("sessionId is required")
	}
	if err := a.authorize(ctx, OperationCancel, sessionID); err != nil {
		return err
	}
	view, err := a.access.View(ctx, interaction.SessionViewRequest{SessionID: sessionID})
	if err != nil {
		return err
	}
	return a.access.Cancel(ctx, interaction.CancelRequest{SessionID: sessionID, ExpectedRevision: view.Revision, Actor: a.config.Actor})
}

func (a *adapter) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	sessionID := agent.SessionID(params.SessionId)
	if !sessionID.Valid() {
		return acp.CloseSessionResponse{}, acp.NewInvalidParams("sessionId is required")
	}
	if err := a.authorize(ctx, OperationCloseSession, sessionID); err != nil {
		return acp.CloseSessionResponse{}, err
	}
	view, err := a.access.View(ctx, interaction.SessionViewRequest{SessionID: sessionID})
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}
	err = a.access.CloseSession(ctx, interaction.CloseSessionRequest{SessionID: sessionID, ExpectedRevision: view.Revision, Actor: a.config.Actor})
	return acp.CloseSessionResponse{}, err
}
