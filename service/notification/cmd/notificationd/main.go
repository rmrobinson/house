// notificationd runs the notification service as a standalone daemon,
// exposing it over api2.NotificationServiceServer (see service/notification/
// service.go). It has no in-process callers of its own - the policy engine
// reaches it over gRPC via service/policy/notifyclient.
//
// Recipients (who can be reached, and over which channels) are loaded once
// at startup from notification.recipients in the config file - there is no
// RPC to manage them yet, see notification.example.yaml for the shape.
//
// notification.smtp_addr/notification.from configure the only channel type
// implemented so far ("email"), relayed through an unauthenticated SMTP
// host trusted by source network - see notification.SMTPSender.
//
// notification.tls.* configures mutual TLS for this process's own
// NotificationService listener, same convention as every other service in
// this repo (see service/house/cmd/housed/main.go).
package main

import (
	"bytes"
	"fmt"
	"net"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/configutil"
	"github.com/rmrobinson/house/service/lib/grpcutil"
	"github.com/rmrobinson/house/service/notification"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigType("yaml")
	viper.SetDefault("notification.listen_port", 17040)

	configPath, err := configutil.FindConfigFile("notification", "yaml", []string{"/etc/house", "$HOME/.config/house", "."})
	if err != nil {
		logger.Fatal("unable to find config", zap.Error(err))
	}
	resolved, err := configutil.ResolveSecrets(configPath)
	if err != nil {
		logger.Fatal("unable to resolve config secrets", zap.Error(err))
	}
	if err := viper.ReadConfig(bytes.NewReader(resolved)); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	smtpAddr := viper.GetString("notification.smtp_addr")
	from := viper.GetString("notification.from")
	if len(smtpAddr) < 1 || len(from) < 1 {
		logger.Fatal("notification.smtp_addr and notification.from are required")
	}

	var recipients []notification.Recipient
	if err := viper.UnmarshalKey("notification.recipients", &recipients); err != nil {
		logger.Fatal("unable to parse notification.recipients", zap.Error(err))
	}
	directory := notification.NewDirectory(recipients)

	senders := map[string]notification.Sender{
		"email": &notification.SMTPSender{Addr: smtpAddr, From: from},
	}

	var opts []grpc.ServerOption
	if certFile := viper.GetString("notification.tls.cert_file"); len(certFile) > 0 {
		keyFile := viper.GetString("notification.tls.key_file")
		caFile := viper.GetString("notification.tls.client_ca_file")
		if len(keyFile) < 1 || len(caFile) < 1 {
			logger.Fatal("notification.tls.cert_file is set; notification.tls.key_file and notification.tls.client_ca_file are required together with it")
		}

		creds, err := grpcutil.ServerTLS(grpcutil.ServerTLSConfig{CertFile: certFile, KeyFile: keyFile, ClientCAFile: caFile})
		if err != nil {
			logger.Fatal("unable to configure server TLS", zap.Error(err))
		}
		opts = append(opts, creds)
	}

	listenPort := viper.GetInt("notification.listen_port")
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		logger.Fatal("error listening", zap.Error(err), zap.Int("port", listenPort))
	}

	grpcServer := grpc.NewServer(opts...)
	api2.RegisterNotificationServiceServer(grpcServer, notification.NewService(logger, directory, senders))

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	if err := grpcServer.Serve(lis); err != nil {
		logger.Fatal("grpc server error", zap.Error(err))
	}
}
