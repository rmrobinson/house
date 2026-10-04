package policy

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"go.uber.org/zap"
)

// deviceKindUpdatedTopicPrefix prefixes the Bus topic Engine.UpdateDeviceState
// additionally publishes on for every device of a given kind (Payload: the
// device's id), alongside the per-device "device.updated.<id>" topic - so a
// condition watching a whole kind of device doesn't need to know every
// device id up front, or resubscribe as devices appear.
const deviceKindUpdatedTopicPrefix = "device.kind.updated."

// DeviceClause is one test a device must pass: GetState(Key) compared to
// Value with Op - "eq" (the default when empty), "ne", "lt", "lte", "gt" or
// "gte". Every ordered comparison is numeric.
type DeviceClause struct {
	Key   string
	Op    string
	Value any
}

// AnyDeviceParams parameterizes the "devices.any-match" condition type:
// true while at least one device of Kind (as tagged by UpdateDeviceState -
// "ups", "sensor", "light", ...) passes every one of Clauses.
type AnyDeviceParams struct {
	Kind    string
	Clauses []DeviceClause
}

func (c DeviceClause) matches(home HomeAPI, id string) (bool, error) {
	got, err := home.GetState(id, c.Key)
	if err != nil {
		return false, err
	}

	switch c.Op {
	case "", "eq":
		return attributeValuesEqual(got, c.Value), nil
	case "ne":
		return !attributeValuesEqual(got, c.Value), nil
	case "lt", "lte", "gt", "gte":
		g, err := toFloat64(got)
		if err != nil {
			return false, err
		}
		w, err := toFloat64(c.Value)
		if err != nil {
			return false, err
		}
		switch c.Op {
		case "lt":
			return g < w, nil
		case "lte":
			return g <= w, nil
		case "gt":
			return g > w, nil
		default:
			return g >= w, nil
		}
	default:
		return false, fmt.Errorf("unknown op %q", c.Op)
	}
}

// anyDeviceCondition implements "devices.any-match". Unlike a plain level
// condition, which would stay true (and so silently swallow a second
// device's alarm) while a first device is still matching, it re-fires -
// false then true - every time a device *newly* joins the matching set. A
// policy built on it must therefore use OnConditionFalse=Complete, so that
// pulse doesn't interrupt an alert script already running. The script learns
// which device(s) newly matched from the "trigger" global (see
// TriggerContextProvider); this condition must be the policy's top-level
// condition for that, and for the re-fire, to work.
type anyDeviceCondition struct {
	e      *Engine
	params AnyDeviceParams

	mu       sync.Mutex
	matching map[string]struct{}
	// joined is the ids that newly entered the matching set on the most
	// recent transition to true, sorted. It is stored before onChange(true)
	// is called, from the same goroutine the engine's synchronous
	// TriggerDeviceIDs read happens on, so it can't be overwritten between.
	joined []string
}

// TriggerDeviceIDs implements TriggerContextProvider.
func (c *anyDeviceCondition) TriggerDeviceIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.joined...)
}

func (c *anyDeviceCondition) snapshot() map[string]struct{} {
	set := make(map[string]struct{})
	for _, id := range c.e.DevicesOfKind(c.params.Kind) {
		ok := true
		for _, clause := range c.params.Clauses {
			m, err := clause.matches(c.e.home, id)
			if err != nil {
				// A device of this kind that lacks the key (e.g. a sensor
				// with no water trait) simply doesn't match.
				c.e.logger.Debug("devices.any-match: clause not evaluable, treating as non-matching",
					zap.String("deviceId", id), zap.String("key", clause.Key), zap.Error(err))
				ok = false
				break
			}
			if !m {
				ok = false
				break
			}
		}
		if ok {
			set[id] = struct{}{}
		}
	}
	return set
}

func (c *anyDeviceCondition) Evaluate() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.matching) > 0
}

func (c *anyDeviceCondition) Start(ctx context.Context, onChange func(bool)) {
	c.mu.Lock()
	c.matching = c.snapshot()
	c.mu.Unlock()

	topic := deviceKindUpdatedTopicPrefix + c.params.Kind
	ch := c.e.bus.Subscribe(topic)

	go func() {
		defer c.e.bus.Unsubscribe(topic, ch)

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}

				next := c.snapshot()

				c.mu.Lock()
				prev := c.matching
				c.matching = next
				var joined []string
				for id := range next {
					if _, was := prev[id]; !was {
						joined = append(joined, id)
					}
				}
				sort.Strings(joined)
				if len(joined) > 0 {
					c.joined = joined
				}
				c.mu.Unlock()

				switch {
				case len(joined) > 0 && len(prev) > 0:
					onChange(false)
					onChange(true)
				case len(joined) > 0:
					onChange(true)
				case len(next) == 0 && len(prev) > 0:
					onChange(false)
				}
			}
		}
	}()
}

func registerDeviceConditionTypes(e *Engine) {
	RegisterConditionType(e.registry, "devices.any-match", func(p AnyDeviceParams) Condition {
		return &anyDeviceCondition{e: e, params: p}
	})
}
