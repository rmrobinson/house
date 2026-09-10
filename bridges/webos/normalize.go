package main

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/webosctrl"
)

// normalize converts cfg (this device's static/persisted identity) and st
// (a live webosctrl.Status snapshot) into a device.Device.
//
// Power is intentionally not modeled as a new enum - see the plan doc's
// state-mapping table:
//   - full standby (socket closed): Address.IsReachable false, OnOff left at
//     its last-known value (stale, but the client should already be treating
//     an unreachable device specially).
//   - active-standby / SetPower(false)'d screen-off (socket alive, screen
//     not): Address.IsReachable true, OnOff.State.IsOn false.
//   - screen on: both true.
//   - never yet connected/paired: OnOff left nil, same as bridges/roku's
//     precedent for a trait it can't yet speak to.
//
// Trait population is gated on st.EverPaired, not the live st.Paired -
// st.Paired drops to false on every disconnect (including a TV going to
// full standby), and gating on it here would wipe OnOff/Volume/App/Channel
// back to nil exactly when a client needs OnOff most: to send the
// Wake-on-LAN-backed SetOnOff(true) that recovers from that very state (see
// command.go's turnOn). EverPaired instead stays true once pairing has ever
// succeeded, so the last-known state survives a disconnect.
func normalize(cfg deviceConfig, st webosctrl.Status) *device.Device {
	d := &device.Device{
		Id:           cfg.UUID,
		ModelId:      "webOS TV",
		Manufacturer: "LG",
		Address: &device.Device_Address{
			Address:     cfg.Host,
			IsReachable: st.Reachable,
			HopCount:    1,
		},
		Config: &device.Device_Config{
			Name: cfg.Name,
		},
		LastSeen: timestamppb.Now(),
	}

	tv := &device.Television{}

	if st.EverPaired {
		tv.OnOff = &trait.OnOff{
			Attributes: &trait.OnOff_Attributes{CanControl: true},
			State:      &trait.OnOff_State{IsOn: st.Power == webosctrl.PowerActive},
		}
		tv.Volume = &trait.Volume{
			Attributes: &trait.Volume_Attributes{CanControl: true, CanMute: true, MaximumLevel: 100},
			State:      &trait.Volume_State{IsMuted: st.Muted, Level: st.Volume},
		}
		tv.App = appTrait(st)
		if cfg.HasTuner && st.HasChannel {
			tv.Channel = channelTrait(st)
		}
	}

	d.Details = &device.Device_Television{Television: tv}
	d.Version = ComputeVersion(d)
	return d
}

func appTrait(st webosctrl.Status) *trait.App {
	apps := make([]*trait.App_Instance, 0, len(st.Apps))
	for _, a := range st.Apps {
		apps = append(apps, &trait.App_Instance{Id: a.ID, Name: a.Name, Version: a.Version})
	}
	return &trait.App{
		Attributes: &trait.App_Attributes{CanControl: true, Applications: apps},
		State:      &trait.App_State{ApplicationId: st.ForegroundID},
	}
}

func channelTrait(st webosctrl.Status) *trait.Channel {
	return &trait.Channel{
		Attributes: &trait.Channel_Attributes{CanControl: true, HasTuner: true},
		State: &trait.Channel_State{
			CurrentChannel: &trait.Channel_ChannelDetails{
				Id:     st.Channel.ID,
				Number: st.Channel.Number,
				Name:   st.Channel.Name,
			},
		},
	}
}
