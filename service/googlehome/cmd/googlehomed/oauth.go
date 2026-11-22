// oauth.go implements the thin OAuth2 account-linking front door Google's
// Smart Home Action framework requires - the authorization_code/
// refresh_token grants at the "Authorization URL"/"Token URL" the Actions
// console's account-linking section asks for (confirmed against
// developers.home.google.com/cloud-to-cloud/project/authorization this
// session). See auth.go's staticTokenValidator doc comment for why a
// single-user deployment doesn't need a real OAuth2 server behind this:
// every grant issued here just hands back the one static
// googlehome.auth_token value staticTokenValidator already checks on
// every fulfillment call - never a freshly minted one.
//
// What this DOES provide for real is the client_id/client_secret check
// Google's backend presents on every /oauth/token call (per the Google
// doc: "By default, Google sends the credentials in the request body") -
// that's the actual security boundary this answers "only Google can
// auth" with: only whoever configured the matching client_secret in the
// Actions console (this deployment's own owner, when they set up account
// linking) can ever complete the handshake and receive the token - same
// enforcement a full OAuth2 server would provide, just without any of
// the multi-user/per-session-token machinery a real one needs, since
// there's exactly one user and one token here.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// authCodeTTL bounds how long a code from /oauth/authorize stays
// redeemable - standard OAuth2 practice is a short window, since Google's
// backend is expected to exchange it at /oauth/token immediately.
const authCodeTTL = 5 * time.Minute

// oauthConfig holds the account-linking client credentials - configured
// here AND pasted into the Actions console's "OAuth Client Information"
// section, so both sides agree.
type oauthConfig struct {
	clientID     string
	clientSecret string
	// redirectURI must exactly match what Google's backend sends
	// (https://oauth-redirect[-sandbox].googleusercontent.com/r/<project-id>)
	// - it's the one thing the authorize handler checks before it's safe
	// to redirect anywhere at all (see newOAuthAuthorizeHandler).
	redirectURI string
	// token is the single static value every successful grant hands
	// back - the same value auth.go's staticTokenValidator checks.
	token string
}

// authCode is one single-use code minted by /oauth/authorize, meant to be
// redeemed by exactly one /oauth/token call.
type authCode struct {
	clientID    string
	redirectURI string
	expiry      time.Time
}

// authCodeStore is the only server-side state this needs: still-redeemable
// codes. A code is deleted the instant it's redeemed (or found expired) -
// never reusable, so a replayed code always fails on its second use.
type authCodeStore struct {
	mu    sync.Mutex
	codes map[string]authCode
}

func newAuthCodeStore() *authCodeStore {
	return &authCodeStore{codes: map[string]authCode{}}
}

func (s *authCodeStore) mint(clientID, redirectURI string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for code, c := range s.codes {
		if now.After(c.expiry) {
			delete(s.codes, code)
		}
	}

	code := uuid.New().String()
	s.codes[code] = authCode{clientID: clientID, redirectURI: redirectURI, expiry: now.Add(authCodeTTL)}
	return code
}

// redeem deletes and returns code's record if it exists and hasn't
// expired.
func (s *authCodeStore) redeem(code string) (authCode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.codes[code]
	if !ok {
		return authCode{}, false
	}
	delete(s.codes, code)
	if time.Now().After(c.expiry) {
		return authCode{}, false
	}
	return c, true
}

// newOAuthAuthorizeHandler serves the "Authorization URL" Google's
// account-linking flow redirects the user's browser to
// (developers.home.google.com/cloud-to-cloud/project/authorization:
// client_id, redirect_uri, state, response_type required; scope
// optional, ignored here - this deployment has nothing to scope). There's
// no real login/consent screen (single-user system) - a valid request is
// approved immediately.
//
// redirect_uri is checked FIRST and exactly (no prefix match): if it
// doesn't match the configured value, this returns a direct error page
// rather than ever redirecting anywhere - redirecting to an
// attacker-supplied redirect_uri would hand them a real authorization
// code, an open-redirect-shaped risk, not just a correctness check. Every
// other validation failure (bad client_id, unsupported response_type) is
// reported back via redirect with an "error" param instead, same as a
// real OAuth2 server would, since redirect_uri is already confirmed good
// by that point.
func newOAuthAuthorizeHandler(logger *zap.Logger, cfg oauthConfig, codes *authCodeStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirectURI := q.Get("redirect_uri")
		state := q.Get("state")

		if redirectURI != cfg.redirectURI {
			logger.Info("oauth authorize: redirect_uri mismatch", zap.String("got", redirectURI))
			http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
			return
		}

		if subtle.ConstantTimeCompare([]byte(q.Get("client_id")), []byte(cfg.clientID)) != 1 {
			redirectWithOAuthError(w, r, redirectURI, state, "unauthorized_client")
			return
		}
		if q.Get("response_type") != "code" {
			redirectWithOAuthError(w, r, redirectURI, state, "unsupported_response_type")
			return
		}

		code := codes.mint(cfg.clientID, redirectURI)
		redirectWithOAuthResult(w, r, redirectURI, map[string]string{"code": code, "state": state})
	}
}

func redirectWithOAuthError(w http.ResponseWriter, r *http.Request, redirectURI, state, errCode string) {
	params := map[string]string{"error": errCode}
	if state != "" {
		params["state"] = state
	}
	redirectWithOAuthResult(w, r, redirectURI, params)
}

func redirectWithOAuthResult(w http.ResponseWriter, r *http.Request, redirectURI string, params map[string]string) {
	dest, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusInternalServerError)
		return
	}
	query := dest.Query()
	for k, v := range params {
		if v != "" {
			query.Set(k, v)
		}
	}
	dest.RawQuery = query.Encode()

	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// oauthTokenResponse matches the exact shape Google's docs specify
// (developers.home.google.com/cloud-to-cloud/project/authorization) -
// "Bearer" capitalized, refresh_token optional on a refresh_token grant
// (always included here anyway, since it's the same static value either
// way - see this file's package doc comment on why that's not a new
// exposure).
type oauthTokenResponse struct {
	TokenType    string `json:"token_type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
}

// newOAuthTokenHandler serves the "Token URL" Google's backend calls
// server-to-server - both to exchange an /oauth/authorize code for a
// token, and later to refresh one. Google's default is to send
// client_id/client_secret in the request body (confirmed against the
// live docs this session - HTTP Basic auth is an opt-in Actions-console
// setting, not the default), so form values are checked first; Basic
// auth is still accepted as a fallback in case that setting is ever
// turned on. client_id/client_secret match is the real security boundary
// here - see this file's package doc comment.
func newOAuthTokenHandler(logger *zap.Logger, cfg oauthConfig, codes *authCodeStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request")
			return
		}

		clientID := r.PostFormValue("client_id")
		clientSecret := r.PostFormValue("client_secret")
		if clientID == "" && clientSecret == "" {
			if basicID, basicSecret, ok := r.BasicAuth(); ok {
				clientID, clientSecret = basicID, basicSecret
			}
		}
		if subtle.ConstantTimeCompare([]byte(clientID), []byte(cfg.clientID)) != 1 ||
			subtle.ConstantTimeCompare([]byte(clientSecret), []byte(cfg.clientSecret)) != 1 {
			logger.Info("oauth token: client authentication failed")
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client")
			return
		}

		switch r.PostFormValue("grant_type") {
		case "authorization_code":
			c, ok := codes.redeem(r.PostFormValue("code"))
			if !ok || c.clientID != clientID || c.redirectURI != r.PostFormValue("redirect_uri") {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant")
				return
			}
		case "refresh_token":
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("refresh_token")), []byte(cfg.token)) != 1 {
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant")
				return
			}
		default:
			writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(oauthTokenResponse{
			TokenType:    "Bearer",
			AccessToken:  cfg.token,
			RefreshToken: cfg.token,
			ExpiresIn:    3600,
		})
	}
}

func writeOAuthError(w http.ResponseWriter, status int, errCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": errCode})
}
