// Package agent continuously observes one server's runtime generation and
// Minecraft readiness. It is read-only: it never starts, stops, or signals the
// server, and it does not hold the lifecycle mutation lock.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/liquidcatmofu/mcwarden/internal/app"
	"github.com/liquidcatmofu/mcwarden/internal/minecraft"
	"github.com/liquidcatmofu/mcwarden/internal/runtime"
	"github.com/liquidcatmofu/mcwarden/internal/state"
)

// DefaultInterval is the default observation and retry interval.
const DefaultInterval = 2 * time.Second

// Event type names emitted by the agent.
const (
	EventAgentStarted          = "agent_started"
	EventSnapshot              = "snapshot"
	EventGenerationObserved    = "generation_observed"
	EventMinecraftStateChanged = "minecraft_state_changed"
	EventGenerationExited      = "generation_exited"
	EventRuntimeStopped        = "runtime_stopped"
	EventObservationError      = "observation_error"
	EventObservationRecovered  = "observation_recovered"
	EventAgentStopped          = "agent_stopped"
)

// Loader builds a fresh service for the selected server. The agent calls it
// again after every generation exit and while the runtime is stopped so it can
// observe replacements without reusing a pinned runtime instance.
type Loader func(configPath, serverName string) (app.Service, error)

// Snapshot is one observation of runtime liveness and generation-scoped
// Minecraft readiness. Generation is an opaque runtime key and must not be
// parsed.
type Snapshot struct {
	Generation string
	Runtime    runtime.State
	Minecraft  minecraft.State
}

// ExitClassification describes whether a generation's exit is explained by an
// MCWarden accepted exit intent. It never means "crashed" when unexpected.
type ExitClassification string

const (
	ExitIntentional ExitClassification = "intentional"
	ExitUnexpected  ExitClassification = "unexpected"
	ExitUnknown     ExitClassification = "unknown"
)

// Event is the typed agent event stream. It deliberately excludes passwords,
// RCON credentials, and external command output.
type Event struct {
	Time time.Time `json:"time"`
	// Server is the selected server name. It is omitted until the name is
	// resolved, which can only happen after the first successful load when the
	// CLI did not specify a server.
	Server         string              `json:"server,omitempty"`
	Type           string              `json:"type"`
	Generation     string              `json:"generation,omitempty"`
	Runtime        runtime.State       `json:"runtime,omitempty"`
	Minecraft      minecraft.State     `json:"minecraft,omitempty"`
	Exit           *runtime.ExitStatus `json:"exit,omitempty"`
	Classification ExitClassification  `json:"classification,omitempty"`
	Operation      state.ExitOperation `json:"operation,omitempty"`
	Error          string              `json:"error,omitempty"`
}

// Clock and Ticker are injectable so tests do not need to sleep.
type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTicker(d time.Duration) Ticker {
	return &realTicker{ticker: time.NewTicker(d)}
}

type realTicker struct {
	ticker *time.Ticker
}

func (t *realTicker) C() <-chan time.Time { return t.ticker.C }
func (t *realTicker) Stop()               { t.ticker.Stop() }

// Options configures one agent process for one server.
type Options struct {
	ConfigPath string
	// ServerName is the CLI selection. It may be empty, in which case the first
	// successful load resolves and pins the selected name.
	ServerName string
	Interval   time.Duration
	Loader     Loader
	Output     io.Writer
	JSON       bool
	Clock      Clock
	// AcquireLock is optional and exists for tests. It defaults to the
	// agent-only runtime flock.
	AcquireLock func(server string) (*state.Lock, error)
}

// Run observes until ctx is cancelled. A normal cancellation returns nil: for
// this long-running command, SIGINT/SIGTERM is shutdown, not failure.
func Run(ctx context.Context, opts Options) error {
	if opts.Loader == nil {
		return fmt.Errorf("agent loader is not configured: %w", app.ErrInvalid)
	}
	if opts.Interval <= 0 {
		return fmt.Errorf("agent interval must be positive: %w", app.ErrInvalid)
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.AcquireLock == nil {
		opts.AcquireLock = state.LockAgent
	}
	a := &agent{opts: opts, serverName: opts.ServerName, requestName: opts.ServerName}
	return a.run(ctx)
}

type agent struct {
	opts        Options
	serverName  string
	requestName string
	lock        *state.Lock
	started     bool

	haveSnapshot bool
	lastRuntime  runtime.State

	lastGeneration       string
	lastMinecraft        minecraft.State
	haveObservationError bool
	lastObservationError string
}

func (a *agent) run(ctx context.Context) error {
	defer a.release()
	for {
		if ctx.Err() != nil {
			break
		}
		// With an explicit or already resolved name, take the agent lock before
		// loading so a duplicate agent is rejected even while configuration is
		// unreadable.
		if a.serverName != "" {
			if err := a.ensureStarted(); err != nil {
				return err
			}
		}

		service, err := a.opts.Loader(a.opts.ConfigPath, a.requestName)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if err := a.observationError(fmt.Errorf("load service: %w", err)); err != nil {
				return err
			}
			if !a.waitInterval(ctx) {
				break
			}
			continue
		}

		resolved := service.ServerName
		if resolved == "" {
			resolved = a.serverName
		}
		if resolved == "" {
			return fmt.Errorf("selected server name is unavailable: %w", app.ErrInvalid)
		}
		if a.serverName == "" {
			a.serverName = resolved
			a.requestName = resolved
		}
		if resolved != a.serverName {
			if err := a.observationError(fmt.Errorf("loader selected server %q, agent is monitoring %q", resolved, a.serverName)); err != nil {
				return err
			}
			if !a.waitInterval(ctx) {
				break
			}
			continue
		}
		if err := a.ensureStarted(); err != nil {
			return err
		}

		delay, err := a.observeService(ctx, service)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			break
		}
		if delay && !a.waitInterval(ctx) {
			break
		}
	}

	if a.started {
		if err := a.emit(Event{Type: EventAgentStopped}); err != nil {
			return err
		}
	}
	return nil
}

func (a *agent) ensureStarted() error {
	if a.lock == nil {
		lock, err := a.opts.AcquireLock(a.serverName)
		if err != nil {
			return fmt.Errorf("agent lock for server %q: %w", a.serverName, err)
		}
		a.lock = lock
	}
	if !a.started {
		if err := a.emit(Event{Type: EventAgentStarted}); err != nil {
			return err
		}
		a.started = true
	}
	return nil
}

func (a *agent) release() {
	if a.lock != nil {
		_ = a.lock.Unlock()
		a.lock = nil
	}
}

func (a *agent) waitInterval(ctx context.Context) bool {
	ticker := a.opts.Clock.NewTicker(a.opts.Interval)
	defer ticker.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-ticker.C():
		return true
	}
}

// observeService observes one fresh service. delay is true when the next load
// should wait one interval; a confirmed generation exit reloads immediately to
// discover the replacement generation.
func (a *agent) observeService(ctx context.Context, service app.Service) (delay bool, err error) {
	if service.Runtime == nil {
		if err := a.observationError(fmt.Errorf("runtime is not configured: %w", app.ErrUnsupported)); err != nil {
			return false, err
		}
		return true, nil
	}
	state, err := service.Runtime.State(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		if err := a.observationError(fmt.Errorf("runtime state: %w", err)); err != nil {
			return false, err
		}
		return true, nil
	}
	if state == runtime.Running {
		return a.observeRunning(ctx, service)
	}

	mcState := minecraft.Unknown
	if state == runtime.Stopped {
		mcState = minecraft.Stopped
	}
	snapshot := Snapshot{Runtime: state, Minecraft: mcState}
	if err := a.observationOK(snapshot); err != nil {
		return false, err
	}
	switch {
	case state == runtime.Stopped && a.lastRuntime == runtime.Running:
		err = a.emit(Event{
			Type:      EventRuntimeStopped,
			Runtime:   state,
			Minecraft: mcState,
		})
	case !a.haveSnapshot || state != a.lastRuntime:
		err = a.emit(Event{
			Type:      EventSnapshot,
			Runtime:   state,
			Minecraft: mcState,
		})
	}
	if err != nil {
		return false, err
	}
	a.haveSnapshot = true
	a.lastRuntime = state
	return true, nil
}

func (a *agent) observeRunning(ctx context.Context, service app.Service) (delay bool, err error) {
	generation, ok, err := generation(ctx, service.Runtime)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		if err := a.observationError(fmt.Errorf("runtime generation: %w", err)); err != nil {
			return false, err
		}
		return true, nil
	}
	if !ok {
		if err := a.observationError(fmt.Errorf("runtime generation is unavailable: %w", app.ErrStateUnknown)); err != nil {
			return false, err
		}
		return true, nil
	}

	mcState, observeErr := a.observeMinecraft(ctx, service.Observer, generation)
	if ctx.Err() != nil {
		return false, nil
	}
	snapshot := Snapshot{Generation: generation, Runtime: runtime.Running, Minecraft: mcState}

	if generation != a.lastGeneration {
		if observeErr == nil {
			if err := a.observationOK(snapshot); err != nil {
				return false, err
			}
		}
		if err := a.emit(Event{
			Type:       EventGenerationObserved,
			Generation: generation,
			Runtime:    runtime.Running,
			Minecraft:  mcState,
		}); err != nil {
			return false, err
		}
		a.lastGeneration = generation
		a.lastMinecraft = mcState
	} else {
		if observeErr == nil {
			if err := a.observationOK(snapshot); err != nil {
				return false, err
			}
			if mcState != a.lastMinecraft {
				if err := a.emit(Event{
					Type:       EventMinecraftStateChanged,
					Generation: generation,
					Minecraft:  mcState,
				}); err != nil {
					return false, err
				}
				a.lastMinecraft = mcState
			}
		}
	}
	if observeErr != nil {
		if err := a.observationError(observeErr); err != nil {
			return false, err
		}
	}
	a.haveSnapshot = true
	a.lastRuntime = runtime.Running
	return a.waitGeneration(ctx, service, generation)
}

type waitResult struct {
	exit runtime.ExitStatus
	err  error
}

func (a *agent) waitGeneration(ctx context.Context, service app.Service, generation string) (delay bool, err error) {
	result := make(chan waitResult, 1)
	go func() {
		exit, waitErr := service.Runtime.Wait(ctx)
		result <- waitResult{exit: exit, err: waitErr}
	}()

	ticker := a.opts.Clock.NewTicker(a.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, nil
		case observed := <-result:
			if ctx.Err() != nil {
				return false, nil
			}
			if observed.err != nil {
				if err := a.observationError(fmt.Errorf("wait for runtime generation exit: %w", observed.err)); err != nil {
					return false, err
				}
				return true, nil
			}
			exit := observed.exit
			classification, operation, classifyErr := a.classifyExit(service, generation)
			if err := a.emit(Event{
				Type:           EventGenerationExited,
				Generation:     generation,
				Exit:           &exit,
				Classification: classification,
				Operation:      operation,
			}); err != nil {
				return false, err
			}
			if classifyErr != nil {
				if err := a.observationError(classifyErr); err != nil {
					return false, err
				}
			}
			return false, nil
		case <-ticker.C():
			mcState, observeErr := a.observeMinecraft(ctx, service.Observer, generation)
			if ctx.Err() != nil {
				return false, nil
			}
			if observeErr != nil {
				if err := a.observationError(observeErr); err != nil {
					return false, err
				}
				continue
			}
			snapshot := Snapshot{Generation: generation, Runtime: runtime.Running, Minecraft: mcState}
			if err := a.observationOK(snapshot); err != nil {
				return false, err
			}
			if mcState == a.lastMinecraft {
				continue
			}
			if err := a.emit(Event{
				Type:       EventMinecraftStateChanged,
				Generation: generation,
				Minecraft:  mcState,
			}); err != nil {
				return false, err
			}
			a.lastMinecraft = mcState
		}
	}
}

func (a *agent) observeMinecraft(ctx context.Context, observer minecraft.Observer, generation string) (minecraft.State, error) {
	if observer == nil {
		return minecraft.Unknown, nil
	}
	state, err := observer.State(ctx, generation)
	if err != nil {
		if ctx.Err() != nil {
			return minecraft.Unknown, ctx.Err()
		}
		return minecraft.Unknown, fmt.Errorf("minecraft observation: %w", err)
	}
	return state, nil
}

func generation(ctx context.Context, rt runtime.Runtime) (string, bool, error) {
	generational, ok := rt.(runtime.Generational)
	if !ok {
		return "", false, nil
	}
	return generational.Generation(ctx)
}

// classifyExit reads the operation ledger for the exact generation that exited.
// Exit status is deliberately ignored: only accepted MCWarden intent makes an
// exit intentional, and a missing match means "not explained by accepted
// intent", not "crashed".
func (a *agent) classifyExit(service app.Service, generation string) (ExitClassification, state.ExitOperation, error) {
	store := service.OperationStore
	if store == nil && service.Operations != nil {
		var err error
		store, err = service.Operations()
		if err != nil {
			return ExitUnknown, "", fmt.Errorf("operation ledger: %w", err)
		}
	}
	if store == nil {
		return ExitUnexpected, "", nil
	}
	rec, exists, err := store.Load()
	if err != nil {
		return ExitUnknown, "", fmt.Errorf("operation ledger: %w", err)
	}
	if !exists {
		return ExitUnexpected, "", nil
	}
	intent, ok := rec.FindExitIntent(generation)
	if !ok {
		return ExitUnexpected, "", nil
	}
	switch intent.Phase {
	case state.IntentAccepted:
		return ExitIntentional, intent.Operation, nil
	case state.IntentPrepared:
		return ExitUnknown, intent.Operation, nil
	default:
		// Load validates phases, but never guess intentional from unknown data.
		return ExitUnknown, "", nil
	}
}

func (a *agent) observationError(err error) error {
	message := err.Error()
	if a.haveObservationError && a.lastObservationError == message {
		return nil
	}
	a.haveObservationError = true
	a.lastObservationError = message
	return a.emit(Event{Type: EventObservationError, Error: message})
}

func (a *agent) observationOK(snapshot Snapshot) error {
	if !a.haveObservationError {
		return nil
	}
	a.haveObservationError = false
	a.lastObservationError = ""
	return a.emit(Event{
		Type:       EventObservationRecovered,
		Generation: snapshot.Generation,
		Runtime:    snapshot.Runtime,
		Minecraft:  snapshot.Minecraft,
	})
}

func (a *agent) emit(event Event) error {
	if event.Time.IsZero() {
		event.Time = a.opts.Clock.Now()
	}
	if event.Server == "" {
		event.Server = a.serverName
	}
	if a.opts.JSON {
		return json.NewEncoder(a.opts.Output).Encode(event)
	}
	_, err := io.WriteString(a.opts.Output, formatEvent(event))
	return err
}

func formatEvent(event Event) string {
	fields := make([]string, 0, 9)
	if !event.Time.IsZero() {
		fields = append(fields, event.Time.Format(time.RFC3339))
	}
	if event.Server != "" {
		fields = append(fields, event.Server)
	}
	fields = append(fields, event.Type)
	if event.Runtime != "" {
		fields = append(fields, "runtime="+string(event.Runtime))
	}
	if event.Minecraft != "" {
		fields = append(fields, "minecraft="+string(event.Minecraft))
	}
	if event.Classification != "" {
		fields = append(fields, "classification="+string(event.Classification))
	}
	if event.Operation != "" {
		fields = append(fields, "operation="+string(event.Operation))
	}
	if event.Exit != nil {
		fields = append(fields, "exit="+formatExit(*event.Exit))
	}
	if event.Error != "" {
		fields = append(fields, "error="+strconv.Quote(event.Error))
	}
	return strings.Join(fields, " ") + "\n"
}

func formatExit(exit runtime.ExitStatus) string {
	switch {
	case !exit.Known:
		return "unknown"
	case exit.Signal != nil && exit.Signal.Name != "":
		return exit.Signal.Name
	case exit.Signal != nil:
		return "signal:" + strconv.Itoa(exit.Signal.Number)
	default:
		return strconv.Itoa(exit.Code)
	}
}
