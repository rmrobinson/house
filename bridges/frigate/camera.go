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

	Enabled        bool
	Active         bool
	MotionDetected bool
	LastActivity   time.Time
}

func (c *Camera) ToDevice() *device.Device {
	return &device.Device{
		Id:           c.ID,
		ModelId:      c.ModelID,
		Manufacturer: c.Manufacturer,
		LastSeen:     timestamppb.New(c.LastActivity),
		Address: &device.Device_Address{
			Address:     c.Name,
			IsReachable: c.Active,
		},
		Details: &device.Device_Camera{
			Camera: &device.Camera{
				MediaStream: &trait.MediaStream{
					State: &trait.MediaStream_State{
						Url: c.Endpoint.String(),
					},
				},
				Presence: &trait.Presence{
					State: &trait.Presence_State{},
				},
			},
		},
	}
}
