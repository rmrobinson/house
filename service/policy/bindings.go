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
type HomeAPI interface {
	// Device state
	GetLight(id string) (bool, error)
	SetLight(id string, on bool) error
	GetSensor(id string) (float64, error)
	GetAttribute(id, key string) (any, error)
	SetAttribute(id, key string, value any) error

	// House state
	GetHouseState(key string) (any, error)
	SetHouseState(key string, value any) error

	// State cache
	GetLastKnown(id string) (any, error)

	// Notifications
	Notify(event string, payload map[string]any) error
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
	L.Error(ud, 1)
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

// registerHomeTable installs the "home" global table backed by api on L.
// A fresh LState gets a fresh table: there is no shared state between
// script runs.
func registerHomeTable(L *lua.LState, api HomeAPI) {
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
		"getAttribute": func(L *lua.LState) int {
			id := L.CheckString(1)
			key := L.CheckString(2)
			v, err := api.GetAttribute(id, key)
			if err != nil {
				fail(L, err, "home.getAttribute(%q, %q)", id, key)
				return 0
			}
			L.Push(goToLua(L, v))
			return 1
		},
		"setAttribute": func(L *lua.LState) int {
			id := L.CheckString(1)
			key := L.CheckString(2)
			v := luaToGo(L.CheckAny(3))
			if err := api.SetAttribute(id, key, v); err != nil {
				fail(L, err, "home.setAttribute(%q, %q)", id, key)
			}
			return 0
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
