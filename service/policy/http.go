package policy

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/lib/htmxutil"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

func formatTimeFunc(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func durationFunc(l ExecutionLog) string {
	if l.EndedAt.IsZero() {
		return "running"
	}
	return l.EndedAt.Sub(l.StartedAt).Round(time.Millisecond).String()
}

func conditionCompactFunc(expr ConditionExpr) string {
	data, err := MarshalConditionExpr(expr)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return string(data)
}

func conditionPrettyFunc(expr ConditionExpr) string {
	data, err := MarshalConditionExpr(expr)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return string(data)
	}
	return buf.String()
}

func onConditionFalseStrFunc(v OnConditionFalse) string {
	if v == Interrupt {
		return "interrupt"
	}
	return "complete"
}

var templateFuncs = template.FuncMap{
	"formatTime":          formatTimeFunc,
	"duration":            durationFunc,
	"conditionCompact":    conditionCompactFunc,
	"conditionPretty":     conditionPrettyFunc,
	"onConditionFalseStr": onConditionFalseStrFunc,
}

// Server is an HTTP UI over an Engine, structured to match this repo's
// other server-rendered admin surface (adminui: html/template + htmx, no
// JS framework) — a shared #flash banner written as an out-of-band swap on
// every mutating action, one global SSE connection opened once on <body>
// that live-patches individual elements by ID rather than re-rendering
// whole pages, and htmx-driven forms (hx-post/hx-target/hx-swap/hx-confirm)
// instead of plain POST-and-redirect. JS assets (htmx, its SSE extension,
// CodeMirror) are vendored under static/, not pulled from a CDN.
//
// Server has no state of its own: every response is built fresh from
// Engine's and ConditionRegistry's existing public API.
//
// The condition-expression editor is a plain JSON textarea using exactly
// the wire format MarshalConditionExpr/UnmarshalConditionExpr already
// round-trip through the Store — not a dynamic per-condition-type form
// builder with a nested And/Or/Not tree UI. That's a deliberate scope cut:
// the available condition type names are still listed for reference, but
// composing the tree itself is done as JSON text.
//
// Likewise, policy simulation shows the condition tree's combined
// Evaluate() result, not a per-leaf breakdown — the live Condition tree
// built by ConditionExpr.build doesn't expose its internal structure (e.g.
// compositeCondition.children is unexported), so a per-leaf view would
// need new plumbing through that boundary.
type Server struct {
	engine   *Engine
	registry *ConditionRegistry
	logger   *zap.Logger
	tmpl     *template.Template
	mux      *http.ServeMux
}

// NewServer creates a Server serving e's policies (built from registry) as
// an HTTP UI.
func NewServer(e *Engine, registry *ConditionRegistry, logger *zap.Logger) *Server {
	tmpl := template.Must(template.New("").Funcs(templateFuncs).ParseFS(templatesFS, "templates/*.html"))

	s := &Server{
		engine:   e,
		registry: registry,
		logger:   logger,
		tmpl:     tmpl,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /policies", s.handlePoliciesList)
	mux.HandleFunc("POST /policies", s.handlePolicySubmit)
	mux.HandleFunc("GET /policies/new", s.handlePolicyEditorNew)
	mux.HandleFunc("GET /policies/{id}", s.handlePolicyDetail)
	mux.HandleFunc("GET /policies/{id}/edit", s.handlePolicyEditorEdit)
	mux.HandleFunc("POST /policies/{id}/delete", s.handlePolicyDelete)
	mux.HandleFunc("GET /policies/{id}/simulate", s.handlePolicySimulate)
	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("GET /events", s.handleEvents)
	s.mux = mux

	return s
}

// Handler returns the Server's routes as an http.Handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// renderPage renders name's full document (layout+content) — top-level
// navigation between pages is plain <a href> (not htmx-boosted); only
// in-page mutating actions go through htmx and use respond instead.
func (s *Server) renderPage(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template render failed", zap.String("template", name), zap.Error(err))
	}
}

// respond re-renders name's content fragment for an htmx action response
// (never a full document — actions are only ever triggered by htmx), plus
// an out-of-band swap of the #flash banner. flash may be empty to clear
// the banner without showing a new message.
func (s *Server) respond(w http.ResponseWriter, name string, data any, flash string, isError bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template render failed", zap.String("template", name), zap.Error(err))
	}
	htmxutil.WriteFlash(w, flash, isError)
}

// renderFragment renders a standalone fragment with no flash OOB swap —
// used for a plain GET response (the simulate poll), as opposed to
// respond, which is for htmx action responses.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template render failed", zap.String("template", name), zap.Error(err))
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/policies", http.StatusFound)
}

// policyRow is a policy's live snapshot reshaped for templates — used for
// list rows, the detail page's own state display, and both's shared
// policy_status_cell fragment.
type policyRow struct {
	ID               string
	ConditionExpr    ConditionExpr
	Script           string
	OnConditionFalse OnConditionFalse
	Active           bool
	LastLog          *ExecutionLog
}

// policyRowWithLastLog builds a policyRow from info, using last as-is for
// LastLog instead of querying the engine for it - for a caller that already
// has the exact log in hand (a LastLogs() lookup, or a uiLogAppendedTopic
// event's own payload) and so has no reason to pay for re-deriving it.
func policyRowWithLastLog(info PolicyInfo, last *ExecutionLog) policyRow {
	return policyRow{
		ID:               info.ID,
		ConditionExpr:    info.ConditionExpr,
		Script:           info.Script,
		OnConditionFalse: info.OnConditionFalse,
		Active:           info.Active,
		LastLog:          last,
	}
}

func (s *Server) policyRowFor(info PolicyInfo) policyRow {
	var last *ExecutionLog
	if logs := s.engine.LogsForPolicy(info.ID); len(logs) > 0 {
		l := logs[len(logs)-1]
		last = &l
	}
	return policyRowWithLastLog(info, last)
}

// buildPolicyRows renders every policy's row for the list page. It fetches
// every policy's last log with a single LastLogs() pass over the whole log
// history, rather than policyRowFor's own per-policy LogsForPolicy scan run
// once per policy - the latter would cost O(policies x total logs) here.
func (s *Server) buildPolicyRows() []policyRow {
	infos := s.engine.Policies()
	lastLogs := s.engine.LastLogs()
	rows := make([]policyRow, 0, len(infos))
	for _, info := range infos {
		var last *ExecutionLog
		if l, ok := lastLogs[info.ID]; ok {
			last = &l
		}
		rows = append(rows, policyRowWithLastLog(info, last))
	}
	return rows
}

func (s *Server) handlePoliciesList(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "policies_page", struct{ Rows []policyRow }{Rows: s.buildPolicyRows()})
}

// maxLogRows caps how many execution log rows a single page renders,
// newest first — logs otherwise accumulate for as long as the engine runs.
const maxLogRows = 200

type logsData struct {
	PolicyID string
	Logs     []ExecutionLog
}

func (s *Server) buildLogsData(policyID string) logsData {
	var logs []ExecutionLog
	if policyID != "" {
		logs = s.engine.LogsForPolicy(policyID)
	} else {
		logs = s.engine.Logs()
	}

	reversed := make([]ExecutionLog, 0, min(len(logs), maxLogRows))
	for i := len(logs) - 1; i >= 0 && len(reversed) < maxLogRows; i-- {
		reversed = append(reversed, logs[i])
	}
	return logsData{PolicyID: policyID, Logs: reversed}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "logs_page", s.buildLogsData(r.URL.Query().Get("policy")))
}

func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, ok := s.engine.Policy(id)
	if !ok {
		http.Error(w, "policy not found", http.StatusNotFound)
		return
	}

	data := struct {
		Info policyRow
		Logs []ExecutionLog
	}{
		Info: s.policyRowFor(info),
		Logs: s.buildLogsData(id).Logs,
	}
	s.renderPage(w, "policy_detail_page", data)
}

func (s *Server) handlePolicySimulate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	result, ok := s.engine.Simulate(id)
	data := struct {
		Result     bool
		Registered bool
	}{Result: result, Registered: ok}
	s.renderFragment(w, "policy_simulate_fragment", data)
}

func (s *Server) handlePolicyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.Unregister(id); err != nil {
		// Engine.Unregister removes id from the live engine's map before
		// attempting to persist the delete, so by this point id is gone
		// from the running engine either way — there's no policy data left
		// to rebuild policy_detail_content from. A plain http.Error here
		// would go unseen: htmx's default responseHandling doesn't swap
		// 4xx/5xx bodies into the target, so the user would get no
		// indication that (in the store-failure case) id may reappear on
		// the next restart. respond() with a minimal replacement fragment
		// at least surfaces that.
		s.respond(w, "policy_delete_failed_content", struct{ ID, Err string }{ID: id, Err: err.Error()}, err.Error(), true)
		return
	}
	w.Header().Set("HX-Redirect", "/policies")
}

// editorData backs the create/edit policy form. The same template serves
// both: IsNew controls whether the ID field is editable and the submit
// button's label.
type editorData struct {
	ID                string
	ConditionExprJSON string
	Script            string
	OnConditionFalse  OnConditionFalse
	IsNew             bool
	TypeNames         []string
}

const exampleConditionExprJSON = `{
  "type": "use",
  "name": "",
  "params": {}
}`

func (s *Server) handlePolicyEditorNew(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "policy_editor_page", editorData{
		IsNew:             true,
		ConditionExprJSON: exampleConditionExprJSON,
		TypeNames:         s.registry.TypeNames(),
	})
}

func (s *Server) handlePolicyEditorEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, ok := s.engine.Policy(id)
	if !ok {
		http.Error(w, "policy not found", http.StatusNotFound)
		return
	}

	s.renderPage(w, "policy_editor_page", editorData{
		ID:                info.ID,
		ConditionExprJSON: conditionPrettyFunc(info.ConditionExpr),
		Script:            info.Script,
		OnConditionFalse:  info.OnConditionFalse,
		TypeNames:         s.registry.TypeNames(),
	})
}

func (s *Server) handlePolicySubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		// respond(), not http.Error: this request came from an hx-post
		// form, and htmx's default responseHandling doesn't swap 4xx/5xx
		// bodies into the target, so a plain http.Error would leave the
		// user staring at an unchanged page with no feedback at all. The
		// original submission's fields aren't recoverable from an
		// unparseable form, so this falls back to a blank create form
		// rather than the caller's in-progress edit.
		s.respond(w, "policy_editor_content", editorData{IsNew: true, TypeNames: s.registry.TypeNames()}, "invalid form: "+err.Error(), true)
		return
	}

	onConditionFalse := Interrupt
	if r.FormValue("on_condition_false") == "complete" {
		onConditionFalse = Complete
	}

	data := editorData{
		ID:                strings.TrimSpace(r.FormValue("id")),
		ConditionExprJSON: r.FormValue("condition_expr"),
		Script:            r.FormValue("script"),
		OnConditionFalse:  onConditionFalse,
		IsNew:             r.FormValue("is_new") == "true",
		TypeNames:         s.registry.TypeNames(),
	}

	if data.ID == "" {
		s.respond(w, "policy_editor_content", data, "policy ID is required", true)
		return
	}

	expr, err := UnmarshalConditionExpr([]byte(data.ConditionExprJSON), s.registry)
	if err != nil {
		s.respond(w, "policy_editor_content", data, fmt.Sprintf("invalid condition expression: %v", err), true)
		return
	}

	p := &Policy{
		ID:               data.ID,
		ConditionExpr:    expr,
		Script:           data.Script,
		OnConditionFalse: data.OnConditionFalse,
	}
	if err := s.engine.Register(p); err != nil {
		s.respond(w, "policy_editor_content", data, err.Error(), true)
		return
	}

	// Nothing sensible to re-render on this response: navigate the browser
	// to the policy the caller just created or edited, matching adminui's
	// redirectAfterDelete convention for the same "old page no longer
	// applies" situation.
	w.Header().Set("HX-Redirect", "/policies/"+url.PathEscape(data.ID))
}

// handleEvents relays the engine's UI-facing Bus topics as Server-Sent
// Events for htmx's SSE extension, via one connection shared by every page
// (see templates/base.html's body_open: hx-ext="sse" sse-connect="/events"
// on <body>) — mirroring adminui's single global /events stream rather
// than a separate connection per page or per widget.
//
// Every push is a targeted out-of-band patch keyed by DOM id (a policy's
// own #policy-status-<id>, or a log table's #logs-tbody[-<policyID>]), not
// a whole-page or whole-table re-render — again mirroring adminui's
// per-entity device_info OOB relay rather than re-rendering entire tables.
// Like that relay, this only updates elements already on the page: a
// policy created while the list page is open won't appear as a new row
// until the page is reloaded, since there's no existing element for it to
// patch — the same accepted gap adminui documents for a newly-discovered
// device.
//
// There's no initial catch-up push on connect: each page's own GET handler
// already rendered current state, so the stream only needs to relay
// what changes afterwards.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	policyCh := s.engine.Bus().Subscribe(uiPolicyChangedTopic)
	defer s.engine.Bus().Unsubscribe(uiPolicyChangedTopic, policyCh)

	logCh := s.engine.Bus().Subscribe(uiLogAppendedTopic)
	defer s.engine.Bus().Unsubscribe(uiLogAppendedTopic, logCh)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-policyCh:
			if !ok {
				return
			}
			if !s.pushPolicyChanged(w, ev.Payload) {
				return
			}
			_ = rc.Flush()
		case ev, ok := <-logCh:
			if !ok {
				return
			}
			if !s.pushLogAppended(w, ev.Payload) {
				return
			}
			_ = rc.Flush()
		}
	}
}

// pushPolicyChanged writes the OOB patch for a uiPolicyChangedTopic event
// (payload: the changed policy's ID) and reports whether the connection is
// still writable.
func (s *Server) pushPolicyChanged(w http.ResponseWriter, payload any) bool {
	id, ok := payload.(string)
	if !ok {
		return true
	}

	var frag bytes.Buffer
	var err error
	if info, found := s.engine.Policy(id); found {
		err = s.tmpl.ExecuteTemplate(&frag, "policy_status_cell_oob", s.policyRowFor(info))
	} else {
		err = s.tmpl.ExecuteTemplate(&frag, "policy_status_removed", id)
	}
	if err != nil {
		s.logger.Error("template render failed", zap.String("template", "policy_status_cell_oob"), zap.Error(err))
		return true
	}

	return writeSSEEvent(w, frag.String())
}

// pushLogAppended writes the OOB patches for a uiLogAppendedTopic event
// (payload: the appended ExecutionLog) and reports whether the connection
// is still writable: the affected policy's status cell (its last-run info
// changed too), plus the new row prepended into both the global log feed
// and that policy's own detail-page feed — whichever, if any, is currently
// on the page (see templates/partials.html's log_row_oob).
func (s *Server) pushLogAppended(w http.ResponseWriter, payload any) bool {
	l, ok := payload.(ExecutionLog)
	if !ok {
		return true
	}

	var frag bytes.Buffer
	if info, found := s.engine.Policy(l.PolicyID); found {
		if err := s.tmpl.ExecuteTemplate(&frag, "policy_status_cell_oob", policyRowWithLastLog(info, &l)); err != nil {
			s.logger.Error("template render failed", zap.String("template", "policy_status_cell_oob"), zap.Error(err))
		}
	}
	for _, target := range []string{"#logs-tbody", "#logs-tbody-" + l.PolicyID} {
		data := struct {
			Target string
			Log    ExecutionLog
		}{Target: target, Log: l}
		if err := s.tmpl.ExecuteTemplate(&frag, "log_row_oob", data); err != nil {
			s.logger.Error("template render failed", zap.String("template", "log_row_oob"), zap.Error(err))
		}
	}

	return writeSSEEvent(w, frag.String())
}

// writeSSEEvent writes html as a single SSE "message" event and reports
// whether the write succeeded. SSE's data field can't contain a literal
// newline, so — since html may bundle several OOB-marked elements one
// after another (pushLogAppended's case) rather than a single root
// element — newlines are stripped outright instead of split into multiple
// "data:" lines: simpler, and htmx's DOM parser doesn't need them.
func writeSSEEvent(w http.ResponseWriter, html string) bool {
	_, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", htmxutil.OneLine(html))
	return err == nil
}
