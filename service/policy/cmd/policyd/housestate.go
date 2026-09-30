package main

import (
	"context"
	"fmt"

	api2 "github.com/rmrobinson/house/api"
)

// buildingLocation is the subset of Building.Config policyd needs to feed
// policy.NewLocationHomeAPI.
type buildingLocation struct {
	lat, lon float64
	tz       string
}

// fetchBuildingLocation calls HouseService.GetBuilding once and extracts
// buildingID's Config.lat/lon/tz. A building's location is effectively
// static configuration - set once at building creation, changed rarely if
// ever via UpdateBuilding - unlike device state, so a single fetch at
// startup is enough: there's no need to poll or hold a live connection open
// the way bridgehome does for device state.
func fetchBuildingLocation(ctx context.Context, client api2.HouseServiceClient, buildingID string) (buildingLocation, error) {
	b, err := client.GetBuilding(ctx, &api2.GetBuildingRequest{Id: buildingID})
	if err != nil {
		return buildingLocation{}, fmt.Errorf("policyd: fetching building %q: %w", buildingID, err)
	}

	cfg := b.GetConfig()
	return buildingLocation{lat: cfg.GetLat(), lon: cfg.GetLon(), tz: cfg.GetTz()}, nil
}
