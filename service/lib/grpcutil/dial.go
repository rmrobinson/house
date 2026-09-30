// Package grpcutil holds small gRPC client helpers shared across this repo's
// services and CLIs.
package grpcutil

import (
	"crypto/tls"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// DialInsecure dials addr over plaintext gRPC (no TLS) - the transport every
// client in this repo used before mutual TLS support was added. Centralizing
// it here means call sites that haven't been configured for TLS yet don't
// each repeat the WithTransportCredentials boilerplate.
func DialInsecure(addr string) (*grpc.ClientConn, error) {
	return grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// ClientTLSConfig names the on-disk cert/key/CA files a gRPC client should
// use to dial over mutual TLS. Like ServerTLSConfig, the cert/key are
// re-read from disk on every handshake so a renewal sidecar can rotate them
// without the dialing process restarting.
type ClientTLSConfig struct {
	CertFile string
	KeyFile  string
	CAFile   string
	// ServerName overrides the hostname DialTLS verifies the server's
	// certificate against. Leave empty for the common case - dialing a
	// server by the same name its certificate was actually issued for -
	// where grpc's default (derived from addr) is already correct. Only
	// needed when addr isn't that name, e.g. a process dialing itself over
	// "localhost" while its certificate was issued for its real hostname.
	ServerName string
}

// Dial dials addr over mutual TLS when cfg is non-nil, or plaintext gRPC
// (DialInsecure) otherwise - the dispatch every TLS-aware client in this
// repo needs, centralized so it's made consistently rather than reimplemented
// per call site.
func Dial(addr string, cfg *ClientTLSConfig) (*grpc.ClientConn, error) {
	if cfg != nil {
		return DialTLS(addr, *cfg)
	}
	return DialInsecure(addr)
}

// DialTLS dials addr over mutual TLS using cfg: it verifies the server's
// certificate against cfg.CAFile (a private CA, not a public one) and
// presents cfg.CertFile/KeyFile as this client's own identity.
func DialTLS(addr string, cfg ClientTLSConfig) (*grpc.ClientConn, error) {
	rootCAs, err := loadCAPool(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("loading root CA pool: %w", err)
	}

	tlsConfig := &tls.Config{
		RootCAs:    rootCAs,
		ServerName: cfg.ServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("loading client cert/key: %w", err)
			}
			return &cert, nil
		},
	}

	return grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
}
