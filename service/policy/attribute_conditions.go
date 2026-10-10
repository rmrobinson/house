package policy

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"go.uber.org/zap"
)

// AttributeThresholdParams parameterizes the "attribute.threshold" condition
// type: a numeric fact over home.GetState(DeviceID, Key), reacting to
// bridgehome's per-device "device.updated.<id>" signal rather than polling.
// Set High == Low for a plain, non-hysteresis threshold (e.g.
// "temperature > 25").
//
// Falling picks which edge is the alarm edge (see hysteresisNext): false
// (the default) is "rises to/above High, recovers at/below Low" (e.g.
// "temperature is above 25"); true is the mirror image, "falls to/below
// Low, recovers at/above High" (e.g. "temperature is below −15", "battery
// is below 5%"). Getting this backwards is a common mistake, since it's
// easy to read High/Low as "the alarm threshold/the recovery threshold" in
// either direction - Falling exists precisely so a "falls below" fact
// doesn't have to be faked by negating the reading.
type AttributeThresholdParams struct {
	DeviceID  string
	Key       string
	High, Low float64
	Falling   bool
}

// AttributeEqualsParams parameterizes the "attribute.equals" condition type:
// true while home.GetState(DeviceID, Key) equals Value. Reacts
// near-instantly to bridgehome's per-device "device.updated.<id>" signal
// rather than polling.
//
// Value is compared with attributeValuesEqual, not a bare reflect.DeepEqual:
// a JSON round-trip through persistence (see store.go) turns every JSON
// number into float64, so a Value authored as e.g. an int against an
// int64-typed attribute would otherwise silently stop matching the moment
// the policy is reloaded from the store. attributeValuesEqual normalizes
// both sides numerically before comparing so that doesn't happen.
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
// condition types built on HomeAPI.GetState, the Bus, and the new
// PredicateCondition/IdleCondition/ScheduleCondition primitives:
// "attribute.threshold", "attribute.equals", "schedule.daily",
// "event.idle-for", and "devices.any-match" (see device_conditions.go). Unlike RegisterSystemConditionTypes's three
// hand-derived sys.* types, any of these four addresses any
// device/attribute/topic combination, present or future, without a Go code
// change - a policy definition is enough.
//
// Callers follow the same ordering rule as RegisterSystemConditionTypes:
// see LoadPersistedPolicies's doc comment for when to call this relative to
// loading persisted policies.
func RegisterBuiltinConditionTypes(e *Engine) {
	RegisterConditionType(e.registry, "attribute.threshold", func(p AttributeThresholdParams) Condition {
		high, low := p.High, p.Low
		if low > high {
			e.logger.Warn("attribute.threshold: Low > High, swapping - check the policy's params",
				zap.String("deviceId", p.DeviceID), zap.String("key", p.Key),
				zap.Float64("high", high), zap.Float64("low", low))
			high, low = low, high
		}

		var mu sync.Mutex
		last := low

		read := func() float64 {
			v, err := e.home.GetState(p.DeviceID, p.Key)
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
			e.logger.Warn("attribute.threshold: GetState failed, holding last value",
				zap.String("deviceId", p.DeviceID), zap.String("key", p.Key), zap.Error(err))
			mu.Lock()
			defer mu.Unlock()
			return last
		}

		return NewHysteresisPredicateCondition(e.bus, "device.updated."+p.DeviceID, read, high, low, p.Falling)
	})

	RegisterConditionType(e.registry, "attribute.equals", func(p AttributeEqualsParams) Condition {
		var mu sync.Mutex
		var last bool

		fn := func() bool {
			v, err := e.home.GetState(p.DeviceID, p.Key)
			if err != nil {
				e.logger.Warn("attribute.equals: GetState failed, holding last value",
					zap.String("deviceId", p.DeviceID), zap.String("key", p.Key), zap.Error(err))
				mu.Lock()
				defer mu.Unlock()
				return last
			}

			cur := attributeValuesEqual(v, p.Value)
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

		hour, minute := p.Hour, p.Minute
		if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			normHour := ((hour % 24) + 24) % 24
			normMinute := ((minute % 60) + 60) % 60
			e.logger.Warn("schedule.daily: Hour/Minute out of range, normalizing",
				zap.Int("hour", hour), zap.Int("minute", minute),
				zap.Int("normalizedHour", normHour), zap.Int("normalizedMinute", normMinute))
			hour, minute = normHour, normMinute
		}

		weekdays := make([]time.Weekday, len(p.Weekdays))
		for i, wd := range p.Weekdays {
			weekdays[i] = time.Weekday(wd)
		}
		return NewScheduleCondition(loc, hour, minute, weekdays...)
	})

	RegisterConditionType(e.registry, "event.idle-for", func(p EventIdleForParams) Condition {
		duration := time.Duration(p.DurationSeconds) * time.Second
		if p.DurationSeconds <= 0 {
			e.logger.Warn("event.idle-for: DurationSeconds must be positive, defaulting to 1s",
				zap.String("topic", p.Topic), zap.Int("durationSeconds", p.DurationSeconds))
			duration = time.Second
		}
		return NewIdleCondition(e.bus, p.Topic, duration)
	})

	RegisterConditionType(e.registry, "button.action", func(p ButtonActionParams) Condition {
		return NewButtonActionCondition(e, p.DeviceID, p.Actions)
	})

	registerDeviceConditionTypes(e)
}

// toFloat64 coerces a HomeAPI.GetState result into a float64 for
// "attribute.threshold", accepting every numeric kind protoreflect's
// bridgehome walker can produce (see bridgehome.scalarToGo) plus bool as
// 0/1. The int32/uint32 cases never fire for a bridgehome-sourced value
// (scalarToGo always returns int64/uint64 regardless of the proto field's
// bit width) but are kept for any other HomeAPI implementation that might
// genuinely produce them.
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

// numericValue reports whether v is one of the numeric Go kinds
// attributeValuesEqual normalizes before comparing, and its float64 value if
// so. bool is deliberately excluded, unlike toFloat64: attribute.equals
// should never treat a boolean attribute as interchangeable with a bare 0/1
// Value, since exact-kind comparison is both more predictable for a policy
// author and unnecessary here - unlike int/uint/enum kinds, bool survives a
// JSON persistence round-trip exactly, so reflect.DeepEqual already handles
// it correctly on its own.
func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	default:
		return 0, false
	}
}

// attributeValuesEqual compares a HomeAPI.GetState result (got) against
// an "attribute.equals" policy's configured Value (want). Plain
// reflect.DeepEqual isn't enough: got and want routinely differ in Go
// numeric kind even when they represent the same value - got is whatever
// bridgehome's scalarToGo produced (int64/uint64/float64 depending on the
// proto field's kind), while want may have been authored as a different Go
// numeric type, and - critically - comes back as float64 for any numeric
// value once a policy has round-tripped through JSON persistence (see
// store.go), regardless of what it was authored as. Falling back to a
// numeric comparison whenever both sides are some numeric kind makes
// "attribute.equals" match consistently regardless of that history; every
// other kind (bool, string, …) still compares with reflect.DeepEqual.
func attributeValuesEqual(got, want any) bool {
	if gf, ok := numericValue(got); ok {
		if wf, ok := numericValue(want); ok {
			return gf == wf
		}
	}
	return reflect.DeepEqual(got, want)
}
