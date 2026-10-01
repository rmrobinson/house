package main

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rmrobinson/house/api/trait"
)

func TestCameraToDeviceEmitsWHEPThenRTSPEndpoints(t *testing.T) {
	rtsp, err := url.Parse("rtsp://192.168.1.100:8554/driveway_camera")
	require.NoError(t, err)
	whep, err := url.Parse("http://192.168.1.100:1984/api/webrtc?src=driveway_camera")
	require.NoError(t, err)

	c := &Camera{
		ID:           "cam1",
		Name:         "driveway_camera",
		Manufacturer: "Reolink",
		ModelID:      "RLC510A",
		Endpoint:     rtsp,
		WHEPEndpoint: whep,
	}

	ms := c.ToDevice().GetCamera().GetMediaStream()

	// Legacy field keeps pointing at the RTSP URL for back-compat.
	assert.Equal(t, "rtsp://192.168.1.100:8554/driveway_camera", ms.GetState().GetUrl())

	endpoints := ms.GetState().GetEndpoints()
	require.Len(t, endpoints, 2)
	assert.Equal(t, trait.MediaStream_WEBRTC_WHEP, endpoints[0].GetProtocol())
	assert.Equal(t, "http://192.168.1.100:1984/api/webrtc?src=driveway_camera", endpoints[0].GetUrl())
	assert.Equal(t, trait.MediaStream_RTSP, endpoints[1].GetProtocol())
	assert.Equal(t, "rtsp://192.168.1.100:8554/driveway_camera", endpoints[1].GetUrl())
}

func TestCameraToDeviceOmitsWHEPEndpointWhenUnset(t *testing.T) {
	rtsp, err := url.Parse("rtsp://192.168.1.100:8554/driveway_camera")
	require.NoError(t, err)

	c := &Camera{
		ID:       "cam1",
		Name:     "driveway_camera",
		Endpoint: rtsp,
	}

	ms := c.ToDevice().GetCamera().GetMediaStream()

	endpoints := ms.GetState().GetEndpoints()
	require.Len(t, endpoints, 1)
	assert.Equal(t, trait.MediaStream_RTSP, endpoints[0].GetProtocol())
}
