package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mdlayher/apcupsd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/service/bridge"
)

// fakeStatusClient returns a canned Status (or error) from each call in order, cycling the last
// entry once exhausted - lets a test drive Refresh through a sequence of responses, e.g. an
// empty-serial status (apcupsd mid-sync) followed by a real one.
type fakeStatusClient struct {
	statuses []*apcupsd.Status
	i        int
}

func (f *fakeStatusClient) Status() (*apcupsd.Status, error) {
	s := f.statuses[f.i]
	if f.i < len(f.statuses)-1 {
		f.i++
	}
	return s, nil
}

// erroringStatusClient always fails Status, simulating a dead connection (e.g. apcupsd restarted
// and closed the socket this client was opened on). Tracks whether Close was called so a test can
// confirm the bridge closes the old connection once it reconnects.
type erroringStatusClient struct {
	err    error
	closed bool
}

func (e *erroringStatusClient) Status() (*apcupsd.Status, error) { return nil, e.err }
func (e *erroringStatusClient) Close() error                     { e.closed = true; return nil }

func newTestBridge(t *testing.T, client statusClient) (*APCUPSBridge, *bridge.Service) {
	t.Helper()
	return newTestBridgeWithDial(t, client, func() (statusClient, error) {
		t.Fatal("dial should not be called")
		return nil, nil
	})
}

func newTestBridgeWithDial(t *testing.T, client statusClient, dial func() (statusClient, error)) (*APCUPSBridge, *bridge.Service) {
	t.Helper()
	svc := bridge.NewService(zaptest.NewLogger(t))
	aub := NewAPCUPSBridge(zaptest.NewLogger(t), svc, client, dial, "127.0.0.1", 3551)
	svc.RegisterHandler(aub, aub.b)
	return aub, svc
}

func listDevices(t *testing.T, svc *bridge.Service) []*device.Device {
	t.Helper()
	resp, err := svc.API().ListDevices(context.Background(), &api2.ListDevicesRequest{})
	require.NoError(t, err)
	return resp.GetDevices()
}

func TestRefresh_EmptySerialNumber_SkippedNotPublished(t *testing.T) {
	client := &fakeStatusClient{statuses: []*apcupsd.Status{
		{}, // apcupsd mid-sync: every field, including SerialNumber, still zero-value
	}}
	aub, svc := newTestBridge(t, client)

	require.NoError(t, aub.Refresh(context.Background()))

	assert.Empty(t, listDevices(t, svc), "an empty-serial status must never be published as a device")
}

func TestRefresh_EmptySerialThenReal_OnlyRealDevicePublished(t *testing.T) {
	client := &fakeStatusClient{statuses: []*apcupsd.Status{
		{}, // first poll: apcupsd still mid-sync
		{SerialNumber: "0B2542L21100", Model: "Back-UPS ES 850G2", EndAPC: time.Now()},
	}}
	aub, svc := newTestBridge(t, client)

	require.NoError(t, aub.Refresh(context.Background()))
	require.NoError(t, aub.Refresh(context.Background()))

	devices := listDevices(t, svc)
	require.Len(t, devices, 1, "the empty-serial poll must not leave behind a second, phantom device")
	assert.Equal(t, "0B2542L21100", devices[0].GetId())
}

func TestRefresh_RealSerialNumber_Published(t *testing.T) {
	client := &fakeStatusClient{statuses: []*apcupsd.Status{
		{SerialNumber: "0B2542L21100", Model: "Back-UPS ES 850G2", EndAPC: time.Now()},
	}}
	aub, svc := newTestBridge(t, client)

	require.NoError(t, aub.Refresh(context.Background()))

	devices := listDevices(t, svc)
	require.Len(t, devices, 1)
	assert.Equal(t, "0B2542L21100", devices[0].GetId())
	assert.Nil(t, devices[0].GetConfig(), "no UPSNAME set on the UPS: Config must not be synthesized")
}

func TestRefresh_UPSNameSet_UsedAsConfigName(t *testing.T) {
	client := &fakeStatusClient{statuses: []*apcupsd.Status{
		{SerialNumber: "0B2542L21100", Model: "Back-UPS ES 850G2", UPSName: "basement-ups", EndAPC: time.Now()},
	}}
	aub, svc := newTestBridge(t, client)

	require.NoError(t, aub.Refresh(context.Background()))

	devices := listDevices(t, svc)
	require.Len(t, devices, 1)
	require.NotNil(t, devices[0].GetConfig())
	assert.Equal(t, "basement-ups", devices[0].GetConfig().GetName())
}

func TestRefresh_StatusError_ReconnectsAndPublishes(t *testing.T) {
	failing := &erroringStatusClient{err: errors.New("connection reset")}
	reconnected := &fakeStatusClient{statuses: []*apcupsd.Status{
		{SerialNumber: "0B2542L21100", Model: "Back-UPS ES 850G2", EndAPC: time.Now()},
	}}
	dialCalls := 0
	aub, svc := newTestBridgeWithDial(t, failing, func() (statusClient, error) {
		dialCalls++
		return reconnected, nil
	})

	require.NoError(t, aub.Refresh(context.Background()))

	assert.Equal(t, 1, dialCalls, "dial should be called exactly once after the failed Status call")
	assert.Same(t, reconnected, aub.client, "the reconnected client should replace the dead one")
	assert.True(t, failing.closed, "the dead connection should be closed once replaced")

	devices := listDevices(t, svc)
	require.Len(t, devices, 1)
	assert.Equal(t, "0B2542L21100", devices[0].GetId())
}

func TestRefresh_DialFails_ReturnsError(t *testing.T) {
	failing := &erroringStatusClient{err: errors.New("connection reset")}
	aub, svc := newTestBridgeWithDial(t, failing, func() (statusClient, error) {
		return nil, errors.New("ups unreachable")
	})

	assert.Error(t, aub.Refresh(context.Background()))
	assert.Same(t, failing, aub.client, "client must be left in place when reconnecting fails")
	assert.Empty(t, listDevices(t, svc))
}

func TestRefresh_ReconnectedClientStillErrors_ReturnsError(t *testing.T) {
	failing := &erroringStatusClient{err: errors.New("connection reset")}
	stillFailing := &erroringStatusClient{err: errors.New("still down")}
	aub, svc := newTestBridgeWithDial(t, failing, func() (statusClient, error) {
		return stillFailing, nil
	})

	assert.Error(t, aub.Refresh(context.Background()))
	assert.Same(t, stillFailing, aub.client, "the newly dialed client should still replace the old one even though it also errors")
	assert.Empty(t, listDevices(t, svc))
}
