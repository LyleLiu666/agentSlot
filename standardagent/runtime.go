package standardagent

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/session"
)

type applicationRuntime struct {
	mu           sync.Mutex
	active       sync.WaitGroup
	dependencies runtimeDependencies
	registry     *runtimeRegistry
	coordinator  *runtimeCoordinator
	gateway      *gateway
	started      bool
}

func newApplicationRuntime(dependencies runtimeDependencies) *applicationRuntime {
	return &applicationRuntime{dependencies: dependencies}
}

func (r *applicationRuntime) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return agent.NewError(agent.ErrorConflict, "standardagent.start", "application runtime was already started", nil)
	}
	r.registry = newRuntimeRegistry()
	r.coordinator = &runtimeCoordinator{
		manager:    r.dependencies.manager,
		registry:   r.registry,
		components: r.dependencies.runtimeComponents(),
	}
	r.gateway = &gateway{runtime: r}
	r.started = true
	return nil
}

func (r *applicationRuntime) stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = false
	registry := r.registry
	r.mu.Unlock()

	// No new operation can enter after started becomes false. Waiting here
	// lets already-routed operations finish before the registry is drained.
	r.active.Wait()

	r.mu.Lock()
	r.registry = nil
	r.coordinator = nil
	r.gateway = nil
	r.mu.Unlock()
	if registry == nil {
		return nil
	}
	return registry.closeAll(ctx)
}

func (r *applicationRuntime) acquire() (*runtimeCoordinator, func(), error) {
	r.mu.Lock()
	if !r.started || r.coordinator == nil {
		r.mu.Unlock()
		return nil, nil, notStartedError()
	}
	r.active.Add(1)
	coordinator := r.coordinator
	r.mu.Unlock()
	return coordinator, r.active.Done, nil
}

func (r *applicationRuntime) command(key string) (interaction.InteractionCommand, bool) {
	for _, named := range r.dependencies.commands {
		if named.Key == key {
			return named.Value, true
		}
	}
	return nil, false
}

type runtimeRegistry struct {
	mu       sync.Mutex
	entries  map[agent.SessionID]runtimeAccess
	inflight map[agent.SessionID]*runtimeFlight
}

type runtimeFlight struct {
	done chan struct{}
	err  error
}

func newRuntimeRegistry() *runtimeRegistry {
	return &runtimeRegistry{
		entries:  make(map[agent.SessionID]runtimeAccess),
		inflight: make(map[agent.SessionID]*runtimeFlight),
	}
}

// getOrCreate is the application-owned single-flight boundary. A SessionID
// has one Runtime even when multiple Entrypoints resume it concurrently.
func (r *runtimeRegistry) getOrCreate(ctx context.Context, id agent.SessionID, create func() (runtimeAccess, error)) (runtimeAccess, error) {
	for {
		r.mu.Lock()
		if runtime, ok := r.entries[id]; ok {
			r.mu.Unlock()
			return runtime, nil
		}
		if flight, ok := r.inflight[id]; ok {
			done := flight.done
			r.mu.Unlock()
			select {
			case <-done:
				if flight.err != nil {
					if ctx.Err() == nil && (errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)) {
						continue
					}
					return nil, flight.err
				}
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		flight := &runtimeFlight{done: make(chan struct{})}
		r.inflight[id] = flight
		r.mu.Unlock()

		runtime, err := create()
		r.mu.Lock()
		delete(r.inflight, id)
		flight.err = err
		if err == nil {
			r.entries[id] = runtime
		}
		close(flight.done)
		r.mu.Unlock()
		return runtime, err
	}
}

func (r *runtimeRegistry) add(runtime runtimeAccess) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := runtime.id()
	if _, exists := r.entries[id]; exists {
		return agent.NewCodedError(agent.ErrorConflict, agent.CodeSessionAlreadyOpen, "runtime_registry.add", "Session already has an open Runtime", nil)
	}
	if _, creating := r.inflight[id]; creating {
		return agent.NewCodedError(agent.ErrorConflict, agent.CodeSessionAlreadyOpen, "runtime_registry.add", "Session Runtime is being opened", nil)
	}
	r.entries[id] = runtime
	return nil
}

func (r *runtimeRegistry) get(id agent.SessionID) (runtimeAccess, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	runtime, ok := r.entries[id]
	return runtime, ok
}

// remove waits for an in-progress creation of the same Session before
// removing it. Close can therefore never report success while a concurrent
// Resume is about to publish a Runtime behind it.
func (r *runtimeRegistry) remove(ctx context.Context, id agent.SessionID) (runtimeAccess, bool, error) {
	for {
		r.mu.Lock()
		if runtime, ok := r.entries[id]; ok {
			delete(r.entries, id)
			r.mu.Unlock()
			return runtime, true, nil
		}
		flight, inflight := r.inflight[id]
		if !inflight {
			r.mu.Unlock()
			return nil, false, nil
		}
		done := flight.done
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

func (r *runtimeRegistry) closeAll(ctx context.Context) error {
	r.mu.Lock()
	entries := make([]runtimeAccess, 0, len(r.entries))
	for _, runtime := range r.entries {
		entries = append(entries, runtime)
	}
	r.entries = make(map[agent.SessionID]runtimeAccess)
	r.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].id() < entries[j].id() })
	var errs []error
	for _, runtime := range entries {
		if err := runtime.close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type runtimeCoordinator struct {
	manager    session.SessionManager
	registry   *runtimeRegistry
	components *runtimeComponents
}

func (c *runtimeCoordinator) create(ctx context.Context, request interaction.CreateSessionRequest) (interaction.SessionOpened, error) {
	if !request.AgentID.Valid() || !request.WorkspaceID.Valid() {
		return interaction.SessionOpened{}, invalidInput("gateway.create_session", "AgentID and WorkspaceID are required")
	}
	if request.ModelConfig != nil {
		if err := request.ModelConfig.Validate(); err != nil {
			return interaction.SessionOpened{}, invalidInput("gateway.create_session", err.Error())
		}
	}
	s, err := c.manager.Create(ctx, session.CreateRequest{
		AgentID: request.AgentID, WorkspaceID: request.WorkspaceID, ModelConfig: request.ModelConfig,
	})
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	candidate, err := newRuntimeInstance(s, c.components)
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	if err := c.registerNew(ctx, candidate); err != nil {
		return interaction.SessionOpened{}, err
	}
	return opened(candidate), nil
}

func (c *runtimeCoordinator) resume(ctx context.Context, request interaction.ResumeSessionRequest) (interaction.SessionOpened, error) {
	if !request.SessionID.Valid() {
		return interaction.SessionOpened{}, invalidInput("gateway.resume_session", "SessionID is required")
	}
	runtime, err := c.registry.getOrCreate(ctx, request.SessionID, func() (runtimeAccess, error) {
		s, err := c.manager.Resume(ctx, session.ResumeRequest{SessionID: request.SessionID})
		if err != nil {
			return nil, err
		}
		runtime, err := newRuntimeInstance(s, c.components)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, runtime.close(context.WithoutCancel(ctx)))
		}
		if runtime.id() != request.SessionID {
			return nil, agent.NewError(agent.ErrorInternal, "gateway.resume_session", "SessionManager returned a different SessionID", nil)
		}
		return runtime, nil
	})
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	return opened(runtime), nil
}

func (c *runtimeCoordinator) fork(ctx context.Context, request interaction.ForkSessionRequest) (interaction.SessionOpened, error) {
	if !request.SourceSessionID.Valid() || !request.AgentID.Valid() || !request.WorkspaceID.Valid() || !request.Mode.Valid() {
		return interaction.SessionOpened{}, invalidInput("gateway.fork_session", "source SessionID, AgentID, WorkspaceID, and a valid fork mode are required")
	}
	if request.ModelConfig != nil {
		if err := request.ModelConfig.Validate(); err != nil {
			return interaction.SessionOpened{}, invalidInput("gateway.fork_session", err.Error())
		}
	}
	s, err := c.manager.Fork(ctx, session.ForkRequest{
		SourceSessionID: request.SourceSessionID,
		AgentID:         request.AgentID, WorkspaceID: request.WorkspaceID,
		Mode: request.Mode, ModelConfig: request.ModelConfig,
	})
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	candidate, err := newRuntimeInstance(s, c.components)
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	if err := c.registerNew(ctx, candidate); err != nil {
		return interaction.SessionOpened{}, err
	}
	return opened(candidate), nil
}

func (c *runtimeCoordinator) summary(ctx context.Context, request interaction.SummarySessionRequest) (interaction.SessionOpened, error) {
	if !request.AgentID.Valid() || !request.WorkspaceID.Valid() || len(request.Messages) == 0 {
		return interaction.SessionOpened{}, invalidInput("gateway.start_session_from_summary", "AgentID, WorkspaceID, and at least one summary message are required")
	}
	if request.ModelConfig != nil {
		if err := request.ModelConfig.Validate(); err != nil {
			return interaction.SessionOpened{}, invalidInput("gateway.start_session_from_summary", err.Error())
		}
	}
	s, err := c.manager.StartFromSummary(ctx, session.SummaryRequest{
		AgentID: request.AgentID, WorkspaceID: request.WorkspaceID,
		Messages: request.Messages, ModelConfig: request.ModelConfig,
	})
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	candidate, err := newRuntimeInstance(s, c.components)
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	if err := c.registerNew(ctx, candidate); err != nil {
		return interaction.SessionOpened{}, err
	}
	return opened(candidate), nil
}

func (c *runtimeCoordinator) runtime(id agent.SessionID) (runtimeAccess, error) {
	runtime, ok := c.registry.get(id)
	if !ok {
		return nil, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotOpen, "gateway.route", "session is not open", nil)
	}
	return runtime, nil
}

func (c *runtimeCoordinator) registerNew(ctx context.Context, runtime runtimeAccess) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, runtime.close(context.WithoutCancel(ctx)))
	}
	if err := c.registry.add(runtime); err != nil {
		return errors.Join(err, runtime.close(context.WithoutCancel(ctx)))
	}
	return nil
}

func (c *runtimeCoordinator) close(ctx context.Context, id agent.SessionID) error {
	if !id.Valid() {
		return invalidInput("gateway.close_session", "SessionID is required")
	}
	runtime, ok, err := c.registry.remove(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotOpen, "gateway.close", "session is not open", nil)
	}
	return runtime.close(ctx)
}

func opened(runtime runtimeAccess) interaction.SessionOpened {
	return interaction.SessionOpened{SessionID: runtime.id(), Revision: runtime.revision()}
}

// runtimeAccess is the private Gateway-to-Runtime contract. Its unexported
// methods prevent product modules and Entrypoints from implementing or
// obtaining this capability through the public Slot map.
type runtimeAccess interface {
	id() agent.SessionID
	revision() agent.Revision
	snapshot(context.Context, interaction.SnapshotRequest) (interaction.SessionSnapshot, error)
	send(context.Context, interaction.SendRequest) (interaction.EnqueueReceipt, error)
	steer(context.Context, interaction.SteerRequest) (interaction.EnqueueReceipt, error)
	pending(context.Context, interaction.RunPendingRequest) (interaction.RunReceipt, error)
	cancel(context.Context, interaction.CancelRequest) error
	idle(context.Context, interaction.WhenIdleRequest) error
	modelConfig(context.Context, interaction.ModelConfigRequest) (interaction.ModelConfigView, error)
	updateModelConfig(context.Context, interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error)
	editQueued(context.Context, interaction.EditQueuedRequest) (interaction.CommitReceipt, error)
	deleteQueued(context.Context, interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error)
	reclassifyQueued(context.Context, interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error)
	subscribe(context.Context, interaction.SubscribeRequest) (interaction.EventStream, error)
	close(context.Context) error
}

type runtimeInstance struct {
	session    session.Session
	components *runtimeComponents
}

func newRuntimeInstance(s session.Session, components *runtimeComponents) (*runtimeInstance, error) {
	if nilSession(s) {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.runtime", "SessionManager returned a nil Session", nil)
	}
	if components == nil {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.runtime", "Runtime components were not assembled", nil)
	}
	runtime := &runtimeInstance{session: s, components: components}
	if !runtime.id().Valid() {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.runtime", "SessionManager returned an invalid SessionID", nil)
	}
	return runtime, nil
}

func nilSession(value session.Session) bool {
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
func (r *runtimeInstance) id() agent.SessionID      { return r.session.ID() }
func (r *runtimeInstance) revision() agent.Revision { return r.session.Revision() }

func (r *runtimeInstance) snapshot(ctx context.Context, request interaction.SnapshotRequest) (interaction.SessionSnapshot, error) {
	snapshot, err := r.session.View(ctx)
	if err != nil {
		return interaction.SessionSnapshot{}, err
	}
	if request.KnownRevision > snapshot.Revision {
		return interaction.SessionSnapshot{}, agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "gateway.snapshot", "client revision is ahead of the persisted session", nil)
	}
	return interaction.SessionSnapshot{SessionID: r.id(), Revision: snapshot.Revision, Messages: append([]agent.Message(nil), snapshot.History...)}, nil
}

func (r *runtimeInstance) unavailable(op string) error {
	return runtimeUnavailable(op)
}

func (r *runtimeInstance) close(context.Context) error { return nil }

func (r *runtimeInstance) send(context.Context, interaction.SendRequest) (interaction.EnqueueReceipt, error) {
	return interaction.EnqueueReceipt{}, r.unavailable("gateway.send")
}
func (r *runtimeInstance) steer(context.Context, interaction.SteerRequest) (interaction.EnqueueReceipt, error) {
	return interaction.EnqueueReceipt{}, r.unavailable("gateway.steer")
}
func (r *runtimeInstance) pending(context.Context, interaction.RunPendingRequest) (interaction.RunReceipt, error) {
	return interaction.RunReceipt{}, r.unavailable("gateway.run_pending")
}
func (r *runtimeInstance) cancel(context.Context, interaction.CancelRequest) error {
	return r.unavailable("gateway.cancel")
}
func (r *runtimeInstance) idle(context.Context, interaction.WhenIdleRequest) error {
	return r.unavailable("gateway.when_idle")
}
func (r *runtimeInstance) modelConfig(context.Context, interaction.ModelConfigRequest) (interaction.ModelConfigView, error) {
	return interaction.ModelConfigView{}, r.unavailable("gateway.model_config")
}
func (r *runtimeInstance) updateModelConfig(context.Context, interaction.UpdateModelConfigRequest) (interaction.CommitReceipt, error) {
	return interaction.CommitReceipt{}, r.unavailable("gateway.update_model_config")
}
func (r *runtimeInstance) editQueued(context.Context, interaction.EditQueuedRequest) (interaction.CommitReceipt, error) {
	return interaction.CommitReceipt{}, r.unavailable("gateway.edit_queued")
}
func (r *runtimeInstance) deleteQueued(context.Context, interaction.DeleteQueuedRequest) (interaction.CommitReceipt, error) {
	return interaction.CommitReceipt{}, r.unavailable("gateway.delete_queued")
}
func (r *runtimeInstance) reclassifyQueued(context.Context, interaction.ReclassifyQueuedRequest) (interaction.CommitReceipt, error) {
	return interaction.CommitReceipt{}, r.unavailable("gateway.reclassify_queued")
}
func (r *runtimeInstance) subscribe(context.Context, interaction.SubscribeRequest) (interaction.EventStream, error) {
	return nil, r.unavailable("gateway.subscribe")
}

func notStartedError() error {
	return agent.NewCodedError(agent.ErrorUnavailable, agent.CodeApplicationNotStarted, "gateway", "application runtime is not started", nil)
}

func invalidInput(op, message string) error {
	return agent.NewError(agent.ErrorInvalidInput, op, message, nil)
}
