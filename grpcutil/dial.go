// Package grpcutil holds small gRPC client helpers shared across this repo's
// services and CLIs.
package grpcutil

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DialInsecure dials addr over plaintext gRPC (no TLS) - the transport every
// client in this repo currently uses. Centralizing it here means a future
// move to TLS/mTLS is a one-place change instead of updating every call site
// that repeats the WithTransportCredentials boilerplate.
func DialInsecure(addr string) (*grpc.ClientConn, error) {
	return grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}
