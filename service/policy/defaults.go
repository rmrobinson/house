package policy

import (
	_ "embed"
	"fmt"
)

//go:embed scripts/sys_occupancy.lua
var sysOccupancyScript string

//go:embed scripts/sys_power_restore.lua
var sysPowerRestoreScript string

// RegisterSystemConditionTypes registers the condition types the shipped
// system policies are built from: "sys.any-motion-detected" (fires on a
// "motion.detected" event on the engine's Bus) and "sys.power-restored"
// (fires on a "power.restored" event). Whatever wires in real motion
// detection or power-restore detection from the device stream should
// Publish those events on Engine.Bus() — the engine doesn't derive them
// itself.
//
// LoadSystemPolicies calls this itself. A caller that also loads persisted
// policies (see LoadPersistedPolicies) must call it directly, before
// LoadPersistedPolicies, since a persisted sys.* policy needs these types
// registered to rebuild its condition tree — and must not call
// LoadSystemPolicies for that same purpose, since it would register both
// the types (a second time, panicking) and the defaults out of order.
func RegisterSystemConditionTypes(e *Engine) {
	RegisterConditionType(e.registry, "sys.any-motion-detected", func(_ struct{}) Condition {
		return NewEventCondition(e.bus, "motion.detected", nil, nil)
	})
	RegisterConditionType(e.registry, "sys.power-restored", func(_ struct{}) Condition {
		return NewEventCondition(e.bus, "power.restored", nil, nil)
	})
}

// LoadDefaultSystemPolicies registers the engine's default system policies
// (well-known IDs prefixed "sys."), loaded from scripts/, skipping any ID
// already registered on e. That skip is what lets a persisted policy —
// loaded via LoadPersistedPolicies before this runs — take precedence over
// the shipped default for the same ID, whether it's a genuine user
// override or simply a previous run's copy of the default itself.
//
// It assumes RegisterSystemConditionTypes has already run; LoadSystemPolicies
// is the entry point that guarantees that for the no-persistence case.
func LoadDefaultSystemPolicies(e *Engine) error {
	policies := []*Policy{
		{
			ID:            "sys.occupancy",
			ConditionExpr: Use("sys.any-motion-detected", struct{}{}),
			Script:        sysOccupancyScript,
		},
		{
			ID:            "sys.power-restore",
			ConditionExpr: Use("sys.power-restored", struct{}{}),
			Script:        sysPowerRestoreScript,
		},
	}

	for _, p := range policies {
		if e.isRegistered(p.ID) {
			continue
		}
		if err := e.Register(p); err != nil {
			return fmt.Errorf("policy: loading default system policy %q: %w", p.ID, err)
		}
	}
	return nil
}

// LoadSystemPolicies registers the system condition types, the generic
// builtin and location-based condition types (see
// RegisterBuiltinConditionTypes, RegisterLocationConditionTypes), and the
// default system policies built from the former. They use the same Register
// path as user policies, so a user policy can override any of them by
// re-registering the same ID.
//
// This is the entry point for the no-persistence case. A caller that also
// loads persisted policies should instead call RegisterSystemConditionTypes,
// RegisterBuiltinConditionTypes and RegisterLocationConditionTypes, then
// LoadPersistedPolicies, then LoadDefaultSystemPolicies, in that order — see
// LoadPersistedPolicies.
func LoadSystemPolicies(e *Engine) error {
	RegisterSystemConditionTypes(e)
	RegisterBuiltinConditionTypes(e)
	RegisterLocationConditionTypes(e)
	return LoadDefaultSystemPolicies(e)
}
