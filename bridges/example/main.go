package main

import (
	"time"

	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/bridge"
)

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	svc := bridge.NewService(logger)

	eb := NewExampleBridge(logger, svc)

	go func() {
		// Use this to mimic a bridge which takes a bit of time to detect and connect
		time.Sleep(time.Second * 15)
		logger.Debug("bridge initialized")

		svc.RegisterHandler(eb, eb.b)

		svc.UpdateDevice(eb.d1.toDevice())
		svc.UpdateDevice(eb.d2.toDevice())
	}()

	go eb.Run()

	s := bridge.NewServer(logger, svc)
	s.Serve()
}
