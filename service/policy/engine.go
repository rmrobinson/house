package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	lua "github.com/yuin/gopher-lua"
	"go.uber.org/zap"
)

// defaultExecutionTimeout is the maximum runtime for a single script
// execution when the engine isn't configured with WithExecutionTimeout.
const defaultExecutionTimeout = 30 * time.Second

// ErrPolicyNotRegistered is returned by Unregister when id has no
// registered policy - distinct from a persistence failure partway through
// unregistering one that was, so a caller (e.g. the gRPC Service) can map
// the two to different status codes.
var ErrPolicyNotRegistered = errors.New("policy not registered")

// UI-facing Bus topics, published whenever something a UI would want to
// live-update on. uiPolicyChangedTopic's Payload is the changed policy's ID
// (a string) — enough for a subscriber to re-render and OOB-patch just that
// one policy's own UI elements rather than a whole page. uiLogAppendedTopic's
// Payload is the appended ExecutionLog itself, which already carries its
// PolicyID for the same reason.
const (
	uiPolicyChangedTopic = "ui.policy-changed"
	uiLogAppendedTopic   = "ui.log-appended"
)

// EngineOption configures optional Engine behaviour at construction time.
type EngineOption func(*Engine)

// WithExecutionTimeout sets the maximum runtime for a single script
// execution, independent of its policy's condition state: a script that
// runs past this is cancelled via lua.SetContext even if its condition
// never goes false. The default is defaultExecutionTimeout.
func WithExecutionTimeout(d time.Duration) EngineOption {
	return func(e *Engine) { e.executionTimeout = d }
}

// WithStore configures the Engine to persist policies and execution logs to
// store. Without this option, both live in memory only and are lost on
// restart.
func WithStore(store Store) EngineOption {
	return func(e *Engine) { e.store = store }
}

// WithNotifyAPI configures the Engine's "notify" Lua global (see
// registerNotifyTable) to send through n. Without this option, e.notify
// stays nil and notify.send raises an ErrNotImplemented-flavoured binding
// error - the same "unconfigured" story HomeAPI's own unbacked methods use.
func WithNotifyAPI(n NotifyAPI) EngineOption {
	return func(e *Engine) { e.notify = n }
}

type cancelEntry struct {
	id     uint64
	cancel context.CancelFunc
}

type registeredPolicy struct {
	// policy is a copy of the *Policy the caller passed to Register, not
	// the caller's own pointer: the caller mutating their Policy after
	// Register must not change what's already registered (see
	// service/bridge's proto.Clone-on-write/read convention for the same
	// hazard with a different object shape).
	policy             *Policy
	condition          Condition
	conditionTypeNames map[string]struct{}
	cancel             context.CancelFunc

	mu      sync.Mutex
	running []cancelEntry
	active  bool
}

// Engine manages policy registration, condition evaluation, and Lua script
// execution, backed by a HomeAPI and a state cache populated by whatever
// hydrates it from the home's device/house streams (see UpdateDeviceState).
type Engine struct {
	home     HomeAPI
	notify   NotifyAPI
	registry *ConditionRegistry
	logger   *zap.Logger
	bus      *Bus
	cache    *stateCache
	store    Store

	executionTimeout time.Duration

	rootCtx    context.Context
	rootCancel context.CancelFunc

	nextExecID atomic.Uint64

	mu       sync.Mutex
	policies map[string]*registeredPolicy

	logsMu sync.Mutex
	logs   []ExecutionLog
}

// NewEngine creates an Engine backed by api for device/house interaction and
// registry for resolving policies' condition expressions into live
// Condition trees.
func NewEngine(api HomeAPI, registry *ConditionRegistry, logger *zap.Logger, opts ...EngineOption) *Engine {
	ctx, cancel := context.WithCancel(context.Background())

	e := &Engine{
		home:             api,
		registry:         registry,
		logger:           logger,
		bus:              NewBus(),
		cache:            newStateCache(),
		executionTimeout: defaultExecutionTimeout,
		rootCtx:          ctx,
		rootCancel:       cancel,
		policies:         make(map[string]*registeredPolicy),
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// Close cancels condition evaluation for every registered policy — no
// further script executions will be triggered — and, for policies
// configured with Interrupt, cancels their running script instances too.
// Instances belonging to a Complete policy are left running: Close does
// not wait for them to finish, but it also doesn't cut them off; they run
// until their own execution timeout or natural completion. Drain via Logs
// if you need to observe that.
func (e *Engine) Close() {
	e.rootCancel()

	e.mu.Lock()
	policies := make([]*registeredPolicy, 0, len(e.policies))
	for _, rp := range e.policies {
		policies = append(policies, rp)
	}
	e.mu.Unlock()

	for _, rp := range policies {
		if rp.policy.OnConditionFalse != Interrupt {
			continue
		}
		rp.mu.Lock()
		for _, ce := range rp.running {
			ce.cancel()
		}
		rp.mu.Unlock()
	}
}

// Bus returns the engine's internal event bus. Whatever wires real event
// sources in (motion, power-restore, house-mode changes, ...) publishes on
// it; EventCondition-based condition types subscribe to it.
func (e *Engine) Bus() *Bus {
	return e.bus
}

// UpdateDeviceState records value as entityID's last known state, tagged
// with kind, and publishes it on the engine's bus as
// "device.updated.<entityID>". Whatever hydrates the engine from the home's
// device/house gRPC streams should call this on every update; the engine
// does not subscribe to those streams itself.
//
// kind is opaque to the engine - it imposes no schema of its own - and is
// whatever the caller finds useful to enumerate devices by later via
// DevicesOfKind (e.g. bridgehome derives it from device.Device's populated
// "details" oneof branch: "light", "sensor", "ups", ...). Pass "" if the
// caller has no meaningful kind to offer.
func (e *Engine) UpdateDeviceState(entityID, kind string, value any) {
	e.cache.set(entityID, kind, value)
	e.bus.Publish(Event{Topic: "device.updated." + entityID, Payload: value})
	if kind != "" {
		e.bus.Publish(Event{Topic: deviceKindUpdatedTopicPrefix + kind, Payload: entityID})
	}
}

// GetLastKnown returns entityID's last known state from the cache, and
// whether an entry was present.
func (e *Engine) GetLastKnown(entityID string) (any, bool) {
	return e.cache.get(entityID)
}

// DevicesOfKind returns the sorted IDs of every cached device tagged with
// kind (see UpdateDeviceState). It reads the engine's local cache only - no
// HomeAPI call, no round-trip to whatever's behind it - so it reflects
// exactly the devices observed via the update stream so far, not
// necessarily every device that exists. Exposed to policy scripts as
// home.findDevices(kind) (see registerHomeTable).
func (e *Engine) DevicesOfKind(kind string) []string {
	return e.cache.byKind(kind)
}

// RemoveDeviceState drops entityID from the cache. Whatever hydrates the
// engine from the home's device/house gRPC streams should call this when a
// device is reported removed; no bus topic is published for it, since
// nothing subscribes to removal today.
func (e *Engine) RemoveDeviceState(entityID string) {
	e.cache.delete(entityID)
}

// Register validates p, compiles its script, builds its condition tree, and
// starts evaluating it. If a policy with the same ID is already registered,
// it is replaced: the old policy's condition evaluation is cancelled, and
// its running script instances are cancelled too if the old policy was
// configured with Interrupt (left to run to completion if Complete).
//
// Register stores a copy of p's fields, not p itself: mutating the *Policy
// you passed in after Register returns has no effect on what's registered.
func (e *Engine) Register(p *Policy) error {
	return e.register(p, true)
}

// register is Register's implementation, with persist controlling whether
// it also writes p to e.store. LoadPersistedPolicies passes false: p was
// just read from that same store, so writing it straight back would be a
// redundant no-op write on every single policy at every startup.
func (e *Engine) register(p *Policy, persist bool) error {
	if p == nil {
		return fmt.Errorf("policy: nil policy")
	}
	if p.ID == "" {
		return fmt.Errorf("policy: ID is required")
	}
	if p.ConditionExpr == nil {
		return fmt.Errorf("policy %q: ConditionExpr is required", p.ID)
	}
	if p.Script == "" {
		return fmt.Errorf("policy %q: Script is required", p.ID)
	}

	if err := validateScript(p.Script); err != nil {
		return fmt.Errorf("policy %q: invalid script: %w", p.ID, err)
	}

	cond, err := p.ConditionExpr.build(e.registry)
	if err != nil {
		return fmt.Errorf("policy %q: %w", p.ID, err)
	}

	if persist && e.store != nil {
		if err := e.store.SavePolicy(p); err != nil {
			return fmt.Errorf("policy %q: persisting: %w", p.ID, err)
		}
	}

	names := map[string]struct{}{}
	p.ConditionExpr.conditionTypeNames(names)

	policyCopy := *p
	condCtx, condCancel := context.WithCancel(e.rootCtx)
	rp := &registeredPolicy{
		policy:             &policyCopy,
		condition:          cond,
		conditionTypeNames: names,
		cancel:             condCancel,
	}

	e.mu.Lock()
	old := e.policies[p.ID]
	e.policies[p.ID] = rp
	e.mu.Unlock()

	if old != nil {
		old.cancel()
		if old.policy.OnConditionFalse == Interrupt {
			old.mu.Lock()
			for _, ce := range old.running {
				ce.cancel()
			}
			old.mu.Unlock()
		}
	}

	cond.Start(condCtx, func(value bool) {
		rp.mu.Lock()
		rp.active = value
		rp.mu.Unlock()
		// rp.policy.ID, not p.ID: this closure can fire arbitrarily long
		// after Register returns, and p is the caller's own pointer — safe
		// for them to mutate or reuse once Register returns (see Register's
		// doc comment). Reading p.ID here would follow that mutation, both
		// publishing the wrong ID and, worse, making the lookup below miss
		// and silently skip triggering the script.
		e.bus.Publish(Event{Topic: uiPolicyChangedTopic, Payload: rp.policy.ID})

		if !value {
			return
		}
		// Guard against a condition goroutine that was already about to
		// fire onChange concurrently with this policy being replaced or
		// unregistered (context cancellation only stops it before its
		// next tick, not mid-flight): e.policies[rp.policy.ID] is
		// swapped/deleted under e.mu strictly before any teardown below
		// runs, so a late fire from a torn-down rp will always see the
		// mismatch here and skip triggering, instead of orphaning an
		// uninterruptible run.
		e.mu.Lock()
		current := e.policies[rp.policy.ID]
		e.mu.Unlock()
		if current != rp {
			return
		}
		e.trigger(rp)
	})

	e.bus.Publish(Event{Topic: uiPolicyChangedTopic, Payload: rp.policy.ID})

	return nil
}

// PolicyInfo is a read-only snapshot of a registered policy, for
// introspection (e.g. by an HTTP UI) rather than execution.
type PolicyInfo struct {
	ID               string
	ConditionExpr    ConditionExpr
	Script           string
	OnConditionFalse OnConditionFalse
	// Active is the condition's last-known evaluated value.
	Active bool
}

func policyInfoFor(rp *registeredPolicy) PolicyInfo {
	rp.mu.Lock()
	active := rp.active
	rp.mu.Unlock()

	return PolicyInfo{
		ID:               rp.policy.ID,
		ConditionExpr:    rp.policy.ConditionExpr,
		Script:           rp.policy.Script,
		OnConditionFalse: rp.policy.OnConditionFalse,
		Active:           active,
	}
}

// Policies returns a snapshot of every registered policy, sorted by ID.
func (e *Engine) Policies() []PolicyInfo {
	e.mu.Lock()
	rps := make([]*registeredPolicy, 0, len(e.policies))
	for _, rp := range e.policies {
		rps = append(rps, rp)
	}
	e.mu.Unlock()

	out := make([]PolicyInfo, 0, len(rps))
	for _, rp := range rps {
		out = append(out, policyInfoFor(rp))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Policy returns a snapshot of id's registered policy, and whether id is
// currently registered.
func (e *Engine) Policy(id string) (PolicyInfo, bool) {
	e.mu.Lock()
	rp, ok := e.policies[id]
	e.mu.Unlock()
	if !ok {
		return PolicyInfo{}, false
	}
	return policyInfoFor(rp), true
}

// Simulate reports id's condition tree evaluated synchronously against
// live state right now — the same combined boolean that decides whether
// id's script would fire — and whether id is currently registered. It has
// no side effects.
func (e *Engine) Simulate(id string) (result bool, registered bool) {
	e.mu.Lock()
	rp, ok := e.policies[id]
	e.mu.Unlock()
	if !ok {
		return false, false
	}
	return rp.condition.Evaluate(), true
}

// isRegistered reports whether id currently has a registered policy.
func (e *Engine) isRegistered(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.policies[id]
	return ok
}

// Unregister cancels id's condition evaluation. If it was configured with
// Interrupt, all of its running script instances are also cancelled.
func (e *Engine) Unregister(id string) error {
	e.mu.Lock()
	rp, ok := e.policies[id]
	if ok {
		delete(e.policies, id)
	}
	e.mu.Unlock()

	if !ok {
		return fmt.Errorf("policy %q: %w", id, ErrPolicyNotRegistered)
	}

	// Cancel id's condition evaluation (and, for Interrupt, its running
	// script instances) unconditionally, before touching the store: rp is
	// already gone from e.policies above, so a store error below must not
	// leave its goroutine and bus subscription running with no way to
	// retry the unregister (the map entry can't be re-added to retry
	// against).
	rp.cancel()
	if rp.policy.OnConditionFalse == Interrupt {
		rp.mu.Lock()
		for _, ce := range rp.running {
			ce.cancel()
		}
		rp.mu.Unlock()
	}

	if e.store != nil {
		if err := e.store.DeletePolicy(id); err != nil {
			return fmt.Errorf("policy %q: unpersisting: %w", id, err)
		}
	}

	e.bus.Publish(Event{Topic: uiPolicyChangedTopic, Payload: id})

	return nil
}

// UnregisterConditionType removes name from the Engine's ConditionRegistry.
// It's allowed even while a registered policy's condition tree was built
// from it — that policy keeps running unaffected, since its Condition tree
// doesn't consult the registry again after Register built it — but it's
// logged as a warning, since that policy will fail to rebuild (on a
// Register replace, or if the process restarts and re-registers it) once
// name is gone.
func (e *Engine) UnregisterConditionType(name string) {
	e.registry.UnregisterConditionType(name)

	e.mu.Lock()
	var affected []string
	for id, rp := range e.policies {
		if _, ok := rp.conditionTypeNames[name]; ok {
			affected = append(affected, id)
		}
	}
	e.mu.Unlock()

	if len(affected) > 0 {
		e.logger.Warn("condition type unregistered while still referenced by active policies",
			zap.String("condition_type", name),
			zap.Strings("policy_ids", affected),
		)
	}
}

// Logs returns every ExecutionLog recorded so far, oldest first.
func (e *Engine) Logs() []ExecutionLog {
	e.logsMu.Lock()
	defer e.logsMu.Unlock()

	out := make([]ExecutionLog, len(e.logs))
	copy(out, e.logs)
	return out
}

// LogsForPolicy returns every ExecutionLog recorded for policyID, oldest
// first.
func (e *Engine) LogsForPolicy(policyID string) []ExecutionLog {
	e.logsMu.Lock()
	defer e.logsMu.Unlock()

	var out []ExecutionLog
	for _, l := range e.logs {
		if l.PolicyID == policyID {
			out = append(out, l)
		}
	}
	return out
}

// LastLog returns policyID's most recent ExecutionLog, or ok=false if it has
// none. Like RecentLogs, it prefers Store.GetLogs when a store is
// configured, so a policy's last run survives an engine restart instead of
// reading as "never run" until it next fires and repopulates e.logs - a
// store query error falls back to the in-memory tail rather than
// propagating, since LastLog has no error return of its own.
func (e *Engine) LastLog(policyID string) (l ExecutionLog, ok bool) {
	if e.store != nil {
		logs, err := e.store.GetLogs(policyID, 1)
		if err != nil {
			e.logger.Warn("unable to query last log from store; falling back to in-memory tail",
				zap.String("policy_id", policyID), zap.Error(err))
		} else if len(logs) > 0 {
			return logs[0], true
		} else {
			return ExecutionLog{}, false
		}
	}

	e.logsMu.Lock()
	defer e.logsMu.Unlock()

	for _, entry := range e.logs {
		if entry.PolicyID == policyID {
			l, ok = entry, true
		}
	}
	return l, ok
}

// RecentLogs returns up to limit ExecutionLogs matching policyID (every
// policy's, if empty), newest first; limit <= 0 means unlimited. It prefers
// Store.GetLogs when a store is configured - a bounded, indexed query -
// rather than always scanning e.logs (itself bounded to maxInMemoryLogs, see
// appendLog) and reversing/truncating it in memory, which is only done as a
// fallback for a store-less Engine.
func (e *Engine) RecentLogs(policyID string, limit int) ([]ExecutionLog, error) {
	if e.store != nil {
		return e.store.GetLogs(policyID, limit)
	}

	var all []ExecutionLog
	if policyID != "" {
		all = e.LogsForPolicy(policyID)
	} else {
		all = e.Logs()
	}

	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	out := make([]ExecutionLog, limit)
	for i := 0; i < limit; i++ {
		out[i] = all[len(all)-1-i]
	}
	return out, nil
}

// LastLogs returns the most recent ExecutionLog for every policy that has
// at least one, keyed by policy ID - for a caller (e.g. the policies list
// page) that wants every policy's last log at once, which would otherwise
// cost one LogsForPolicy scan of the entire history per policy rendered.
// Like LastLog, it prefers Store.LastLogs when a store is configured, so
// the policies list still shows last-run status after an engine restart;
// a store query error falls back to the in-memory tail.
func (e *Engine) LastLogs() map[string]ExecutionLog {
	if e.store != nil {
		out, err := e.store.LastLogs()
		if err != nil {
			e.logger.Warn("unable to query last logs from store; falling back to in-memory tail", zap.Error(err))
		} else {
			return out
		}
	}

	e.logsMu.Lock()
	defer e.logsMu.Unlock()

	out := make(map[string]ExecutionLog)
	for _, l := range e.logs {
		out[l.PolicyID] = l
	}
	return out
}

// maxInMemoryLogs bounds e.logs: without a cap, a long-running engine with
// frequently-firing policies accumulates one ExecutionLog per trigger
// forever, growing memory (and the cost of every Logs/LogsForPolicy scan)
// without bound. Full history remains available through Store, when one is
// configured (see WithStore); e.logs is only the in-memory tail used to
// serve Logs/LogsForPolicy without a store round-trip, and as LastLog/
// LastLogs' fallback for a store-less Engine or a failed store query -
// those two otherwise prefer Store directly, so a policy's last run
// survives an engine restart.
const maxInMemoryLogs = 1000

func (e *Engine) appendLog(l ExecutionLog) {
	e.logsMu.Lock()
	e.logs = append(e.logs, l)
	if len(e.logs) > maxInMemoryLogs {
		e.logs = e.logs[len(e.logs)-maxInMemoryLogs:]
	}
	e.logsMu.Unlock()

	if e.store != nil {
		if err := e.store.AppendLog(l); err != nil {
			e.logger.Warn("unable to persist execution log",
				zap.String("policy_id", l.PolicyID),
				zap.Error(err),
			)
		}
	}

	e.bus.Publish(Event{Topic: uiLogAppendedTopic, Payload: l})
}

// trigger spawns a new, independent script execution for rp. Execution is
// re-entrant: a trigger while a previous instance is still running starts a
// second, unrelated instance rather than replacing it.
func (e *Engine) trigger(rp *registeredPolicy) {
	execID := e.nextExecID.Add(1)

	// Read synchronously, on the condition's own onChange goroutine, so a
	// second transition can't overwrite the context before this run uses it.
	var triggerDevices []string
	if tp, ok := rp.condition.(TriggerContextProvider); ok {
		triggerDevices = tp.TriggerDeviceIDs()
	}

	// Deliberately not derived from e.rootCtx: Close() must be able to
	// force-cancel Interrupt-mode runs without also force-cancelling
	// Complete-mode ones just because the whole engine is shutting down.
	execCtx, cancel := context.WithTimeout(context.Background(), e.executionTimeout)

	rp.mu.Lock()
	rp.running = append(rp.running, cancelEntry{id: execID, cancel: cancel})
	rp.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			rp.mu.Lock()
			for i, ce := range rp.running {
				if ce.id == execID {
					rp.running = append(rp.running[:i], rp.running[i+1:]...)
					break
				}
			}
			rp.mu.Unlock()
		}()

		e.runScript(execCtx, rp.policy, triggerDevices)
	}()
}

func (e *Engine) runScript(ctx context.Context, p *Policy, triggerDevices []string) {
	log := ExecutionLog{
		PolicyID:  p.ID,
		StartedAt: time.Now(),
		Status:    StatusRunning,
	}

	L := lua.NewState()
	defer L.Close()
	L.SetContext(ctx)

	registerHomeTable(L, e.home, e.DevicesOfKind)
	registerNotifyTable(L, e.notify)
	registerTriggerTable(L, triggerDevices)

	err := L.DoString(p.Script)
	log.EndedAt = time.Now()

	switch {
	case err == nil:
		log.Status = StatusSuccess
	case ctx.Err() != nil:
		log.Status = StatusError
		log.Error = ctx.Err().Error()
	default:
		if bindingErr := asBindingError(err); bindingErr != nil {
			log.Status = StatusError
			log.Error = bindingErr.Error()
		} else {
			log.Status = StatusFailure
			log.Error = err.Error()
		}
	}

	if err != nil {
		// log.Error, not zap.Error(err): for a binding failure, err's own
		// Error() renders the raw LUserData carrier ("userdata: 0x..."),
		// not the wrapped message — log.Error already holds the right one.
		e.logger.Warn("policy script execution failed",
			zap.String("policy_id", p.ID),
			zap.String("status", string(log.Status)),
			zap.String("error", log.Error),
		)
	}

	e.appendLog(log)
}

// validateScript compiles script without executing it, so Register fails
// fast on a syntax error instead of the policy silently failing on its
// first trigger.
func validateScript(script string) error {
	L := lua.NewState()
	defer L.Close()

	_, err := L.LoadString(script)
	return err
}
