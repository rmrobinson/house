package main

import (
	"net/url"
	"time"

	"google.golang.org/protobuf/proto"
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

	Enabled bool
	Active  bool
	// MotionDetected is Frigate's raw pixel-motion state; OccupancyDetected is whether it has an active
	// (moving, non-stationary) object of a configured label - by default a person - in view.
	MotionDetected    bool
	OccupancyDetected bool
	LastActivity      time.Time

	// DetectionLabels are the object labels this camera reports in Detection, and Visible the qualifying
	// objects (moving, not judged false) Frigate is currently tracking on it.
	DetectionLabels []string
	Visible         []DetectedObject
}

// DetectedObject is one qualifying tracked object on a camera.
type DetectedObject struct {
	Label         string
	Score         float64
	EventID       string
	FirstSeen     time.Time
	HasStillImage bool
	Zones         []string
	SubLabel      string
	SubLabelScore float64
}

func (o DetectedObject) toProto() *trait.Detection_Object {
	obj := &trait.Detection_Object{
		Label:         o.Label,
		Score:         float32(o.Score),
		EventId:       o.EventID,
		HasStillImage: o.HasStillImage,
		Zones:         append([]string(nil), o.Zones...),
		SubLabel:      o.SubLabel,
		SubLabelScore: float32(o.SubLabelScore),
	}
	if !o.FirstSeen.IsZero() {
		obj.FirstSeen = timestamppb.New(o.FirstSeen)
	}
	return obj
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

	visible := make([]*trait.Detection_Object, 0, len(c.Visible))
	for _, o := range c.Visible {
		visible = append(visible, o.toProto())
	}

	cam := &device.Camera{
		MediaStream: &trait.MediaStream{
			State: &trait.MediaStream_State{
				Url:       c.Endpoint.String(),
				Endpoints: endpoints,
			},
		},
		Presence: &trait.Presence{
			State: &trait.Presence_State{
				MotionDetected:    c.MotionDetected,
				OccupancyDetected: proto.Bool(c.OccupancyDetected),
			},
		},
	}
	if c.Enabled {
		cam.StillImage = &trait.StillImage{
			Attributes: &trait.StillImage_Attributes{
				ContentTypes: []string{"image/jpeg"},
				SupportedSources: []trait.StillImage_Source{
					trait.StillImage_EVENT_SNAPSHOT,
					trait.StillImage_LATEST_FRAME,
				},
			},
		}
		cam.Detection = &trait.Detection{
			Attributes: &trait.Detection_Attributes{Labels: c.DetectionLabels},
			State:      &trait.Detection_State{Visible: visible},
		}
	}

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
		Details: &device.Device_Camera{Camera: cam},
	}
}
