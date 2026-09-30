package main

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/service/lib/grpcutil"
)

// fakeHouseServer answers GetBuilding with a fixed response - the only RPC
// fetchBuildingLocation calls.
type fakeHouseServer struct {
	api2.UnimplementedHouseServiceServer

	building *api2.Building
	err      error
}

func (s *fakeHouseServer) GetBuilding(ctx context.Context, req *api2.GetBuildingRequest) (*api2.Building, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.building, nil
}

func startFakeHouseServer(t *testing.T, s api2.HouseServiceServer) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	api2.RegisterHouseServiceServer(grpcServer, s)

	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	return lis.Addr().String()
}

func TestFetchBuildingLocation(t *testing.T) {
	srv := &fakeHouseServer{building: &api2.Building{
		Id: "b1",
		Config: &api2.Building_Config{
			Name: "Home",
			Tz:   "America/Toronto",
			Lat:  43.7,
			Lon:  -79.4,
		},
	}}
	addr := startFakeHouseServer(t, srv)

	conn, err := grpcutil.DialInsecure(addr)
	require.NoError(t, err)
	defer conn.Close()
	client := api2.NewHouseServiceClient(conn)

	loc, err := fetchBuildingLocation(context.Background(), client, "b1")
	require.NoError(t, err)
	assert.Equal(t, 43.7, loc.lat)
	assert.Equal(t, -79.4, loc.lon)
	assert.Equal(t, "America/Toronto", loc.tz)
}

func TestFetchBuildingLocation_PropagatesError(t *testing.T) {
	srv := &fakeHouseServer{err: status.Error(codes.NotFound, "simulated: no such building")}
	addr := startFakeHouseServer(t, srv)

	conn, err := grpcutil.DialInsecure(addr)
	require.NoError(t, err)
	defer conn.Close()
	client := api2.NewHouseServiceClient(conn)

	_, err = fetchBuildingLocation(context.Background(), client, "no-such-building")
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}
