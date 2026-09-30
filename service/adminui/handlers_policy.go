package main

import (
	"net/http"
	"net/url"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/houseview"
)

// policiesPageData/policyDetailPageData/editorData/logsPageData mirror
// service/policy/http.go's page data shapes, ported to PolicyServiceClient
// as the source of truth instead of an in-process *policy.Engine.

type policiesPageData struct {
	Rows []policyView
}

func (s *Server) handlePoliciesList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.listPolicies(r.Context())
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "policies", policiesPageData{Rows: rows})
}

type logsPageData struct {
	PolicyID string
	Logs     []executionLogView
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	policyID := r.URL.Query().Get("policy")
	logs, err := s.listLogs(r.Context(), policyID)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "logs", logsPageData{PolicyID: policyID, Logs: logs})
}

type policyDetailPageData struct {
	Info policyView
	Logs []executionLogView
}

// writePolicyFetchError writes the appropriate response for err, as returned
// by PolicyService.GetPolicy: a plain 404 (a policy simply not existing is
// an ordinary, expected outcome of a lookup by ID, not a request failure
// worth httpError's error-level log) for NotFound, or httpError's generic
// handling for anything else. Shared by every handler that fetches a single
// policy by ID up front.
func (s *Server) writePolicyFetchError(w http.ResponseWriter, r *http.Request, err error) {
	if status.Code(err) == codes.NotFound {
		http.Error(w, "policy not found", http.StatusNotFound)
		return
	}
	s.httpError(w, r, err)
}

func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	// GetPolicy and listLogs are independent RPCs (logs are keyed by id from
	// the URL, not by anything GetPolicy returns) - run concurrently rather
	// than paying RTT(GetPolicy) + RTT(listLogs) for every page load.
	var wg sync.WaitGroup
	var p *api2.Policy
	var policyErr error
	var logs []executionLogView
	var logsErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		p, policyErr = s.policy.GetPolicy(ctx, &api2.GetPolicyRequest{Id: id})
	}()
	go func() {
		defer wg.Done()
		logs, logsErr = s.listLogs(ctx, id)
	}()
	wg.Wait()

	if policyErr != nil {
		s.writePolicyFetchError(w, r, policyErr)
		return
	}
	if logsErr != nil {
		s.httpError(w, r, logsErr)
		return
	}

	s.renderPage(w, "policy_detail", policyDetailPageData{Info: policyToView(p), Logs: logs})
}

func (s *Server) handlePolicySimulate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	resp, err := s.policy.SimulatePolicy(r.Context(), &api2.SimulatePolicyRequest{Id: id})
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	data := struct {
		Result     bool
		Registered bool
	}{Result: resp.GetResult(), Registered: resp.GetRegistered()}
	s.renderFragment(w, "policy_simulate", data)
}

func (s *Server) handlePolicyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.policy.DeletePolicy(r.Context(), &api2.DeletePolicyRequest{Id: id}); err != nil {
		// Same reasoning as service/policy/http.go's original handler: a
		// plain http.Error would go unseen (htmx's default responseHandling
		// doesn't swap 4xx/5xx bodies into the target), so this renders a
		// minimal replacement fragment instead. NotFound gets its own copy
		// (below, via Gone) rather than DeletePolicy's raw message - it
		// means "id was already gone" (e.g. a double-submit), not that
		// removal was attempted and failed, and houseview.Message(err) here would
		// otherwise read as "removing it failed: policy not registered",
		// implying the opposite of what happened.
		gone := status.Code(err) == codes.NotFound
		msg := houseview.Message(err)
		s.respond(w, "policy_delete_failed", struct {
			ID   string
			Err  string
			Gone bool
		}{ID: id, Err: msg, Gone: gone}, msg, true)
		return
	}
	redirectAfterDelete(w, "/policies")
}

type policyEditorData struct {
	ID                string
	ConditionExprJSON string
	Script            string
	OnConditionFalse  api2.OnConditionFalse
	IsNew             bool
	TypeNames         []string
}

const exampleConditionExprJSON = `{
  "type": "use",
  "name": "",
  "params": {}
}`

func (s *Server) handlePolicyEditorNew(w http.ResponseWriter, r *http.Request) {
	typeNames, err := s.listConditionTypes(r.Context())
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "policy_editor", policyEditorData{
		IsNew:             true,
		ConditionExprJSON: exampleConditionExprJSON,
		TypeNames:         typeNames,
	})
}

func (s *Server) handlePolicyEditorEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	// GetPolicy and listConditionTypes are independent RPCs - run
	// concurrently rather than paying RTT(GetPolicy) + RTT(listConditionTypes)
	// for every "edit policy" page load.
	var wg sync.WaitGroup
	var p *api2.Policy
	var policyErr error
	var typeNames []string
	var typesErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		p, policyErr = s.policy.GetPolicy(ctx, &api2.GetPolicyRequest{Id: id})
	}()
	go func() {
		defer wg.Done()
		typeNames, typesErr = s.listConditionTypes(ctx)
	}()
	wg.Wait()

	if policyErr != nil {
		s.writePolicyFetchError(w, r, policyErr)
		return
	}
	if typesErr != nil {
		s.httpError(w, r, typesErr)
		return
	}

	s.renderPage(w, "policy_editor", policyEditorData{
		ID:                p.GetId(),
		ConditionExprJSON: conditionPrettyFunc(p.GetConditionExprJson()),
		Script:            p.GetScript(),
		OnConditionFalse:  p.GetOnConditionFalse(),
		TypeNames:         typeNames,
	})
}

func (s *Server) handlePolicySubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		typeNames, _ := s.listConditionTypes(ctx)
		s.respond(w, "policy_editor", policyEditorData{IsNew: true, TypeNames: typeNames}, "invalid form: "+err.Error(), true)
		return
	}

	onConditionFalse := api2.OnConditionFalse_INTERRUPT
	if r.FormValue("on_condition_false") == "complete" {
		onConditionFalse = api2.OnConditionFalse_COMPLETE
	}

	data := policyEditorData{
		ID:                strings.TrimSpace(r.FormValue("id")),
		ConditionExprJSON: r.FormValue("condition_expr"),
		Script:            r.FormValue("script"),
		OnConditionFalse:  onConditionFalse,
		IsNew:             r.FormValue("is_new") == "true",
	}

	if data.ID == "" {
		// Only fetched on a branch that actually re-renders policy_editor -
		// the common successful-save path below never reads TypeNames, so it
		// shouldn't pay for the RPC.
		data.TypeNames, _ = s.listConditionTypes(ctx)
		s.respond(w, "policy_editor", data, "policy ID is required", true)
		return
	}

	_, err := s.policy.SavePolicy(ctx, &api2.SavePolicyRequest{
		Id:                data.ID,
		ConditionExprJson: data.ConditionExprJSON,
		Script:            data.Script,
		OnConditionFalse:  data.OnConditionFalse,
	})
	if err != nil {
		data.TypeNames, _ = s.listConditionTypes(ctx)
		s.respond(w, "policy_editor", data, houseview.Message(err), true)
		return
	}

	// Nothing sensible to re-render on this response: navigate the browser
	// to the policy the caller just created or edited, via the same
	// mechanism redirectAfterDelete uses for the same "old page no longer
	// applies" situation.
	redirectAfterDelete(w, "/policies/"+url.PathEscape(data.ID))
}
