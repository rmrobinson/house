package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// fakeChargerClient returns a canned ChargerState (or error) on each call to State, letting a
// test drive Refresh through a chosen sequence of poll outcomes without a real HTTP server.
type fakeChargerClient struct {
	state *ChargerState
	err   error
}

func (f *fakeChargerClient) State() (*ChargerState, error) {
	return f.state, f.err
}

func completeChargerState() *ChargerState {
	return &ChargerState{
		vitals:      &vitalAPIResponse{ContactorClosed: true},
		lifetime:    &lifetimeAPIResponse{},
		version:     &versionAPIResponse{SerialNumber: "1234ASDF", PartNumber: "1734412-02-D"},
		ipAddr:      "10.0.0.5",
		retrievedAt: time.Now(),
	}
}

func newTestChargerBridge(t *testing.T, charger chargerClient) (*ChargerBridge, *bridge.Service) {
	t.Helper()
	svc := bridge.NewService(zaptest.NewLogger(t))
	cb := &ChargerBridge{
		logger:  zaptest.NewLogger(t),
		svc:     svc,
		charger: charger,
		b:       &api2.Bridge{Id: "test-bridge"},
	}
	svc.RegisterHandler(cb, cb.b)
	return cb, svc
}

func listChargerDevices(t *testing.T, svc *bridge.Service) []*device.Device {
	t.Helper()
	resp, err := svc.API().ListDevices(context.Background(), &api2.ListDevicesRequest{})
	require.NoError(t, err)
	return resp.GetDevices()
}

func TestRefresh_Success_PublishesReachableDeviceWithAddress(t *testing.T) {
	cb, svc := newTestChargerBridge(t, &fakeChargerClient{state: completeChargerState()})

	require.NoError(t, cb.Refresh(context.Background()))

	devices := listChargerDevices(t, svc)
	require.Len(t, devices, 1)
	assert.Equal(t, "1234ASDF", devices[0].GetId())
	assert.True(t, devices[0].GetAddress().GetIsReachable())
	assert.Equal(t, "10.0.0.5:80", devices[0].GetAddress().GetAddress())
}

func TestRefresh_StateErrorBeforeAnySuccess_NoDevicePublished(t *testing.T) {
	cb, svc := newTestChargerBridge(t, &fakeChargerClient{err: errors.New("charger unreachable")})

	assert.Error(t, cb.Refresh(context.Background()))
	assert.Empty(t, listChargerDevices(t, svc), "nothing to mark unreachable if nothing was ever published")
}

func TestRefresh_StateErrorAfterPriorSuccess_RepublishesLastDeviceUnreachable(t *testing.T) {
	client := &fakeChargerClient{state: completeChargerState()}
	cb, svc := newTestChargerBridge(t, client)

	require.NoError(t, cb.Refresh(context.Background()))
	devices := listChargerDevices(t, svc)
	require.Len(t, devices, 1)
	require.True(t, devices[0].GetAddress().GetIsReachable())

	client.state = nil
	client.err = errors.New("charger unreachable")
	assert.Error(t, cb.Refresh(context.Background()))

	devices = listChargerDevices(t, svc)
	require.Len(t, devices, 1, "the device should be republished unreachable, not removed")
	assert.Equal(t, "1234ASDF", devices[0].GetId())
	assert.False(t, devices[0].GetAddress().GetIsReachable())
}

// TestRefresh_PartialChargerState_DoesNotCrash guards against the bug where Refresh passed
// ChargerState.toDevice()'s nil result (from a partial vitals/lifetime/version response)
// straight to bridge.Service.UpdateDevice, which treats a nil device as fatal and would have
// killed the whole bridge process over one incomplete poll.
func TestRefresh_PartialChargerState_DoesNotCrash(t *testing.T) {
	client := &fakeChargerClient{state: completeChargerState()}
	cb, svc := newTestChargerBridge(t, client)

	require.NoError(t, cb.Refresh(context.Background()))
	require.True(t, listChargerDevices(t, svc)[0].GetAddress().GetIsReachable())

	// lifetime missing -> toDevice() returns nil, even though State() itself reported no error.
	client.state = &ChargerState{
		vitals:  &vitalAPIResponse{},
		version: &versionAPIResponse{SerialNumber: "1234ASDF"},
	}

	assert.NoError(t, cb.Refresh(context.Background()), "a partial poll is not itself an RPC error")

	devices := listChargerDevices(t, svc)
	require.Len(t, devices, 1, "the previously published device must still be there, just marked unreachable")
	assert.False(t, devices[0].GetAddress().GetIsReachable())
}
