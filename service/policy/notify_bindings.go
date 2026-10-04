package policy

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// luaStringField returns tbl's field as a string, or "" if it's unset or
// not a string - used for notify.send's optional subject/body/content_type
// fields, where "absent" and "empty string" should behave the same rather
// than raising a type error.
func luaStringField(L *lua.LState, tbl *lua.LTable, field string) string {
	s, ok := L.GetField(tbl, field).(lua.LString)
	if !ok {
		return ""
	}
	return string(s)
}

// registerNotifyTable installs the "notify" global table backed by n - the
// Lua-facing counterpart to registerHomeTable, deliberately a separate
// global rather than part of "home": NotifyAPI is a different concern
// (reaching a person) from HomeAPI (reading/writing house state). A fresh
// LState gets a fresh table: there is no shared state between script runs.
//
// n may be nil - no notification backend configured, the same story as any
// HomeAPI method a concrete adapter doesn't back yet - in which case
// notify.send raises an ErrNotImplemented-flavoured binding error instead
// of nil-pointer-panicking.
func registerNotifyTable(L *lua.LState, n NotifyAPI) {
	L.RegisterModule("notify", map[string]lua.LGFunction{
		"send": func(L *lua.LState) int {
			if n == nil {
				raiseBindingError(L, fmt.Errorf("notify.send: %w", ErrNotImplemented))
				return 0
			}

			tbl := L.CheckTable(1)

			toTbl, ok := L.GetField(tbl, "to").(*lua.LTable)
			if !ok {
				raiseBindingError(L, fmt.Errorf(`notify.send: "to" is required and must be a table of recipient IDs`))
				return 0
			}
			var recipientIDs []string
			toTbl.ForEach(func(_, v lua.LValue) {
				recipientIDs = append(recipientIDs, v.String())
			})

			subject := luaStringField(L, tbl, "subject")
			body := luaStringField(L, tbl, "body")
			contentType := luaStringField(L, tbl, "content_type")

			if err := n.Send(recipientIDs, subject, body, contentType); err != nil {
				raiseBindingError(L, fmt.Errorf("notify.send: %w", err))
			}
			return 0
		},
	})
}
