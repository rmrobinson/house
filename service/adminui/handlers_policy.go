package main

import (
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
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

func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	p, err := s.policy.GetPolicy(ctx, &api2.GetPolicyRequest{Id: id})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			http.Error(w, "policy not found", http.StatusNotFound)
			return
		}
		s.httpError(w, r, err)
		return
	}

	logs, err := s.listLogs(ctx, id)
	if err != nil {
		s.httpError(w, r, err)
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
		// minimal replacement fragment instead.
		s.respond(w, "policy_delete_failed", struct{ ID, Err string }{ID: id, Err: grpcMessage(err)}, grpcMessage(err), true)
		return
	}
	w.Header().Set("HX-Redirect", "/policies")
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

	p, err := s.policy.GetPolicy(ctx, &api2.GetPolicyRequest{Id: id})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			http.Error(w, "policy not found", http.StatusNotFound)
			return
		}
		s.httpError(w, r, err)
		return
	}

	typeNames, err := s.listConditionTypes(ctx)
	if err != nil {
		s.httpError(w, r, err)
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
	data.TypeNames, _ = s.listConditionTypes(ctx)

	if data.ID == "" {
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
		s.respond(w, "policy_editor", data, grpcMessage(err), true)
		return
	}

	// Nothing sensible to re-render on this response: navigate the browser
	// to the policy the caller just created or edited, matching this app's
	// redirectAfterDelete convention for the same "old page no longer
	// applies" situation.
	w.Header().Set("HX-Redirect", "/policies/"+url.PathEscape(data.ID))
}
