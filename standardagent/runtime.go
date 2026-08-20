package standardagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/model"
	"github.com/LyleLiu666/agentSlot/observe"
	"github.com/LyleLiu666/agentSlot/session"
)

type applicationRuntime struct {
	mu           sync.Mutex
	active       sync.WaitGroup
	dependencies runtimeDependencies
	registry     *runtimeRegistry
	coordinator  *runtimeCoordinator
	gateway      *gateway
	observations *observationHub
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
	r.observations = newObservationHub(r.dependencies.traces, r.dependencies.metrics, r.dependencies.audits, r.dependencies.usages)
	r.coordinator = &runtimeCoordinator{
		manager:    r.dependencies.manager,
		registry:   r.registry,
		components: r.dependencies.runtimeComponents(r.observations),
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
	observations := r.observations
	r.mu.Unlock()

	// No new operation can enter after started becomes false. Waiting here
	// lets already-routed operations finish before the registry is drained.
	r.active.Wait()

	r.mu.Lock()
	r.registry = nil
	r.coordinator = nil
	r.gateway = nil
	r.observations = nil
	r.mu.Unlock()
	var closeError error
	if registry != nil {
		closeError = registry.closeAll(ctx)
	}
	return errors.Join(closeError, observations.stop(ctx))
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
// has one Runtime even when multiple Gateway channels resume it concurrently.
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

func (r *runtimeRegistry) wait(ctx context.Context, id agent.SessionID) (runtimeAccess, bool, error) {
	for {
		r.mu.Lock()
		if runtime, ok := r.entries[id]; ok {
			r.mu.Unlock()
			return runtime, true, nil
		}
		flight, ok := r.inflight[id]
		if !ok {
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

// markClosing removes a Runtime from routable entries while retaining a
// single-flight tombstone until Close finishes. Concurrent Resume therefore
// cannot create a second Runtime while the old one can still commit.
func (r *runtimeRegistry) markClosing(id agent.SessionID, runtime runtimeAccess, done <-chan struct{}) error {
	r.mu.Lock()
	current, ok := r.entries[id]
	if !ok || current != runtime || r.inflight[id] != nil {
		r.mu.Unlock()
		return agent.NewCodedError(agent.ErrorConflict, agent.CodeSessionAlreadyOpen, "runtime_registry.close", "Session Runtime changed during close", nil)
	}
	delete(r.entries, id)
	flight := &runtimeFlight{done: make(chan struct{})}
	r.inflight[id] = flight
	r.mu.Unlock()
	go func() {
		<-done
		r.mu.Lock()
		if r.inflight[id] == flight {
			delete(r.inflight, id)
		}
		close(flight.done)
		r.mu.Unlock()
	}()
	return nil
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
	manager    *session.Manager
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
			return nil, agent.NewError(agent.ErrorInternal, "gateway.resume_session", "framework Manager returned a different SessionID", nil)
		}
		return runtime, nil
	})
	if err != nil {
		return interaction.SessionOpened{}, err
	}
	return opened(runtime), nil
}

func (c *runtimeCoordinator) fork(ctx context.Context, request interaction.ForkSessionRequest) (interaction.SessionOpened, error) {
	if !request.SourceSessionID.Valid() || !request.AgentID.Valid() || !request.WorkspaceID.Valid() {
		return interaction.SessionOpened{}, invalidInput("gateway.fork_session", "source SessionID, AgentID, and WorkspaceID are required")
	}
	if request.ModelConfig != nil {
		if err := request.ModelConfig.Validate(); err != nil {
			return interaction.SessionOpened{}, invalidInput("gateway.fork_session", err.Error())
		}
	}
	s, err := c.manager.Fork(ctx, session.ForkRequest{
		SourceSessionID: request.SourceSessionID,
		CutoffSequence:  request.CutoffSequence,
		AgentID:         request.AgentID, WorkspaceID: request.WorkspaceID,
		ModelConfig: request.ModelConfig,
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
		SourceSessionID: request.SourceSessionID,
		AgentID:         request.AgentID, WorkspaceID: request.WorkspaceID,
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
		return nil, agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotOpen, "gateway.runtime", "session is not open", nil)
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

func (c *runtimeCoordinator) close(ctx context.Context, request interaction.CloseSessionRequest) error {
	if !request.SessionID.Valid() {
		return invalidInput("gateway.close_session", "SessionID is required")
	}
	runtime, ok, err := c.registry.wait(ctx, request.SessionID)
	if err != nil {
		return err
	}
	if !ok {
		return agent.NewCodedError(agent.ErrorNotFound, agent.CodeSessionNotOpen, "gateway.close", "session is not open", nil)
	}
	done, err := runtime.beginClose(ctx, request)
	if err != nil {
		return err
	}
	if err := c.registry.markClosing(request.SessionID, runtime, done); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *runtimeCoordinator) models(ctx context.Context) ([]model.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var descriptors []model.Descriptor
	seen := make(map[string]bool)
	for _, named := range c.components.catalogs {
		models, err := named.Value.Models(ctx)
		if err != nil {
			return nil, err
		}
		for _, descriptor := range models {
			if err := descriptor.Validate(); err != nil {
				return nil, agent.NewError(agent.ErrorInternal, "gateway.available_models", "ModelCatalog returned an invalid descriptor", err)
			}
			if descriptor.ProviderKey != named.Key {
				return nil, agent.NewError(agent.ErrorInternal, "gateway.available_models", "ModelCatalog descriptor provider does not match its Slot key", nil)
			}
			identity := descriptor.ProviderKey + "\x00" + descriptor.ModelID
			if seen[identity] {
				return nil, agent.NewError(agent.ErrorInternal, "gateway.available_models", "ModelCatalog returned a duplicate model identity", nil)
			}
			seen[identity] = true
			descriptors = append(descriptors, cloneModelDescriptor(descriptor))
		}
	}
	sort.Slice(descriptors, func(i, j int) bool {
		if descriptors[i].ProviderKey != descriptors[j].ProviderKey {
			return descriptors[i].ProviderKey < descriptors[j].ProviderKey
		}
		return descriptors[i].ModelID < descriptors[j].ModelID
	})
	return descriptors, nil
}

func cloneModelDescriptor(source model.Descriptor) model.Descriptor {
	copy := source
	copy.Capabilities.Media.InputModalities = append([]model.Modality(nil), source.Capabilities.Media.InputModalities...)
	copy.Capabilities.Media.OutputModalities = append([]model.Modality(nil), source.Capabilities.Media.OutputModalities...)
	copy.Capabilities.Reasoning = append([]model.Reasoning(nil), source.Capabilities.Reasoning...)
	return copy
}

func opened(runtime runtimeAccess) interaction.SessionOpened {
	return interaction.SessionOpened{SessionID: runtime.id(), Revision: runtime.revision()}
}

// runtimeAccess is the private Gateway-to-Runtime contract. Its unexported
// methods prevent product modules and Gateway channels from implementing or
// obtaining this capability through the public Slot map.
type runtimeAccess interface {
	id() agent.SessionID
	revision() agent.Revision
	view(context.Context, interaction.SessionViewRequest) (interaction.SessionView, error)
	history(context.Context, interaction.HistoryRequest) (interaction.HistoryPage, error)
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
	beginClose(context.Context, interaction.CloseSessionRequest) (<-chan struct{}, error)
	close(context.Context) error
}

type runtimeInstance struct {
	session    session.Session
	components *runtimeComponents

	mu             sync.Mutex
	state          runtimeLifecycle
	active         *activeRun
	idleSignal     chan struct{}
	closing        bool
	closeDone      chan struct{}
	prefix         string
	sequence       atomic.Uint64
	revisionValue  atomic.Uint64
	commitObserver *sessionCommitObserver
	events         *eventHub
}

func newRuntimeInstance(s session.Session, components *runtimeComponents) (*runtimeInstance, error) {
	if nilSession(s) {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.runtime", "framework Manager returned a nil Session", nil)
	}
	if components == nil {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.runtime", "Runtime components were not assembled", nil)
	}
	prefixBytes := make([]byte, 8)
	if _, err := rand.Read(prefixBytes); err != nil {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.runtime", "cannot initialize Runtime ID allocator", err)
	}
	idleSignal := make(chan struct{})
	close(idleSignal)
	runtime := &runtimeInstance{
		session: s, components: components, state: runtimeIdle,
		idleSignal: idleSignal, closeDone: make(chan struct{}), prefix: hex.EncodeToString(prefixBytes), events: newEventHub(),
	}
	runtime.revisionValue.Store(uint64(s.Revision()))
	if !runtime.id().Valid() {
		return nil, agent.NewError(agent.ErrorInternal, "standardagent.runtime", "framework Manager returned an invalid SessionID", nil)
	}
	runtime.commitObserver = newSessionCommitObserver(components.commitObservers)
	components.observations.publishTrace(observe.TraceRecord{
		Kind: observe.TraceRuntimeOpened, At: time.Now().UTC(),
		Identity: observe.Identity{SessionID: runtime.id(), Actor: serviceObservationActor("agent-runtime")},
	})
	if err := runtime.restorePreparedRun(); err != nil {
		runtime.commitObserver.stop()
		runtime.events.close()
		return nil, err
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
func (r *runtimeInstance) id() agent.SessionID { return r.session.ID() }
func (r *runtimeInstance) revision() agent.Revision {
	return agent.Revision(r.revisionValue.Load())
}

func (r *runtimeInstance) beginClose(ctx context.Context, request interaction.CloseSessionRequest) (<-chan struct{}, error) {
	if request.SessionID != r.id() {
		return nil, invalidInput("gateway.close_session", "SessionID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.close_session"); err != nil {
		return nil, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return nil, err
	}
	if request.ExpectedRevision != snapshot.Revision {
		cause := agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "gateway.close_session", "expected revision does not match", nil)
		return nil, &interaction.RevisionConflictError{CurrentRevision: snapshot.Revision, SnapshotRequired: true, Cause: cause}
	}
	if r.active != nil && request.Actor.Valid() {
		r.active.controlActor = request.Actor
	}
	r.startCloseLocked()
	return r.closeDone, nil
}

func (r *runtimeInstance) view(ctx context.Context, request interaction.SessionViewRequest) (interaction.SessionView, error) {
	if request.SessionID != r.id() {
		return interaction.SessionView{}, invalidInput("gateway.view", "SessionID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.view"); err != nil {
		return interaction.SessionView{}, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return interaction.SessionView{}, err
	}
	page, err := r.components.store.HistoryPage(ctx, session.HistoryPageRequest{SessionID: r.id(), StepLimit: 100})
	if err != nil {
		return interaction.SessionView{}, err
	}
	return interaction.SessionView{
		SessionID: r.id(), Revision: snapshot.Revision,
		RecentHistory: page.Facts, HasMoreHistory: page.HasMore,
		Queue: snapshot.Queue, ModelConfig: cloneRuntimeConfig(snapshot.ModelConfig),
		RunState: snapshot.RunState, ActiveRunID: snapshot.ActiveRunID,
	}, nil
}

func (r *runtimeInstance) history(ctx context.Context, request interaction.HistoryRequest) (interaction.HistoryPage, error) {
	if request.SessionID != r.id() {
		return interaction.HistoryPage{}, invalidInput("gateway.history", "SessionID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.history"); err != nil {
		return interaction.HistoryPage{}, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return interaction.HistoryPage{}, err
	}
	page, err := r.components.store.HistoryPage(ctx, session.HistoryPageRequest{
		SessionID: request.SessionID, BeforeHistorySequence: request.BeforeHistorySequence, StepLimit: request.StepLimit,
	})
	if err != nil {
		return interaction.HistoryPage{}, err
	}
	return interaction.HistoryPage{SessionID: r.id(), Revision: snapshot.Revision, Facts: page.Facts, HasMore: page.HasMore}, nil
}

func (r *runtimeInstance) subscribe(ctx context.Context, request interaction.SubscribeRequest) (interaction.EventStream, error) {
	if request.SessionID != r.id() {
		return nil, invalidInput("gateway.subscribe", "SessionID is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureOpenLocked("gateway.subscribe"); err != nil {
		return nil, err
	}
	snapshot, err := r.viewLocked(ctx)
	if err != nil {
		return nil, err
	}
	if request.AfterRevision != snapshot.Revision {
		return nil, agent.NewCodedError(agent.ErrorConflict, agent.CodeRevisionConflict, "gateway.subscribe", "subscriber must first obtain the current SessionView", nil)
	}
	return r.events.subscribe()
}

func notStartedError() error {
	return agent.NewCodedError(agent.ErrorUnavailable, agent.CodeApplicationNotStarted, "gateway", "application runtime is not started", nil)
}

func invalidInput(op, message string) error {
	return agent.NewError(agent.ErrorInvalidInput, op, message, nil)
}
