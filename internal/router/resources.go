package router

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Resources is a single-writer coordinator. Slow process effects post
// generation-tagged results; a snapshot never authorizes start/stop by itself.
type Resources struct {
	cfg              config.Config
	factory          process.Factory
	logger, upstream *logmon.Monitor
	ledger           *resourceLedger
	backends         map[string]*resourceBackend
	queue            []string
	ops              chan func()
	done             chan struct{}
	now              func() time.Time
	closing, frozen  bool
	storageError     string
	runtimeCommand   resourceRuntimeCommand
}

type resourceBackend struct {
	id                                                                    string
	cfg                                                                   config.ModelConfig
	state, reason                                                         string
	generation                                                            uint64
	p                                                                     process.ResourceProcess
	cancel                                                                context.CancelFunc
	identity                                                              *process.ResourceIdentity
	sources                                                               map[string]time.Time
	waiters                                                               []*resourceWaiter
	queued                                                                bool
	inflight                                                              int
	startDone, stopDone, stopSent                                         bool
	startAt, drainAt, lastUse                                             time.Time
	recovered                                                             bool
	runtime                                                               *resourceRuntimeIdentity
	runtimePending, runtimeReleased, processExitedConfirmed, forcePending bool
	runtimeNextCheck                                                      time.Time
}

type resourceWaiter struct {
	ctx   context.Context
	reply chan resourceGrant
}
type resourceGrant struct {
	process    process.Process
	status     int
	message    string
	generation uint64
}

type ResourceDemand struct {
	Model   string `json:"model"`
	Source  string `json:"source"`
	HasWork bool   `json:"hasWork"`
}
type ResourceBackendStatus struct {
	Model         string                   `json:"model"`
	Generation    uint64                   `json:"generation"`
	State         string                   `json:"state"`
	Reason        string                   `json:"reason,omitempty"`
	QueuePosition int                      `json:"queuePosition,omitempty"`
	Resources     []string                 `json:"resources"`
	Sources       map[string]time.Time     `json:"sources"`
	Inflight      int                      `json:"inflight"`
	Runtime       *resourceRuntimeIdentity `json:"runtime,omitempty"`
}
type ResourcePoolStatus struct {
	Owner      string            `json:"owner,omitempty"`
	Generation uint64            `json:"generation,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
}
type ResourceStatus struct {
	Pools        map[string]ResourcePoolStatus    `json:"pools"`
	Backends     map[string]ResourceBackendStatus `json:"backends"`
	Queue        []string                         `json:"queue"`
	StorageError string                           `json:"storageError,omitempty"`
}

var _ LocalRouter = (*Resources)(nil)

func NewResources(cfg config.Config, logger, upstream *logmon.Monitor, opts ...Option) (*Resources, error) {
	resolved, err := resolveOptions(opts)
	if err != nil {
		return nil, err
	}
	if cfg.Routing.Router.Settings.Resources == nil {
		return nil, fmt.Errorf("resources configuration is missing")
	}
	l, err := openResourceLedger(cfg.Routing.Router.Settings.Resources.StateDir)
	if err != nil {
		return nil, err
	}
	r := &Resources{cfg: cfg, factory: resolved.processFactory, logger: logger, upstream: upstream, ledger: l, backends: map[string]*resourceBackend{}, ops: make(chan func()), done: make(chan struct{}), now: time.Now}
	r.runtimeCommand = runResourceRuntimeCommand
	for id, c := range cfg.Models {
		r.backends[id] = &resourceBackend{id: id, cfg: c, state: "stopped", sources: map[string]time.Time{}}
	}
	for id, owner := range l.Owners {
		b := r.backends[id]
		if b == nil {
			b = &resourceBackend{id: id, cfg: config.ModelConfig{Resources: owner.Resources}, sources: map[string]time.Time{}}
			r.backends[id] = b
		}
		b.state = "blocked"
		b.reason = "unconfirmed owner recovered from previous supervisor"
		b.generation = owner.Generation
		b.identity = owner.Identity
		b.recovered = true
		b.runtime = owner.Runtime
		// The old claim, rather than a changed configuration, defines which
		// devices remain unavailable after restart.
		b.cfg.Resources = append([]string(nil), owner.Resources...)
	}
	go r.run()
	return r, nil
}

func (r *Resources) submit(fn func()) bool {
	select {
	case r.ops <- fn:
		return true
	case <-r.done:
		return false
	}
}

func (r *Resources) run() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	defer close(r.done)
	for {
		select {
		case fn := <-r.ops:
			fn()
		case <-ticker.C:
			r.tick(r.now())
		}
		r.reconcile()
		if r.closing && len(r.ledger.Owners) == 0 {
			for _, b := range r.backends {
				if b.cancel != nil {
					b.cancel()
				}
			}
			if err := r.ledger.close(); err != nil {
				r.logger.Warnf("resources: closing ledger: %v", err)
			}
			return
		}
	}
}

func (r *Resources) persist() bool {
	if err := r.ledger.save(); err != nil {
		r.storageError = err.Error()
		r.logger.Warnf("resources: ledger write failed: %v", err)
		return false
	}
	r.storageError = ""
	return true
}

func (r *Resources) setState(b *resourceBackend, state, reason string) {
	if b.state == state && b.reason == reason {
		return
	}
	old := b.state
	b.state = state
	b.reason = reason
	r.logger.Infof("resources: model=%s generation=%d state=%s reason=%s", b.id, b.generation, state, reason)
	event.Emit(swaputil.ResourceStateChangeEvent{Model: b.id, Generation: b.generation, OldState: old, NewState: state, Reason: reason})
}

func (r *Resources) enqueue(b *resourceBackend) {
	if !b.queued {
		b.queued = true
		r.queue = append(r.queue, b.id)
	}
}

func (r *Resources) pruneWaiters(b *resourceBackend) {
	live := b.waiters[:0]
	for _, w := range b.waiters {
		if w.ctx.Err() == nil {
			live = append(live, w)
		}
	}
	b.waiters = live
}

func (r *Resources) demanded(b *resourceBackend) bool {
	return len(b.sources) > 0 || len(b.waiters) > 0
}

func (r *Resources) failWaiters(b *resourceBackend, message string) {
	for _, w := range b.waiters {
		select {
		case w.reply <- resourceGrant{status: http.StatusServiceUnavailable, message: message}:
		case <-w.ctx.Done():
		}
	}
	b.waiters = nil
}

func (r *Resources) grantWaiters(b *resourceBackend) {
	if b.state != "ready" {
		return
	}
	for _, w := range b.waiters {
		if w.ctx.Err() != nil {
			continue
		}
		grant := resourceGrant{process: b.p, generation: b.generation}
		if b.cfg.ConcurrencyLimit > 0 && b.inflight >= b.cfg.ConcurrencyLimit {
			grant = resourceGrant{status: http.StatusTooManyRequests, message: "backend concurrency limit reached"}
		}
		select {
		case w.reply <- grant:
			if grant.process != nil {
				b.inflight++
			}
		case <-w.ctx.Done():
		}
	}
	b.waiters = nil
}

func (r *Resources) beginDrain(b *resourceBackend, reason string) {
	if b.state != "ready" && b.state != "starting" {
		return
	}
	b.drainAt = r.now()
	r.setState(b, "draining", reason)
	r.failWaiters(b, "backend is draining")
}

func (r *Resources) stopIfDrained(b *resourceBackend) {
	if (b.state != "draining" && b.state != "blocked") || b.recovered || b.stopSent || b.inflight != 0 || b.p == nil || (!b.startDone && b.identity == nil) {
		return
	}
	b.stopSent = true
	p, id, gen, timeout := b.p, b.id, b.generation, b.cfg.Drain.Timeout
	go func() {
		err := p.Stop(timeout)
		r.submit(func() {
			current := r.backends[id]
			if current.generation != gen {
				return
			}
			current.stopDone = true
			if err != nil {
				r.setState(current, "blocked", err.Error())
			}
		})
	}()
}

func (r *Resources) conflicts(b *resourceBackend) []*resourceBackend {
	result := []*resourceBackend{}
	for _, other := range r.backends {
		if other == b || other.state == "stopped" {
			continue
		}
		for _, a := range b.cfg.Resources {
			for _, v := range other.cfg.Resources {
				if a == v {
					result = append(result, other)
					goto next
				}
			}
		}
	next:
	}
	return result
}

func (r *Resources) start(b *resourceBackend) {
	b.queued = false
	b.startDone = false
	b.stopDone = false
	b.stopSent = false
	b.recovered = false
	b.identity = nil
	b.runtime = nil
	b.runtimePending = false
	b.runtimeReleased = false
	b.processExitedConfirmed = false
	b.forcePending = false
	b.runtimeNextCheck = time.Time{}
	b.drainAt = time.Time{}
	b.startAt = r.now()
	r.ledger.NextGeneration++
	b.generation = r.ledger.NextGeneration
	if b.cfg.Runtime != nil {
		identity, err := newResourceRuntime(b.cfg.Runtime.Command, b.id, b.generation)
		if err != nil {
			r.failWaiters(b, err.Error())
			return
		}
		b.runtime = identity
	}
	r.ledger.Owners[b.id] = resourceOwner{Model: b.id, Generation: b.generation, Resources: append([]string(nil), b.cfg.Resources...), Runtime: b.runtime}
	if !r.persist() {
		r.setState(b, "blocked", "reservation could not be persisted")
		return
	}
	r.setState(b, "starting", "")
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	pcfg := b.cfg
	pcfg.ResourceEnvironment = []string{"LLAMA_SWAP_RESOURCE_MODEL=" + b.id, "LLAMA_SWAP_RESOURCE_GENERATION=" + fmt.Sprint(b.generation)}
	if b.runtime != nil {
		pcfg.ResourceEnvironment = append(pcfg.ResourceEnvironment, b.runtime.environment()...)
	}
	pcfg.UnloadAfter = 0 // TTL is an admission event, never a process-local bypass.
	p, err := r.factory.New(ctx, b.id, pcfg, logmon.NewWriter(r.upstream), r.logger)
	if err != nil {
		cancel()
		b.startDone = true
		b.runtime = nil // the factory failed before any runner could execute
		r.failWaiters(b, err.Error())
		r.release(b, err.Error())
		return
	}
	managed, ok := p.(process.ResourceProcess)
	if !ok {
		cancel()
		b.startDone = true
		b.runtime = nil
		r.failWaiters(b, "process factory does not support managed resources")
		r.release(b, "process factory does not support managed resources")
		return
	}
	b.p = managed
	id, gen := b.id, b.generation
	go func() {
		err := managed.EnsureReady(context.Background(), time.Duration(r.cfg.HealthCheckTimeout)*time.Second)
		r.submit(func() {
			current := r.backends[id]
			if current.generation != gen {
				return
			}
			current.startDone = true
			if err != nil {
				r.failWaiters(current, err.Error())
				r.setState(current, "draining", "startup failed: "+err.Error())
				current.drainAt = r.now()
				return
			}
			if current.state == "starting" {
				current.lastUse = r.now()
				r.setState(current, "ready", "")
				r.grantWaiters(current)
			}
		})
	}()
}

func (r *Resources) release(b *resourceBackend, reason string) {
	if b.runtime != nil && !b.runtimeReleased {
		if b.state != "blocked" {
			r.setState(b, "blocked", "waiting for confirmed Docker container termination")
		}
		return
	}
	owner, exists := r.ledger.Owners[b.id]
	if !exists {
		return
	}
	delete(r.ledger.Owners, b.id)
	if !r.persist() {
		r.ledger.Owners[b.id] = owner
		r.setState(b, "blocked", "release could not be persisted")
		return
	}
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.p = nil
	b.identity = nil
	b.recovered = false
	b.runtime = nil
	b.inflight = 0
	r.setState(b, "stopped", reason)
	// Demand heartbeats that predate this exit do not automatically restart
	// the victim. Fresh true reports (or new HTTP requests) enqueue it again.
}

func (r *Resources) tick(now time.Time) {
	for _, b := range r.backends {
		r.checkRuntime(b)
		for source, expires := range b.sources {
			if !now.Before(expires) {
				delete(b.sources, source)
			}
		}
		r.pruneWaiters(b)
		if b.state == "starting" && now.Sub(b.startAt) > time.Duration(r.cfg.HealthCheckTimeout)*time.Second+time.Second {
			r.beginDrain(b, "startup readiness timeout")
			r.setState(b, "blocked", "startup readiness timeout; waiting for cleanup")
		}
		if b.state == "ready" {
			if b.cfg.Activation == "demand" && len(b.sources) == 0 {
				r.beginDrain(b, "demand expired or withdrawn")
			}
			if b.cfg.UnloadAfter > 0 && b.inflight == 0 && now.Sub(b.lastUse) >= time.Duration(b.cfg.UnloadAfter)*time.Second {
				r.beginDrain(b, "idle TTL expired")
			}
		}
		if b.p != nil {
			if identity := b.p.ResourceIdentity(); identity != nil && b.identity == nil {
				b.identity = identity
				owner := r.ledger.Owners[b.id]
				owner.Identity = identity
				r.ledger.Owners[b.id] = owner
				if !r.persist() {
					r.beginDrain(b, "identity could not be persisted")
					r.setState(b, "blocked", "identity could not be persisted")
				}
			}
			if err := b.p.ResourceStopError(); err != "" {
				r.setState(b, "blocked", "stop command failed: "+err)
			}
			leaderExited := false
			if b.identity != nil {
				leaderExited, _ = process.ResourceLeaderExited(*b.identity)
			}
			if b.startDone && (b.p.State() == process.StateStopped || leaderExited) {
				if b.identity == nil {
					b.processExitedConfirmed = true
					r.release(b, b.reason)
					continue
				}
				exited, err := process.ResourceGroupExited(*b.identity)
				if err == nil && exited {
					b.processExitedConfirmed = true
					r.release(b, b.reason)
					continue
				}
				if err != nil {
					r.setState(b, "blocked", err.Error())
				} else {
					r.setState(b, "blocked", "descendants still own the process group")
				}
			}
		}
		if b.recovered && b.identity != nil {
			if exited, err := process.ResourceGroupExited(*b.identity); err == nil && exited {
				b.processExitedConfirmed = true
				r.release(b, "recovered owner exited")
				continue
			}
		}
		if b.state == "draining" && !b.drainAt.IsZero() && now.Sub(b.drainAt) >= b.cfg.Drain.Timeout {
			r.setState(b, "blocked", "graceful drain timeout exceeded")
		}
		r.stopIfDrained(b)
	}
}

func (r *Resources) reconcile() {
	if r.frozen {
		return
	}
	live := r.queue[:0]
	for _, id := range r.queue {
		b := r.backends[id]
		r.pruneWaiters(b)
		if b.queued && r.demanded(b) {
			live = append(live, id)
		} else {
			b.queued = false
		}
	}
	r.queue = live
	priorities := r.cfg.Routing.Scheduler.Settings.Fifo.Priority
	sort.SliceStable(r.queue, func(i, j int) bool { return priorities[r.queue[i]] > priorities[r.queue[j]] })
	ahead := map[string]bool{}
	for _, id := range r.queue {
		b := r.backends[id]
		if b.state == "ready" {
			b.queued = false
			r.grantWaiters(b)
			continue
		}
		if b.state != "stopped" {
			for _, name := range b.cfg.Resources {
				ahead[name] = true
			}
			continue
		}
		blocked := false
		for _, name := range b.cfg.Resources {
			if ahead[name] {
				blocked = true
			}
		}
		if blocked {
			continue
		}
		conflicts := r.conflicts(b)
		if len(conflicts) > 0 {
			for _, owner := range conflicts {
				r.beginDrain(owner, "queued competitor "+b.id)
				r.stopIfDrained(owner)
			}
			for _, name := range b.cfg.Resources {
				ahead[name] = true
			}
			continue
		}
		if !r.closing && r.storageError == "" {
			r.start(b)
		}
	}
	for _, b := range r.backends {
		r.stopIfDrained(b)
	}
}

func (r *Resources) statusBackend(b *resourceBackend) ResourceBackendStatus {
	sources := map[string]time.Time{}
	for id, t := range b.sources {
		sources[id] = t
	}
	status := ResourceBackendStatus{Model: b.id, Generation: b.generation, State: b.state, Reason: b.reason, Resources: append([]string{}, b.cfg.Resources...), Sources: sources, Inflight: b.inflight}
	if b.runtime != nil {
		runtime := *b.runtime
		status.Runtime = &runtime
	}
	position := 0
	for _, id := range r.queue {
		if r.backends[id].queued {
			position++
			if id == b.id {
				status.QueuePosition = position
				break
			}
		}
	}
	if status.State == "stopped" && b.queued {
		status.State = "queued"
	}
	return status
}

func (r *Resources) Demand(d ResourceDemand) (ResourceBackendStatus, int, error) {
	type result struct {
		s    ResourceBackendStatus
		code int
		err  error
	}
	reply := make(chan result, 1)
	if !r.submit(func() {
		b := r.backends[d.Model]
		if _, ok := r.cfg.Models[d.Model]; !ok {
			reply <- result{code: 404, err: fmt.Errorf("unknown backend")}
			return
		}
		if strings.TrimSpace(d.Source) == "" || len(d.Source) > 256 || (b.cfg.Activation != "demand" && b.cfg.Activation != "both") {
			reply <- result{code: 400, err: fmt.Errorf("nonempty source and demand-enabled backend are required")}
			return
		}
		if r.closing || r.frozen {
			reply <- result{code: 503, err: fmt.Errorf("resource coordinator is unavailable")}
			return
		}
		if d.HasWork {
			b.sources[d.Source] = r.now().Add(r.cfg.Routing.Router.Settings.Resources.DemandTimeout)
			if b.state == "stopped" {
				r.enqueue(b)
			}
		} else {
			delete(b.sources, d.Source)
		}
		if b.state == "ready" && b.cfg.Activation == "demand" && len(b.sources) == 0 {
			r.beginDrain(b, "demand withdrawn")
		}
		r.reconcile()
		reply <- result{s: r.statusBackend(b), code: 200}
	}) {
		return ResourceBackendStatus{}, 503, fmt.Errorf("resource coordinator stopped")
	}
	got := <-reply
	return got.s, got.code, got.err
}

func (r *Resources) Snapshot() ResourceStatus {
	reply := make(chan ResourceStatus, 1)
	if !r.submit(func() {
		s := ResourceStatus{Pools: map[string]ResourcePoolStatus{}, Backends: map[string]ResourceBackendStatus{}, Queue: []string{}, StorageError: r.storageError}
		for name, pool := range r.cfg.Routing.Router.Settings.Resources.Pools {
			s.Pools[name] = ResourcePoolStatus{Parameters: pool.Parameters}
		}
		for id, b := range r.backends {
			s.Backends[id] = r.statusBackend(b)
			if b.state != "stopped" {
				for _, name := range b.cfg.Resources {
					pool := s.Pools[name]
					pool.Owner = id
					pool.Generation = b.generation
					s.Pools[name] = pool
				}
			}
		}
		for _, id := range r.queue {
			if r.backends[id].queued {
				s.Queue = append(s.Queue, id)
			}
		}
		reply <- s
	}) {
		return ResourceStatus{}
	}
	return <-reply
}

func (r *Resources) ForceStop(id string, generation uint64) error {
	reply := make(chan error, 1)
	if !r.submit(func() {
		b := r.backends[id]
		if b == nil {
			reply <- fmt.Errorf("unknown backend")
			return
		}
		if generation == 0 || b.generation != generation || b.state == "stopped" {
			reply <- fmt.Errorf("backend generation does not match current owner")
			return
		}
		if b.identity == nil && b.p != nil {
			if identity := b.p.ResourceIdentity(); identity != nil {
				b.identity = identity
				owner := r.ledger.Owners[b.id]
				owner.Identity = identity
				r.ledger.Owners[b.id] = owner
				if !r.persist() {
					reply <- fmt.Errorf("cannot persist identity before force-stop")
					return
				}
			}
		}
		if b.identity == nil {
			if b.runtime != nil {
				r.forceRuntime(b, reply)
				return
			}
			reply <- fmt.Errorf("owner identity is unconfirmed; operator verification is required")
			return
		}
		r.beginDrain(b, "explicit force-stop")
		if b.runtime != nil {
			r.forceRuntime(b, reply)
			return
		}
		identity := *b.identity
		// The actor serializes generation validation with the signal. Killing
		// is a short syscall; waiting and resource release remain asynchronous.
		if b.p != nil {
			reply <- b.p.ForceResourceStop()
		} else {
			reply <- process.KillResourceGroup(identity)
		}
	}) {
		return fmt.Errorf("resource coordinator stopped")
	}
	return <-reply
}

func (r *Resources) Handles(id string) bool { _, ok := r.cfg.Models[id]; return ok }
func (r *Resources) ProcessLogger(id string) (*logmon.Monitor, bool) {
	type result struct {
		log *logmon.Monitor
		ok  bool
	}
	reply := make(chan result, 1)
	if !r.submit(func() {
		b := r.backends[id]
		if b != nil && b.p != nil {
			reply <- result{b.p.Logger(), true}
		} else {
			reply <- result{}
		}
	}) {
		return nil, false
	}
	v := <-reply
	return v.log, v.ok
}
func (r *Resources) RunningModels() map[string]process.ProcessState {
	result := map[string]process.ProcessState{}
	for id, st := range r.RunningStatus() {
		result[id] = st.State
	}
	return result
}

// RunningStatus snapshots coordinator ownership and upstream process details
// together. Recovered owners can be blocked even without a process instance.
func (r *Resources) RunningStatus() map[string]process.Status {
	reply := make(chan map[string]process.Status, 1)
	if !r.submit(func() {
		result := map[string]process.Status{}
		for id, b := range r.backends {
			var state process.ProcessState
			switch b.state {
			case "starting":
				state = process.StateStarting
			case "ready":
				state = process.StateReady
			case "draining", "blocked":
				state = process.StateStopping
			default:
				continue
			}
			st := process.Status{}
			if b.p != nil {
				st = b.p.Status()
			}
			st.State = state
			if state != process.StateReady {
				st.ReadySince = time.Time{}
			}
			if state != process.StateStarting {
				st.Loading = nil
			}
			result[id] = st
		}
		reply <- result
	}) {
		return map[string]process.Status{}
	}
	return <-reply
}

func (r *Resources) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	data, err := swaputil.FetchContext(req, r.cfg)
	if err != nil {
		swaputil.SendError(w, req, err)
		return
	}
	waiter := &resourceWaiter{ctx: req.Context(), reply: make(chan resourceGrant)}
	if !r.submit(func() {
		b := r.backends[data.ModelID]
		if b == nil || !r.Handles(data.ModelID) {
			select {
			case waiter.reply <- resourceGrant{status: 404, message: "unknown backend"}:
			case <-req.Context().Done():
			}
			return
		}
		if r.closing || r.frozen || (b.cfg.Activation == "demand" && b.state != "ready") {
			select {
			case waiter.reply <- resourceGrant{status: 503, message: "backend unavailable; report demand to activate it"}:
			case <-req.Context().Done():
			}
			return
		}
		b.waiters = append(b.waiters, waiter)
		if b.state == "ready" {
			r.grantWaiters(b)
		} else {
			r.enqueue(b)
		}
	}) {
		http.Error(w, "resource coordinator stopped", http.StatusServiceUnavailable)
		return
	}
	var grant resourceGrant
	select {
	case grant = <-waiter.reply:
	case <-req.Context().Done():
		r.submit(func() { r.pruneWaiters(r.backends[data.ModelID]) })
		return
	}
	if grant.process == nil {
		http.Error(w, grant.message, grant.status)
		return
	}
	defer r.submit(func() {
		b := r.backends[data.ModelID]
		if b.generation == grant.generation {
			b.inflight--
			b.lastUse = r.now()
		}
	})
	grant.process.ServeHTTP(w, req)
}

func (r *Resources) Unload(_ time.Duration, ids ...string) {
	r.submit(func() {
		targets := ids
		if len(targets) == 0 {
			for id := range r.backends {
				targets = append(targets, id)
			}
		}
		for _, id := range targets {
			if b := r.backends[id]; b != nil {
				b.sources = map[string]time.Time{}
				b.queued = false
				r.failWaiters(b, "backend unloaded")
				r.beginDrain(b, "manual unload")
			}
		}
	})
}

func (r *Resources) Shutdown(_ time.Duration) error {
	if !r.submit(func() {
		r.closing = true
		for _, b := range r.backends {
			b.queued = false
			b.sources = map[string]time.Time{}
			r.failWaiters(b, "supervisor shutting down")
			r.beginDrain(b, "supervisor shutdown")
		}
	}) {
		return nil
	}
	<-r.done
	return nil
}

// PauseReload fences admissions before unlocking an empty ledger. A failed
// replacement can relock and resume this instance without losing live owners.
func (r *Resources) PauseReload() error {
	reply := make(chan error, 1)
	if !r.submit(func() {
		if r.closing || r.frozen || len(r.ledger.Owners) > 0 || len(r.queue) > 0 {
			reply <- fmt.Errorf("resource router has reservations, owners, or queued work; reload rejected")
			return
		}
		r.frozen = true
		err := r.ledger.unlock()
		if err != nil {
			r.frozen = false
		}
		reply <- err
	}) {
		return fmt.Errorf("resource coordinator stopped")
	}
	return <-reply
}
func (r *Resources) ResumeReload() error {
	reply := make(chan error, 1)
	if !r.submit(func() {
		err := r.ledger.relock()
		if err == nil {
			r.frozen = false
		} else {
			r.storageError = err.Error()
		}
		reply <- err
	}) {
		return fmt.Errorf("resource coordinator stopped")
	}
	return <-reply
}
