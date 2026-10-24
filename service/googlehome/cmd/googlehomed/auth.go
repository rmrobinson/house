package main

import (
	"context"
	"crypto/subtle"
	"errors"
)

// ErrInvalidToken is returned by staticTokenValidator.Validate when the
// supplied bearer token doesn't match the configured shared secret.
var ErrInvalidToken = errors.New("invalid token")

// staticTokenValidator implements action.AccessTokenValidator against a
// single shared-secret token configured in house-config, rather than a real
// OAuth2 account-linking server - house is a single-user system, so the
// account-linking handshake Google's OAuth2 flow is built for has nothing
// to link multiple accounts to. Google still requires the account-linking
// flow to be wired up in the Actions console (authorization/token endpoints
// returning this same fixed token), but the token itself never expires or
// varies per user.
type staticTokenValidator struct {
	token       string
	agentUserID string
}

// Validate implements action.AccessTokenValidator.
func (v *staticTokenValidator) Validate(_ context.Context, token string) (string, error) {
	if subtle.ConstantTimeCompare([]byte(token), []byte(v.token)) != 1 {
		return "", ErrInvalidToken
	}
	return v.agentUserID, nil
}
