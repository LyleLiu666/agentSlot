package grpcchannel

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type wireRequest struct {
	Profile   string          `json:"profile"`
	Operation Operation       `json:"operation"`
	Payload   json.RawMessage `json:"payload"`
}

type wireResponse struct {
	Profile string          `json:"profile"`
	Payload json.RawMessage `json:"payload"`
}

func (c *Channel) call(ctx context.Context, value *wrapperspb.BytesValue) (*wrapperspb.BytesValue, error) {
	request, err := c.decodeRequest(value)
	if err != nil {
		return nil, encodeError(err)
	}
	var response any
	switch request.Operation {
	case OperationListSessions:
		response, err = dispatch(c, ctx, request, func(value interaction.ListSessionsRequest) RequestScope {
			return RequestScope{Operation: request.Operation, AgentID: value.AgentID, WorkspaceID: value.WorkspaceID}
		}, nil, c.access.ListSessions)
	case OperationCreateSession:
		response, err = dispatch(c, ctx, request, func(value interaction.CreateSessionRequest) RequestScope {
			return RequestScope{Operation: request.Operation, AgentID: value.AgentID, WorkspaceID: value.WorkspaceID}
		}, nil, c.access.CreateSession)
	case OperationResumeSession:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.ResumeSessionRequest](request.Operation, func(value interaction.ResumeSessionRequest) agent.SessionID { return value.SessionID }), nil, c.access.ResumeSession)
	case OperationForkSession:
		response, err = dispatch(c, ctx, request, func(value interaction.ForkSessionRequest) RequestScope {
			return RequestScope{Operation: request.Operation, AgentID: value.AgentID, WorkspaceID: value.WorkspaceID, SessionID: value.SourceSessionID}
		}, nil, c.access.ForkSession)
	case OperationStartSessionFromSummary:
		response, err = dispatch(c, ctx, request, func(value interaction.SummarySessionRequest) RequestScope {
			return RequestScope{Operation: request.Operation, AgentID: value.AgentID, WorkspaceID: value.WorkspaceID, SessionID: value.SourceSessionID}
		}, nil, c.access.StartSessionFromSummary)
	case OperationSend:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.SendRequest](request.Operation, func(value interaction.SendRequest) agent.SessionID { return value.SessionID }), func(value *interaction.SendRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.Send)
	case OperationSendAndWait:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.SendRequest](request.Operation, func(value interaction.SendRequest) agent.SessionID { return value.SessionID }), func(value *interaction.SendRequest, actor agent.ActorIdentity) { value.Actor = actor }, func(ctx context.Context, value interaction.SendRequest) (interaction.RunResult, error) {
			detached, cancel := c.operationContext(ctx, true)
			defer cancel()
			return c.access.SendAndWait(detached, value)
		})
	case OperationSteer:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.SteerRequest](request.Operation, func(value interaction.SteerRequest) agent.SessionID { return value.SessionID }), func(value *interaction.SteerRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.Steer)
	case OperationRunPending:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.RunPendingRequest](request.Operation, func(value interaction.RunPendingRequest) agent.SessionID { return value.SessionID }), func(value *interaction.RunPendingRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.RunPending)
	case OperationCancel:
		response, err = dispatchEmpty(c, ctx, request, sessionScope[interaction.CancelRequest](request.Operation, func(value interaction.CancelRequest) agent.SessionID { return value.SessionID }), func(value *interaction.CancelRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.Cancel)
	case OperationWhenIdle:
		response, err = dispatchEmpty(c, ctx, request, sessionScope[interaction.WhenIdleRequest](request.Operation, func(value interaction.WhenIdleRequest) agent.SessionID { return value.SessionID }), nil, c.access.WhenIdle)
	case OperationEditQueued:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.EditQueuedRequest](request.Operation, func(value interaction.EditQueuedRequest) agent.SessionID { return value.SessionID }), func(value *interaction.EditQueuedRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.EditQueued)
	case OperationDeleteQueued:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.DeleteQueuedRequest](request.Operation, func(value interaction.DeleteQueuedRequest) agent.SessionID { return value.SessionID }), func(value *interaction.DeleteQueuedRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.DeleteQueued)
	case OperationReclassifyQueued:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.ReclassifyQueuedRequest](request.Operation, func(value interaction.ReclassifyQueuedRequest) agent.SessionID { return value.SessionID }), func(value *interaction.ReclassifyQueuedRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.ReclassifyQueued)
	case OperationModelConfig:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.ModelConfigRequest](request.Operation, func(value interaction.ModelConfigRequest) agent.SessionID { return value.SessionID }), nil, c.access.ModelConfig)
	case OperationUpdateModelConfig:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.UpdateModelConfigRequest](request.Operation, func(value interaction.UpdateModelConfigRequest) agent.SessionID { return value.SessionID }), func(value *interaction.UpdateModelConfigRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.UpdateModelConfig)
	case OperationView:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.SessionViewRequest](request.Operation, func(value interaction.SessionViewRequest) agent.SessionID { return value.SessionID }), nil, c.access.View)
	case OperationHistory:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.HistoryRequest](request.Operation, func(value interaction.HistoryRequest) agent.SessionID { return value.SessionID }), nil, c.access.History)
	case OperationExtensionDiagnostics:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.ExtensionDiagnosticsRequest](request.Operation, func(value interaction.ExtensionDiagnosticsRequest) agent.SessionID { return value.SessionID }), nil, c.access.ExtensionDiagnostics)
	case OperationCommands:
		response, err = dispatch(c, ctx, request, func(value interaction.CommandScope) RequestScope {
			return RequestScope{Operation: request.Operation, AgentID: value.AgentID, WorkspaceID: value.WorkspaceID, SessionID: value.SessionID}
		}, nil, c.access.Commands)
	case OperationInvokeCommand:
		response, err = dispatch(c, ctx, request, func(value interaction.CommandInvocation) RequestScope {
			return RequestScope{Operation: request.Operation, AgentID: value.Scope.AgentID, WorkspaceID: value.Scope.WorkspaceID, SessionID: value.Scope.SessionID}
		}, func(value *interaction.CommandInvocation, actor agent.ActorIdentity) { value.Actor = actor }, c.access.InvokeCommand)
	case OperationCloseSession:
		response, err = dispatch(c, ctx, request, sessionScope[interaction.CloseSessionRequest](request.Operation, func(value interaction.CloseSessionRequest) agent.SessionID { return value.SessionID }), func(value *interaction.CloseSessionRequest, actor agent.ActorIdentity) { value.Actor = actor }, c.access.CloseSession)
	default:
		err = agent.NewCodedError(agent.ErrorInvalidInput, "", "grpcchannel.call", "unknown Gateway operation", nil)
	}
	if err != nil {
		return nil, encodeError(err)
	}
	return c.encodeResponse(response)
}

func (c *Channel) subscribe(value *wrapperspb.BytesValue, stream grpc.ServerStream) error {
	request, err := c.decodeRequest(value)
	if err != nil {
		return encodeError(err)
	}
	if request.Operation != OperationSubscribe {
		return encodeError(agent.NewError(agent.ErrorInvalidInput, "grpcchannel.subscribe", "invalid streaming operation", nil))
	}
	var input interaction.SubscribeRequest
	if err := json.Unmarshal(request.Payload, &input); err != nil {
		return encodeError(agent.NewError(agent.ErrorInvalidInput, "grpcchannel.subscribe", "invalid request payload", nil))
	}
	if _, err := c.authenticateAndAuthorize(stream.Context(), RequestScope{Operation: request.Operation, SessionID: input.SessionID}); err != nil {
		return encodeError(err)
	}
	backend, err := c.access.Subscribe(stream.Context(), input)
	if err != nil {
		return encodeError(err)
	}
	defer backend.Close()
	for {
		event, err := backend.Recv(stream.Context())
		if err != nil {
			if errors.Is(err, interaction.ErrEventStreamClosed) || errors.Is(err, io.EOF) {
				return nil
			}
			return encodeError(err)
		}
		response, err := c.encodeResponse(event)
		if err != nil {
			return encodeError(err)
		}
		if err := stream.SendMsg(response); err != nil {
			return err
		}
	}
}

func dispatch[Request any, Response any](c *Channel, ctx context.Context, wire wireRequest, scope func(Request) RequestScope, setActor func(*Request, agent.ActorIdentity), invoke func(context.Context, Request) (Response, error)) (Response, error) {
	var request Request
	if err := json.Unmarshal(wire.Payload, &request); err != nil {
		return *new(Response), agent.NewError(agent.ErrorInvalidInput, "grpcchannel.dispatch", "invalid request payload", nil)
	}
	actor, err := c.authenticateAndAuthorize(ctx, scope(request))
	if err != nil {
		return *new(Response), err
	}
	if setActor != nil {
		setActor(&request, actor)
	}
	return invoke(ctx, request)
}

func dispatchEmpty[Request any](c *Channel, ctx context.Context, wire wireRequest, scope func(Request) RequestScope, setActor func(*Request, agent.ActorIdentity), invoke func(context.Context, Request) error) (struct{}, error) {
	return dispatch(c, ctx, wire, scope, setActor, func(ctx context.Context, request Request) (struct{}, error) {
		return struct{}{}, invoke(ctx, request)
	})
}

func sessionScope[Request any](operation Operation, sessionID func(Request) agent.SessionID) func(Request) RequestScope {
	return func(value Request) RequestScope {
		return RequestScope{Operation: operation, SessionID: sessionID(value)}
	}
}

func (c *Channel) authenticateAndAuthorize(ctx context.Context, scope RequestScope) (agent.ActorIdentity, error) {
	actor, err := c.config.Authenticate(ctx)
	if err != nil {
		return agent.ActorIdentity{}, err
	}
	if !actor.Valid() || actor.Kind == agent.ActorLocalUser {
		return agent.ActorIdentity{}, agent.NewError(agent.ErrorUnauthorized, "grpcchannel.authenticate", "remote identity is invalid", nil)
	}
	if err := c.config.Authorize(ctx, actor, scope); err != nil {
		return agent.ActorIdentity{}, err
	}
	return actor, nil
}

func (c *Channel) decodeRequest(value *wrapperspb.BytesValue) (wireRequest, error) {
	if value == nil || len(value.Value) == 0 || len(value.Value) > c.config.MaxRequestBytes {
		return wireRequest{}, agent.NewError(agent.ErrorInvalidInput, "grpcchannel.decode", "invalid request size", nil)
	}
	var request wireRequest
	if err := json.Unmarshal(value.Value, &request); err != nil || request.Profile != Profile || len(request.Payload) == 0 {
		return wireRequest{}, agent.NewError(agent.ErrorInvalidInput, "grpcchannel.decode", "invalid request envelope", nil)
	}
	return request, nil
}

func (c *Channel) encodeResponse(value any) (*wrapperspb.BytesValue, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, agent.NewError(agent.ErrorInternal, "grpcchannel.encode", "response cannot be encoded", nil)
	}
	encoded, err := json.Marshal(wireResponse{Profile: Profile, Payload: payload})
	if err != nil || len(encoded) > c.config.MaxResponseBytes {
		return nil, agent.NewError(agent.ErrorInternal, "grpcchannel.encode", "response exceeds configured boundary", nil)
	}
	return wrapperspb.Bytes(encoded), nil
}
