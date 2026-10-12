package policy

import (
	"fmt"
	"strings"
	"sync"
)

// This file implements home.send, the Lua binding for commanding several devices at once.
//
// A script's own home.setLight/home.setState calls each block until the device confirms, one after
// another, so a script that commands two lights waits for the first before it even starts on the
// second. When one device is slow or out of radio range, every command to the others is delayed by
// however long that device takes - up to the command timeout per call - and the delayed commands
// land after whatever the user did next. home.send fixes that at the source: the script hands over
// the whole set of commands, and the engine runs each device's steps in order but different
// devices in parallel.

// sendStep is one command in a home.send batch.
type sendStep struct {
	deviceID string
	op       string // "setLight" or "setState"
	on       bool   // setLight
	key      string // setState
	value    any    // setState
}

// sendResult is the outcome of one sendStep. A step skipped because an earlier step for the same
// device failed has OK false, Skipped true, and Err describing why.
type sendResult struct {
	OK      bool
	Skipped bool
	Err     error
}

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
func runSendSteps(api HomeAPI, steps []sendStep) []sendResult {
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
			failed := false
			for _, i := range indices {
				// Each goroutine writes only its own devices' slots, so results needs no lock.
				if failed {
					results[i] = sendResult{Skipped: true, Err: fmt.Errorf("skipped: an earlier step for this device failed")}
					continue
				}
				if err := runSendStep(api, steps[i]); err != nil {
					results[i] = sendResult{Err: err}
					failed = true
					continue
				}
				results[i] = sendResult{OK: true}
			}
		}(byDevice[id])
	}
	wg.Wait()
	return results
}

func runSendStep(api HomeAPI, s sendStep) error {
	switch s.op {
	case "setLight":
		return api.SetLight(s.deviceID, s.on)
	case "setState":
		return api.SetState(s.deviceID, s.key, s.value)
	}
	return fmt.Errorf("unknown op %q", s.op)
}

// summarizeSendFailures describes every step that actually failed (not the ones skipped as a
// consequence), naming the device the way a person would, or returns "" if none did. A script
// typically passes it straight to error().
func summarizeSendFailures(api HomeAPI, steps []sendStep, results []sendResult) string {
	var parts []string
	for i, r := range results {
		if r.OK || r.Skipped {
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
