package houseview

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHTTPStatus(t *testing.T) {
	for code, want := range map[codes.Code]int{
		codes.NotFound:           http.StatusNotFound,
		codes.InvalidArgument:    http.StatusBadRequest,
		codes.FailedPrecondition: http.StatusConflict,
		codes.PermissionDenied:   http.StatusForbidden,
		codes.ResourceExhausted:  http.StatusTooManyRequests,
		codes.Internal:           http.StatusInternalServerError,
	} {
		assert.Equal(t, want, HTTPStatus(status.Error(code, "x")), code.String())
	}
	assert.Equal(t, http.StatusInternalServerError, HTTPStatus(errors.New("plain")))
}
