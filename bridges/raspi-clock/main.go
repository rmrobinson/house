package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/rafalop/sevensegment"
	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	d := sevensegment.NewSevenSegment(0x70)
	d.Clear()
	d.SetBrightness(0)

	c := NewClock(d)
	go c.Run(context.Background())

	modelName := "Adafruit 7 Segment Display"

	cb := &ClockBridge{
		c: c,
		d: &device.Device{
			Id:           "temporary-id",
			ModelId:      "ADA-879",
			Manufacturer: "Adafruit",
			ModelName:    &modelName,
			Details: &device.Device_Clock{
				Clock: &device.Clock{
					OnOff: &trait.OnOff{
						Attributes: &trait.OnOff_Attributes{
							CanControl: true,
						},
						State: &trait.OnOff_State{
							IsOn: true,
						},
					},
					Brightness: &trait.Brightness{
						Attributes: &trait.Brightness_Attributes{
							CanControl: true,
						},
						State: &trait.Brightness_State{
							Level: 100,
						},
					},
					Time: &trait.Time{
						Attributes: &trait.Time_Attributes{
							CanControl:  true,
							SupportsNtp: true,
						},
						State: &trait.Time_State{
							SetTimezoneAutomatically: false,
							Timezone:                 "UTC",
							TimeFormat:               trait.Time_TIME_FORMAT_24H,
						},
					},
				},
			},
		},
	}

	b := &api2.Bridge{
		Id:               "temporary-id",
		IsReachable:      true,
		ModelId:          "RPiCl1",
		Manufacturer:     "Faltung Networks",
		ModelName:        nil,
		ModelDescription: nil,
		Config: &api2.Bridge_Config{
			Name:        "",
			Description: "",
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: "10.17.20.102",
					Port: 12345,
				},
			},
			Timezone: time.Local.String(),
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	svc := bridge.NewService(logger, cb, b)
	api := bridge.NewAPI(logger, svc)

	svc.UpdateDevice(cb.d)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 12345))
	if err != nil {
		logger.Fatal("failed to listen", zap.Error(err))
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	api2.RegisterBridgeServiceServer(grpcServer, api)

	logger.Info("serving requests", zap.String("address", lis.Addr().String()))
	grpcServer.Serve(lis)
}
