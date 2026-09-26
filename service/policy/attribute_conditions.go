package policy

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"go.uber.org/zap"
)

// defaultAttributePollInterval is used by "attribute.threshold" when a
// policy doesn't specify one - short enough to feel live, long enough that
// polling a cache read (not a network call - see GetAttribute) is free.
const defaultAttributePollInterval = 10 * time.Second

// AttributeThresholdParams parameterizes the "attribute.threshold" condition
// type: true once home.GetAttribute(DeviceID, Key) reaches High, false again
// once it drops to Low. Set High == Low for a plain, non-hysteresis
// threshold (e.g. "temperature > 25").
type AttributeThresholdParams struct {
	DeviceID        string
	Key             string
	High, Low       float64
	IntervalSeconds int
}

// AttributeEqualsParams parameterizes the "attribute.equals" condition type:
// true while home.GetAttribute(DeviceID, Key) equals Value. Reacts
// near-instantly to bridgehome's per-device "device.updated.<id>" signal
// rather than polling.
type AttributeEqualsParams struct {
	DeviceID string
	Key      string
	Value    any
}

// ScheduleDailyParams parameterizes the "schedule.daily" condition type: a
// momentary trigger at Hour:Minute, restricted to Weekdays if any are given
// (time.Weekday values: 0=Sunday ... 6=Saturday), in the named IANA
// timezone, or the engine process's local zone if TZ is empty.
type ScheduleDailyParams struct {
	Hour, Minute int
	Weekdays     []int
	TZ           string
}

// EventIdleForParams parameterizes the "event.idle-for" condition type: true
// once DurationSeconds pass with no event published to Topic.
type EventIdleForParams struct {
	Topic           string
	DurationSeconds int
}

// RegisterBuiltinConditionTypes registers the generic, parameterized
// condition types built on HomeAPI.GetAttribute, the Bus, and the new
// PredicateCondition/IdleCondition/ScheduleCondition primitives:
// "attribute.threshold", "attribute.equals", "schedule.daily", and
// "event.idle-for". Unlike RegisterSystemConditionTypes's two hand-derived
// sys.* types, any of these four addresses any device/attribute/topic
// combination, present or future, without a Go code change - a policy
// definition is enough.
//
// Callers follow the same ordering rule as RegisterSystemConditionTypes: see
// LoadSystemPolicies (the no-persistence case) and LoadPersistedPolicies's
// doc comment (the persistence case) for when to call this.
func RegisterBuiltinConditionTypes(e *Engine) {
	RegisterConditionType(e.registry, "attribute.threshold", func(p AttributeThresholdParams) Condition {
		var mu sync.Mutex
		last := p.Low

		read := func() float64 {
			v, err := e.home.GetAttribute(p.DeviceID, p.Key)
			if err == nil {
				var f float64
				f, err = toFloat64(v)
				if err == nil {
					mu.Lock()
					last = f
					mu.Unlock()
					return f
				}
			}
			e.logger.Warn("attribute.threshold: GetAttribute failed, holding last value",
				zap.String("deviceId", p.DeviceID), zap.String("key", p.Key), zap.Error(err))
			mu.Lock()
			defer mu.Unlock()
			return last
		}

		interval := time.Duration(p.IntervalSeconds) * time.Second
		if interval <= 0 {
			interval = defaultAttributePollInterval
		}
		return NewHysteresisPollingCondition(read, p.High, p.Low, interval)
	})

	RegisterConditionType(e.registry, "attribute.equals", func(p AttributeEqualsParams) Condition {
		var mu sync.Mutex
		var last bool

		fn := func() bool {
			v, err := e.home.GetAttribute(p.DeviceID, p.Key)
			if err != nil {
				e.logger.Warn("attribute.equals: GetAttribute failed, holding last value",
					zap.String("deviceId", p.DeviceID), zap.String("key", p.Key), zap.Error(err))
				mu.Lock()
				defer mu.Unlock()
				return last
			}

			cur := reflect.DeepEqual(v, p.Value)
			mu.Lock()
			last = cur
			mu.Unlock()
			return cur
		}
		return NewPredicateCondition(e.bus, "device.updated."+p.DeviceID, fn)
	})

	RegisterConditionType(e.registry, "schedule.daily", func(p ScheduleDailyParams) Condition {
		loc := time.Local
		if p.TZ != "" {
			l, err := time.LoadLocation(p.TZ)
			if err != nil {
				e.logger.Warn("schedule.daily: invalid TZ, using local", zap.String("tz", p.TZ), zap.Error(err))
			} else {
				loc = l
			}
		}

		weekdays := make([]time.Weekday, len(p.Weekdays))
		for i, wd := range p.Weekdays {
			weekdays[i] = time.Weekday(wd)
		}
		return NewScheduleCondition(loc, p.Hour, p.Minute, weekdays...)
	})

	RegisterConditionType(e.registry, "event.idle-for", func(p EventIdleForParams) Condition {
		return NewIdleCondition(e.bus, p.Topic, time.Duration(p.DurationSeconds)*time.Second)
	})
}

// toFloat64 coerces a HomeAPI.GetAttribute result into a float64 for
// "attribute.threshold", accepting every numeric kind protoreflect's
// bridgehome walker can produce (see bridgehome.scalarToGo) plus bool as
// 0/1.
func toFloat64(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case int32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case uint64:
		return float64(t), nil
	case uint32:
		return float64(t), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("policy: cannot convert %T to a numeric value", v)
	}
}
