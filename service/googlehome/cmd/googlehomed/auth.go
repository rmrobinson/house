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
// single shared-secret token configured in house-config - house is a
// single-user system, so the account-linking handshake Google's OAuth2
// flow is built for has nothing to link multiple accounts to. Google
// still requires that handshake to be wired up in the Actions console,
// and oauth.go's /oauth/authorize and /oauth/token handlers serve it -
// but every grant they issue just hands back this same fixed token, which
// never expires or varies per user.
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
