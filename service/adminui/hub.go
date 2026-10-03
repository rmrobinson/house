package main

import (
	"context"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/hub"
)

// deviceHub is a hub fanning out BridgeService.StreamUpdates, so a
// device_info cell a page already rendered from its own GetDevice/
// ListDevices call can be patched live - see handleSSE.
type deviceHub = hub.Hub[*api2.Update]

func newDeviceHub(logger *zap.Logger, bridge api2.BridgeServiceClient) *deviceHub {
	return hub.New(logger, func(ctx context.Context) (func() (*api2.Update, error), error) {
		stream, err := bridge.StreamUpdates(ctx, &api2.StreamUpdatesRequest{})
		if err != nil {
			return nil, err
		}
		return stream.Recv, nil
	})
}
