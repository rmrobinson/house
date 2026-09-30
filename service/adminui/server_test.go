package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

// TestRequirePolicyRendersUnavailablePage guards against the nil-interface
// panic requirePolicy exists to prevent: a nil PolicyServiceClient has no
// concrete type to dispatch a method call through, so calling one of the
// wrapped handlers directly would panic per-request instead of failing
// gracefully.
func TestRequirePolicyRendersUnavailablePage(t *testing.T) {
	s := &Server{logger: zaptest.NewLogger(t)} // policy left nil

	called := false
	h := requirePolicy(func(s *Server, w http.ResponseWriter, r *http.Request) { called = true })

	rec := httptest.NewRecorder()
	h(s, rec, httptest.NewRequest("GET", "/policies", nil))

	assert.False(t, called, "wrapped handler must not run when policy is unconfigured")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "policy_addr")
}

// TestNavGreysPoliciesWhenNotConfigured checks navFuncs' policyAvailable
// (templates.go), which every page's shared layout.html nav reads to decide
// whether to dim the Policies/Execution Log links - exercised here through
// an unrelated page (buildings) since the behavior is layout-wide, not
// specific to the policy pages themselves.
func TestNavGreysPoliciesWhenNotConfigured(t *testing.T) {
	old := policyConfigured.Load()
	t.Cleanup(func() { policyConfigured.Store(old) })

	s := &Server{logger: zaptest.NewLogger(t)}

	// The layout's <style> block always contains the literal "nav-disabled"
	// (it's a CSS rule, present either way) - so assert on the class actually
	// landing on the <a> element, not on the substring's mere presence.
	policyConfigured.Store(false)
	rec := httptest.NewRecorder()
	s.renderPage(rec, "buildings", buildingsPageData{})
	assert.Contains(t, rec.Body.String(), `href="/policies" class="nav-disabled"`)

	policyConfigured.Store(true)
	rec = httptest.NewRecorder()
	s.renderPage(rec, "buildings", buildingsPageData{})
	assert.NotContains(t, rec.Body.String(), `href="/policies" class="nav-disabled"`)
}
