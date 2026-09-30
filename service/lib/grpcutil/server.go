package grpcutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ServerTLSConfig names the on-disk cert/key/CA files a gRPC server should
// use for mutual TLS. The cert/key are re-read from disk on every TLS
// handshake rather than cached at startup, so an external process (e.g. a
// step-ca renewal sidecar) can rotate them in place without a server
// restart - this matters because step-ca leaf certs are short-lived.
type ServerTLSConfig struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string
}

// ServerTLS builds a grpc.ServerOption enabling mutual TLS from cfg: the
// server presents CertFile/KeyFile and requires callers to present a
// certificate signed by ClientCAFile. Callers that want to stay on
// plaintext gRPC simply don't call this - a ServerTLSConfig is only
// constructed when a cert file is actually configured.
func ServerTLS(cfg ServerTLSConfig) (grpc.ServerOption, error) {
	clientCAs, err := loadCAPool(cfg.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("loading client CA pool: %w", err)
	}

	tlsConfig := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("loading server cert/key: %w", err)
			}
			return &cert, nil
		},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
	}

	return grpc.Creds(credentials.NewTLS(tlsConfig)), nil
}

func loadCAPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", caFile, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return pool, nil
}
