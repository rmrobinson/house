package policy

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// ErrNotImplemented is returned by a HomeAPI implementation for a method it
// doesn't back yet, the same way service/house handles unbuilt RPCs.
var ErrNotImplemented = errors.New("policy: not implemented")

// HomeAPI is the surface a policy script can call into. It covers
// device-backed state and commands, the engine's state cache, and
// notifications.
//
// GetHouseState/SetHouseState are part of the interface for forward
// compatibility with the house-state stream described in the policy engine
// plan, but that stream doesn't exist in this codebase yet: a concrete
// HomeAPI implementation should return ErrNotImplemented for both until it
// does, the same way service/house handles unbuilt RPCs.
//
// Three GetHouseState keys are reserved, well-known house properties rather
// than arbitrary state: "location.latitude" and "location.longitude"
// (float64, degrees, positive north/east) and "location.timezone" (string,
// an IANA zone name e.g. "America/Toronto"). RegisterLocationConditionTypes's
// "schedule.sun-event"/"schedule.daylight"/"schedule.date-range" condition
// types read them to compute sunrise/sunset and calendar-date facts for the
// house's own location; see LocationHomeAPI for a ready way to answer them
// without a full house-state stream.
//
// An implementation must be safe for concurrent use: separate script runs
// share one, and a single script's home.send calls it from several
// goroutines at once, one per device.
type HomeAPI interface {
	// Device state
	GetLight(id string) (bool, error)
	SetLight(id string, on bool) error
	GetSensor(id string) (float64, error)
	GetState(id, key string) (any, error)
	SetState(id, key string, value any) error
	// GetDeviceName returns id's configured display name, falling back to
	// id itself if unset - distinct from GetState's dotted path, which only
	// walks Device.details' populated oneof branch and can't reach
	// Device.Config.Name (it lives outside that oneof entirely).
	GetDeviceName(id string) (string, error)
	// HasState reports whether key's dot-path (the same path GetState
	// takes) is actually populated, rather than returning its value -
	// GetState's underlying Get() returns a zero-value message for an
	// *unset* optional message field, so e.g. GetState(id, "battery.state.
	// capacity_remaining_pct") on a device with no Battery trait silently
	// returns 0, indistinguishable from "battery actually at 0%".
	// HasState(id, "battery") is what tells those apart.
	HasState(id, key string) (bool, error)
	// GetDeviceRoom returns id's linked room's display name, or "" (no
	// error) if id isn't linked to any room - a device standing alone is a
	// normal, common state, not a failure. Room linking is HouseService
	// data (api/house.proto's DeviceRoomLink), entirely separate from
	// BridgeService's own Device - a HomeAPI implementation with no
	// HouseService connection should return ErrNotImplemented, the same
	// "unconfigured" story GetHouseState/SetHouseState use.
	GetDeviceRoom(id string) (string, error)
	// GetLastSeen returns when id was last heard from (Device.last_seen), or
	// the zero time (no error) if its bridge never reports one - "unknown",
	// not "never". Lives outside Device.details, so GetState can't reach it.
	GetLastSeen(id string) (time.Time, error)
	// IsReachable reports Device.address.is_reachable. A device whose bridge
	// reports no address at all is treated as reachable: absence of the
	// signal isn't evidence of a connectivity problem.
	IsReachable(id string) (bool, error)

	// House state
	GetHouseState(key string) (any, error)
	SetHouseState(key string, value any) error

	// State cache
	GetLastKnown(id string) (any, error)

	// Notifications
	Notify(event string, payload map[string]any) error
}

// LocationHomeAPI wraps another HomeAPI, answering GetHouseState's
// "location.latitude"/"location.longitude"/"location.timezone" keys (see
// HomeAPI's doc comment) from a fixed lat/lon/tz configured at construction
// time, and delegating every other call - including any other
// GetHouseState key - unchanged to the wrapped HomeAPI.
//
// It exists because no HomeAPI implementation in this codebase backs those
// three keys from a real source yet (bridgehome's GetHouseState is
// unconditionally ErrNotImplemented, staying strictly scoped to
// bridge.proto's contract - see its own doc comment): wrapping a
// deployment's HomeAPI with this makes the location-based condition types
// usable today from static configuration, with no change to them needed
// later once a house-service-backed GetHouseState replaces this static
// answer.
type LocationHomeAPI struct {
	HomeAPI
	lat, lon float64
	tz       string
}

// NewLocationHomeAPI wraps home, answering "location.latitude"/
// "location.longitude" with lat/lon (degrees) and "location.timezone" with
// tz (an IANA zone name, e.g. "America/Toronto"; empty leaves
// "location.timezone" to fall through to home, e.g. if home has some other
// way to answer it).
func NewLocationHomeAPI(home HomeAPI, lat, lon float64, tz string) *LocationHomeAPI {
	return &LocationHomeAPI{HomeAPI: home, lat: lat, lon: lon, tz: tz}
}

// GetHouseState implements policy.HomeAPI, answering the three location
// keys locally and delegating everything else to the wrapped HomeAPI.
func (l *LocationHomeAPI) GetHouseState(key string) (any, error) {
	switch key {
	case "location.latitude":
		return l.lat, nil
	case "location.longitude":
		return l.lon, nil
	case "location.timezone":
		if l.tz != "" {
			return l.tz, nil
		}
	}
	return l.HomeAPI.GetHouseState(key)
}

// bindingErrorTag marks a Lua error as having originated from a HomeAPI
// call (via raiseBindingError) rather than the script's own logic (e.g. a
// bare Lua error() call). It's carried through gopher-lua as an LUserData's
// Value so runScript can tell the two apart after DoString returns: a
// sticky *error flag can't do this correctly, because a script that pcalls
// around a failing binding call and then raises its own, unrelated error
// would otherwise have that later error misattributed to the binding.
type bindingErrorTag struct{ err error }

func (t *bindingErrorTag) Error() string { return t.err.Error() }

// raiseBindingError raises err as a Lua error tagged with bindingErrorTag,
// so it survives to be identified by asBindingError once DoString returns.
func raiseBindingError(L *lua.LState, err error) {
	ud := L.NewUserData()
	ud.Value = &bindingErrorTag{err: err}
	L.SetMetatable(ud, bindingErrorMetatable(L))
	L.Error(ud, 1)
}

// bindingErrorTypeName is the registry key for the metatable shared by every binding error value
// in one Lua state.
const bindingErrorTypeName = "policy.bindingError"

// bindingErrorMetatable returns the metatable that makes a binding error readable from a script.
// Without it, a script that catches a failing home.* call with pcall gets an opaque userdata and
// tostring(err) prints only "userdata: 0x...", so the only thing it can report about the failure
// is that one happened - not "device did not confirm command in time". runScript is unaffected:
// it still identifies a binding failure through ud.Value (see asBindingError), never the string.
func bindingErrorMetatable(L *lua.LState) *lua.LTable {
	if mt, ok := L.GetTypeMetatable(bindingErrorTypeName).(*lua.LTable); ok {
		return mt
	}
	mt := L.NewTypeMetatable(bindingErrorTypeName)
	L.SetField(mt, "__tostring", L.NewFunction(func(L *lua.LState) int {
		if ud, ok := L.Get(1).(*lua.LUserData); ok {
			if tag, ok := ud.Value.(*bindingErrorTag); ok {
				L.Push(lua.LString(tag.err.Error()))
				return 1
			}
		}
		L.Push(lua.LString("binding error"))
		return 1
	}))
	return mt
}

// asBindingError returns the underlying error if err (as returned by
// DoString) was raised via raiseBindingError, and nil otherwise — meaning
// it's the script's own doing (a Lua error() call, a type error, ...).
func asBindingError(err error) error {
	apiErr, ok := err.(*lua.ApiError)
	if !ok {
		return nil
	}
	ud, ok := apiErr.Object.(*lua.LUserData)
	if !ok {
		return nil
	}
	tag, ok := ud.Value.(*bindingErrorTag)
	if !ok {
		return nil
	}
	return tag.err
}

// sendBinding builds the Lua function behind home.send (raise true) and home.trySend (raise false).
// A malformed steps table always raises: that is a bug in the script, not a failed command.
func sendBinding(api HomeAPI, fail func(*lua.LState, error, string, ...any), raise bool) lua.LGFunction {
	name := "home.trySend"
	if raise {
		name = "home.send"
	}
	return func(L *lua.LState) int {
		steps, err := parseSendSteps(luaToGo(L.CheckTable(1)))
		if err != nil {
			L.ArgError(1, err.Error())
			return 0
		}
		results := runSendSteps(L.Context(), api, steps)
		summary := summarizeSendFailures(api, steps, results)

		if raise {
			if summary != "" {
				fail(L, errors.New(summary), "%s", name)
				return 0
			}
			return 0
		}

		out := L.NewTable()
		for i, r := range results {
			t := L.NewTable()
			t.RawSetString("ok", lua.LBool(r.OK))
			if r.Skipped {
				t.RawSetString("skipped", lua.LTrue)
			}
			if r.Superseded {
				t.RawSetString("superseded", lua.LTrue)
			}
			if r.Err != nil {
				t.RawSetString("error", lua.LString(r.Err.Error()))
			}
			out.RawSetInt(i+1, t)
		}
		L.Push(out)
		if summary != "" {
			L.Push(lua.LString(summary))
		} else {
			L.Push(lua.LNil)
		}
		return 2
	}
}

// registerHomeTable installs the "home" global table backed by api on L,
// plus home.findDevices(kind) backed directly by devicesOfKind (see
// Engine.DevicesOfKind) - a local-cache lookup, not a HomeAPI method, since
// no HomeAPI implementation needs to support device enumeration for this to
// work. A fresh LState gets a fresh table: there is no shared state between
// script runs.
func registerHomeTable(L *lua.LState, api HomeAPI, devicesOfKind func(kind string) []string) {
	fail := func(L *lua.LState, err error, format string, args ...any) {
		raiseBindingError(L, fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err))
	}

	L.RegisterModule("home", map[string]lua.LGFunction{
		"getLight": func(L *lua.LState) int {
			id := L.CheckString(1)
			on, err := api.GetLight(id)
			if err != nil {
				fail(L, err, "home.getLight(%q)", id)
				return 0
			}
			L.Push(lua.LBool(on))
			return 1
		},
		"setLight": func(L *lua.LState) int {
			id := L.CheckString(1)
			on := L.CheckBool(2)
			if err := api.SetLight(id, on); err != nil {
				fail(L, err, "home.setLight(%q)", id)
			}
			return 0
		},
		// home.send(steps) commands several devices at once: steps is a list of
		// {id=, op="setLight", on=} / {id=, op="setState", key=, value=} tables. A device's own
		// steps run in order (and stop at its first failure); different devices run in parallel,
		// so one slow device never delays the rest. Like every other home.* call it raises if a
		// command fails - after every device has been tried, naming each device that failed - so
		// a script that ignores the outcome can't silently log a success. A command a newer one
		// to the same device superseded is not a failure.
		"send": sendBinding(api, fail, true),
		// home.trySend(steps) is home.send that never raises for a failed command. It returns a
		// results list, one entry per step ({ok=, skipped=, superseded=, error=}), then a summary
		// of the genuine failures (nil if none), for a script that wants to react itself.
		"trySend": sendBinding(api, fail, false),
		"getSensor": func(L *lua.LState) int {
			id := L.CheckString(1)
			v, err := api.GetSensor(id)
			if err != nil {
				fail(L, err, "home.getSensor(%q)", id)
				return 0
			}
			L.Push(lua.LNumber(v))
			return 1
		},
		"getState": func(L *lua.LState) int {
			id := L.CheckString(1)
			key := L.CheckString(2)
			v, err := api.GetState(id, key)
			if err != nil {
				fail(L, err, "home.getState(%q, %q)", id, key)
				return 0
			}
			L.Push(goToLua(L, v))
			return 1
		},
		"setState": func(L *lua.LState) int {
			id := L.CheckString(1)
			key := L.CheckString(2)
			v := luaToGo(L.CheckAny(3))
			if err := api.SetState(id, key, v); err != nil {
				fail(L, err, "home.setState(%q, %q)", id, key)
			}
			return 0
		},
		"getDeviceName": func(L *lua.LState) int {
			id := L.CheckString(1)
			name, err := api.GetDeviceName(id)
			if err != nil {
				fail(L, err, "home.getDeviceName(%q)", id)
				return 0
			}
			L.Push(lua.LString(name))
			return 1
		},
		"hasState": func(L *lua.LState) int {
			id := L.CheckString(1)
			key := L.CheckString(2)
			has, err := api.HasState(id, key)
			if err != nil {
				fail(L, err, "home.hasState(%q, %q)", id, key)
				return 0
			}
			L.Push(lua.LBool(has))
			return 1
		},
		"getDeviceRoom": func(L *lua.LState) int {
			id := L.CheckString(1)
			room, err := api.GetDeviceRoom(id)
			if err != nil {
				fail(L, err, "home.getDeviceRoom(%q)", id)
				return 0
			}
			L.Push(lua.LString(room))
			return 1
		},
		// secondsSinceSeen returns nil (not 0) when the device's bridge reports
		// no last_seen at all, so a script can tell "unknown" from "just now".
		"secondsSinceSeen": func(L *lua.LState) int {
			id := L.CheckString(1)
			t, err := api.GetLastSeen(id)
			if err != nil {
				fail(L, err, "home.secondsSinceSeen(%q)", id)
				return 0
			}
			if t.IsZero() {
				L.Push(lua.LNil)
				return 1
			}
			L.Push(lua.LNumber(time.Since(t).Seconds()))
			return 1
		},
		"isReachable": func(L *lua.LState) int {
			id := L.CheckString(1)
			ok, err := api.IsReachable(id)
			if err != nil {
				fail(L, err, "home.isReachable(%q)", id)
				return 0
			}
			L.Push(lua.LBool(ok))
			return 1
		},
		"getHouseState": func(L *lua.LState) int {
			key := L.CheckString(1)
			v, err := api.GetHouseState(key)
			if err != nil {
				fail(L, err, "home.getHouseState(%q)", key)
				return 0
			}
			L.Push(goToLua(L, v))
			return 1
		},
		"setHouseState": func(L *lua.LState) int {
			key := L.CheckString(1)
			v := luaToGo(L.CheckAny(2))
			if err := api.SetHouseState(key, v); err != nil {
				fail(L, err, "home.setHouseState(%q)", key)
			}
			return 0
		},
		"getLastKnown": func(L *lua.LState) int {
			id := L.CheckString(1)
			v, err := api.GetLastKnown(id)
			if err != nil {
				fail(L, err, "home.getLastKnown(%q)", id)
				return 0
			}
			L.Push(goToLua(L, v))
			return 1
		},
		"findDevices": func(L *lua.LState) int {
			kind := L.CheckString(1)
			ids := devicesOfKind(kind)
			tbl := L.NewTable()
			for _, id := range ids {
				tbl.Append(lua.LString(id))
			}
			L.Push(tbl)
			return 1
		},
		"notify": func(L *lua.LState) int {
			event := L.CheckString(1)
			payload := map[string]any{}
			if L.GetTop() >= 2 {
				tbl := L.OptTable(2, L.NewTable())
				tbl.ForEach(func(k, v lua.LValue) {
					payload[k.String()] = luaToGo(v)
				})
			}
			if err := api.Notify(event, payload); err != nil {
				fail(L, err, "home.notify(%q)", event)
			}
			return 0
		},
	})
}

// goToLua converts a Go value returned from HomeAPI into a Lua value. A
// type it has no representation for (a func, chan, or anything else
// reflection can't turn into a Lua-shaped value) raises a script-catchable
// binding error instead of silently stringifying it, so a caller can't
// mistake a mangled value for the real one.
func goToLua(L *lua.LState, v any) lua.LValue {
	switch t := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(t)
	case string:
		return lua.LString(t)
	case []byte:
		return lua.LString(string(t))
	case time.Time:
		return lua.LString(t.Format(time.RFC3339))
	case map[string]any:
		tbl := L.NewTable()
		for k, mv := range t {
			L.SetField(tbl, k, goToLua(L, mv))
		}
		return tbl
	case []any:
		tbl := L.NewTable()
		for _, ev := range t {
			tbl.Append(goToLua(L, ev))
		}
		return tbl
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return lua.LNumber(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return lua.LNumber(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return lua.LNumber(rv.Float())
	case reflect.Slice, reflect.Array:
		tbl := L.NewTable()
		for i := 0; i < rv.Len(); i++ {
			tbl.Append(goToLua(L, rv.Index(i).Interface()))
		}
		return tbl
	case reflect.Map:
		tbl := L.NewTable()
		iter := rv.MapRange()
		for iter.Next() {
			L.SetField(tbl, fmt.Sprintf("%v", iter.Key().Interface()), goToLua(L, iter.Value().Interface()))
		}
		return tbl
	}

	raiseBindingError(L, fmt.Errorf("policy: cannot convert Go value of type %T to Lua", v))
	return lua.LNil // unreachable: raiseBindingError panics
}

// luaToGo converts a Lua value passed from a script into a plain Go value
// suitable for HomeAPI and ExecutionLog payloads.
func luaToGo(lv lua.LValue) any {
	switch t := lv.(type) {
	case lua.LBool:
		return bool(t)
	case lua.LNumber:
		return float64(t)
	case lua.LString:
		return string(t)
	case *lua.LTable:
		return luaTableToGo(t)
	case *lua.LNilType:
		return nil
	default:
		return lv.String()
	}
}

// luaTableToGo converts a Lua table to a []any if it's a clean 1..n
// sequence (no gaps, no non-integer keys), and to a map[string]any
// otherwise. Without this distinction, a script's {1,2,3} array would
// silently become the Go map {"1":1,"2":2,"3":3} instead of []any{1,2,3}.
func luaTableToGo(t *lua.LTable) any {
	n := t.Len()
	if n > 0 {
		count := 0
		t.ForEach(func(lua.LValue, lua.LValue) { count++ })
		if count == n {
			arr := make([]any, n)
			for i := 1; i <= n; i++ {
				arr[i-1] = luaToGo(t.RawGetInt(i))
			}
			return arr
		}
	}

	m := map[string]any{}
	t.ForEach(func(k, v lua.LValue) {
		m[k.String()] = luaToGo(v)
	})
	return m
}

// registerTriggerTable installs the "trigger" global describing what caused
// this script run: trigger.device_ids is a (possibly empty) list of the
// device ids that newly satisfied the policy's condition, and
// trigger.device_id is the first of them (nil if none). Both are empty for a
// condition that doesn't implement TriggerContextProvider, e.g. a schedule.
func registerTriggerTable(L *lua.LState, deviceIDs []string) {
	t := L.NewTable()
	ids := L.NewTable()
	for _, id := range deviceIDs {
		ids.Append(lua.LString(id))
	}
	t.RawSetString("device_ids", ids)
	if len(deviceIDs) > 0 {
		t.RawSetString("device_id", lua.LString(deviceIDs[0]))
	}
	L.SetGlobal("trigger", t)
}
