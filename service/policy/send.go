package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file implements home.send and home.trySend, the Lua bindings for commanding several devices
// at once.
//
// A script's own home.setLight/home.setState calls each block until the device confirms, one after
// another, so a script that commands two lights waits for the first before it even starts on the
// second. When one device is slow or out of radio range, every command to the others is delayed by
// however long that device takes - up to the command timeout per call - and the delayed commands
// land after whatever the user did next. These bindings fix that at the source: the script hands
// over the whole set of commands, and the engine runs each device's steps in order but different
// devices in parallel.

// sendStep is one command in a home.send batch.
type sendStep struct {
	deviceID string
	op       string // "setLight" or "setState"
	on       bool   // setLight
	key      string // setState
	value    any    // setState
}

// sendResult is the outcome of one sendStep. Exactly one of OK, Skipped and Superseded is true, or
// none of them (a real failure), with Err set for every case but OK.
type sendResult struct {
	OK bool
	// Skipped: not attempted, because an earlier step for the same device failed or the script was
	// cancelled.
	Skipped bool
	// Superseded: the bridge dropped this command because a newer command to the same device
	// replaced it (gRPC ABORTED). That is not a failure - the newer command governs the device -
	// so it never raises and is left out of the failure summary.
	Superseded bool
	Err        error
}

// failed reports whether r is a genuine failure, as opposed to success, a skipped step, or a
// command a newer one superseded.
func (r sendResult) failed() bool { return !r.OK && !r.Skipped && !r.Superseded }

// parseSendSteps validates the Go form of the table a script passed to home.send: a list of
// {id=, op=, ...} tables. An empty table is a valid, empty batch.
func parseSendSteps(v any) ([]sendStep, error) {
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil, nil // Lua can't tell {} the empty list from {} the empty map
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list of steps, got %T", v)
	}

	steps := make([]sendStep, 0, len(list))
	for i, raw := range list {
		n := i + 1
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("step %d: expected a table like {id=..., op=...}, got %T", n, raw)
		}
		id, _ := m["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("step %d: missing id", n)
		}
		op, _ := m["op"].(string)

		s := sendStep{deviceID: id, op: op}
		switch op {
		case "setLight":
			on, ok := m["on"].(bool)
			if !ok {
				return nil, fmt.Errorf("step %d: setLight needs on=true or on=false", n)
			}
			s.on = on
		case "setState":
			key, _ := m["key"].(string)
			if key == "" {
				return nil, fmt.Errorf("step %d: setState needs a key", n)
			}
			val, present := m["value"]
			if !present {
				return nil, fmt.Errorf("step %d: setState needs a value", n)
			}
			s.key, s.value = key, val
		default:
			return nil, fmt.Errorf("step %d: unknown op %q (want \"setLight\" or \"setState\")", n, op)
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// runSendSteps executes steps against api and returns one result per step, in the same order.
// Steps for the same device run one after another, in list order, and stop at that device's first
// failure (the rest are skipped: a brighten that couldn't turn the light on shouldn't go on to
// brighten it). Different devices run concurrently. It returns once every device is done.
//
// ctx is the script's context: once it is done (the script timed out or was interrupted) no
// further step is started - each device's remaining steps are skipped - though a command already
// in flight is waited for, since its goroutine writes into the results this returns.
//
// api is called from several goroutines at once, so it must be safe for concurrent use; every
// HomeAPI already has to be, since separate script runs share one.
func runSendSteps(ctx context.Context, api HomeAPI, steps []sendStep) []sendResult {
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]sendResult, len(steps))

	byDevice := make(map[string][]int)
	var order []string
	for i, s := range steps {
		if _, seen := byDevice[s.deviceID]; !seen {
			order = append(order, s.deviceID)
		}
		byDevice[s.deviceID] = append(byDevice[s.deviceID], i)
	}

	var wg sync.WaitGroup
	for _, id := range order {
		wg.Add(1)
		go func(indices []int) {
			defer wg.Done()
			runDeviceSteps(ctx, api, steps, results, indices)
		}(byDevice[id])
	}
	wg.Wait()
	return results
}

// runDeviceSteps runs one device's steps in order. Each goroutine writes only its own steps'
// slots in results, so results needs no lock.
func runDeviceSteps(ctx context.Context, api HomeAPI, steps []sendStep, results []sendResult, indices []int) {
	var stop error // why the remaining steps are skipped, once set
	for _, i := range indices {
		if stop == nil {
			if ctx.Err() != nil {
				stop = fmt.Errorf("skipped: the script was cancelled")
			}
		}
		if stop != nil {
			results[i] = sendResult{Skipped: true, Err: stop}
			continue
		}

		err := runSendStep(api, steps[i])
		switch {
		case err == nil:
			results[i] = sendResult{OK: true}
		case status.Code(err) == codes.Aborted:
			// The bridge replaced this command with a newer one to the same device. Whatever came
			// after it in this batch is moot too: the newer command now governs the device.
			results[i] = sendResult{Superseded: true, Err: err}
			stop = fmt.Errorf("skipped: an earlier step for this device was superseded")
		default:
			results[i] = sendResult{Err: err}
			stop = fmt.Errorf("skipped: an earlier step for this device failed")
		}
	}
}

// runSendStep runs one step. A panic in api is turned into that step's error: it happens on a
// goroutine of ours, where nothing would recover it, and would take the whole process down.
func runSendStep(api HomeAPI, s sendStep) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s panicked: %v", s.op, r)
		}
	}()
	switch s.op {
	case "setLight":
		return api.SetLight(s.deviceID, s.on)
	case "setState":
		return api.SetState(s.deviceID, s.key, s.value)
	}
	return fmt.Errorf("unknown op %q", s.op)
}

// summarizeSendFailures describes every step that genuinely failed (not skipped ones, and not ones
// a newer command superseded), naming the device the way a person would, or returns "" if none
// did.
func summarizeSendFailures(api HomeAPI, steps []sendStep, results []sendResult) string {
	var parts []string
	for i, r := range results {
		if !r.failed() {
			continue
		}
		name, err := api.GetDeviceName(steps[i].deviceID)
		if err != nil || name == "" {
			name = steps[i].deviceID
		}
		parts = append(parts, fmt.Sprintf("%s: %s: %v", name, steps[i].op, r.Err))
	}
	return strings.Join(parts, "; ")
}
