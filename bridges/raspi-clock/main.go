package main

import (
	"context"

	"github.com/rafalop/sevensegment"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/bridge"
)

const i2cAddress = 0x70

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	svc := bridge.NewService(logger)

	d := sevensegment.NewSevenSegment(i2cAddress)
	d.Clear()
	d.SetBrightness(0)

	c := NewClock(d)
	go c.Run(context.Background())

	_ = NewClockBridge(logger, svc, c)

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
