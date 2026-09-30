package main

import (
	"context"

	"go.uber.org/zap"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/hub"
)

// policyHub is a hub fanning out PolicyService.StreamEvents - the
// policy-engine analogue of deviceHub (hub.go), same "one upstream stream
// backs every open browser tab" reasoning.
type policyHub = hub.Hub[*api2.PolicyEvent]

func newPolicyHub(logger *zap.Logger, policy api2.PolicyServiceClient) *policyHub {
	return hub.New(logger, func(ctx context.Context) (func() (*api2.PolicyEvent, error), error) {
		stream, err := policy.StreamEvents(ctx, &api2.StreamEventsRequest{})
		if err != nil {
			return nil, err
		}
		return stream.Recv, nil
	})
}
