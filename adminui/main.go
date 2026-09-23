// Command adminui serves a server-rendered admin UI (html/template + htmx,
// no JS framework, no grpc-web) over HouseService (building/floor/room
// topology, device-to-room linking) and the BridgeService facade (device
// state, live updates). See README.md for the page/route map.
package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/grpcutil"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	viper.SetConfigName("adminui")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/house")
	viper.AddConfigPath("$HOME/.config/house")
	viper.AddConfigPath(".")

	viper.SetDefault("adminui.listen_port", 8080)

	if err := viper.ReadInConfig(); err != nil {
		logger.Fatal("unable to read config", zap.Error(err))
	}

	houseAddr := viper.GetString("adminui.house_addr")
	if len(houseAddr) < 1 {
		logger.Fatal("adminui.house_addr is required: address of the housed HouseService")
	}

	// bridge_facade_addr defaults to house_addr: the common deployment is
	// housed embedding the BridgeService facade on its own listener (see
	// service/house/cmd/housed), so the same address serves both services.
	// Set this explicitly only when the facade runs as its own process.
	bridgeFacadeAddr := viper.GetString("adminui.bridge_facade_addr")
	if len(bridgeFacadeAddr) < 1 {
		bridgeFacadeAddr = houseAddr
	}

	houseConn, err := grpcutil.DialInsecure(houseAddr)
	if err != nil {
		logger.Fatal("unable to dial house service", zap.String("address", houseAddr), zap.Error(err))
	}
	houseClient := api2.NewHouseServiceClient(houseConn)

	bridgeConn, err := grpcutil.DialInsecure(bridgeFacadeAddr)
	if err != nil {
		logger.Fatal("unable to dial bridge facade", zap.String("address", bridgeFacadeAddr), zap.Error(err))
	}
	bridgeClient := api2.NewBridgeServiceClient(bridgeConn)

	srv := newServer(context.Background(), logger, houseClient, bridgeClient)

	port := viper.GetInt("adminui.listen_port")
	logger.Info("serving admin ui", zap.Int("port", port))
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), srv.mux); err != nil {
		logger.Fatal("http server stopped", zap.Error(err))
	}
}
