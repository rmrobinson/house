package policy

import "go.uber.org/zap"

// HouseStateChangedTopic is the engine Bus topic a HomeAPI implementation
// backing GetHouseState with live data (e.g. service/policy/housestate.
// Adapter, subscribed to HouseService.StreamHouseUpdates) should Publish on
// whenever a GetHouseState key such as "occupied" or "mode" changes, so
// "sys.occupied" (and any other condition type built on GetHouseState) can
// react without polling itself. It carries no meaningful Payload — like
// "device.updated.<id>", it's just a signal to re-check, not itself a fact.
const HouseStateChangedTopic = "house.state.changed"

// RegisterSystemConditionTypes registers the condition types built on
// Go-native plumbing (the Bus, GetHouseState) rather than a generic
// attribute path, the same way RegisterBuiltinConditionTypes/
// RegisterLocationConditionTypes register theirs: "sys.any-motion-detected"
// (fires on a "motion.detected" event on the engine's Bus),
// "sys.power-restored" (fires on a "power.restored" event), and
// "sys.occupied" (true while HomeAPI.GetHouseState("occupied") is true,
// re-checked on HouseStateChangedTopic). Whatever wires in real motion
// detection or power-restore detection from the device stream should
// Publish those events on Engine.Bus() — the engine doesn't derive them
// itself — and whatever backs GetHouseState with live data should Publish
// HouseStateChangedTopic whenever it changes.
//
// These are building blocks only, not policies themselves: the engine
// ships no default policies of its own — every policy, including one built
// on these condition types (e.g. "switch to home mode once occupied"), is
// user data authored through adminui's policy editor and persisted via
// Store, exactly like any other. A caller that also loads persisted
// policies (see LoadPersistedPolicies) must call this before that, since a
// persisted policy using one of these types needs it registered to rebuild
// its condition tree.
func RegisterSystemConditionTypes(e *Engine) {
	RegisterConditionType(e.registry, "sys.any-motion-detected", func(_ struct{}) Condition {
		return NewEventCondition(e.bus, "motion.detected", nil, nil)
	})
	RegisterConditionType(e.registry, "sys.power-restored", func(_ struct{}) Condition {
		return NewEventCondition(e.bus, "power.restored", nil, nil)
	})
	RegisterConditionType(e.registry, "sys.occupied", func(_ struct{}) Condition {
		return NewPredicateCondition(e.bus, HouseStateChangedTopic, func() bool {
			v, err := e.home.GetHouseState("occupied")
			if err != nil {
				e.logger.Warn("sys.occupied: GetHouseState(\"occupied\") failed, treating as unoccupied", zap.Error(err))
				return false
			}
			occupied, _ := v.(bool)
			return occupied
		})
	})
}
