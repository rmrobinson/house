package main

import (
	"net/url"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

type Camera struct {
	ID           string
	Manufacturer string
	ModelID      string
	Name         string
	Endpoint     *url.URL
	WHEPEndpoint *url.URL

	Enabled        bool
	Active         bool
	MotionDetected bool
	LastActivity   time.Time
}

func (c *Camera) ToDevice() *device.Device {
	endpoints := make([]*trait.MediaStream_Endpoint, 0, 2)
	if c.WHEPEndpoint != nil {
		endpoints = append(endpoints, &trait.MediaStream_Endpoint{
			Protocol: trait.MediaStream_WEBRTC_WHEP,
			Url:      c.WHEPEndpoint.String(),
		})
	}
	endpoints = append(endpoints, &trait.MediaStream_Endpoint{
		Protocol: trait.MediaStream_RTSP,
		Url:      c.Endpoint.String(),
	})

	return &device.Device{
		Id:           c.ID,
		ModelId:      c.ModelID,
		Manufacturer: c.Manufacturer,
		LastSeen:     timestamppb.New(c.LastActivity),
		Config:       &device.Device_Config{Name: c.Name},
		Address: &device.Device_Address{
			Address:     c.Endpoint.String(),
			IsReachable: c.Active,
		},
		Details: &device.Device_Camera{
			Camera: &device.Camera{
				MediaStream: &trait.MediaStream{
					State: &trait.MediaStream_State{
						Url:       c.Endpoint.String(),
						Endpoints: endpoints,
					},
				},
				Presence: &trait.Presence{
					State: &trait.Presence_State{MotionDetected: c.MotionDetected},
				},
			},
		},
	}
}
