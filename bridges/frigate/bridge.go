package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/bridges/frigate/frigate"
	"github.com/rmrobinson/house/service/bridge"
)

const (
	cameraRestreamFormat     = "rtsp://%s:8554/%s"
	cameraRestreamWHEPFormat = "http://%s:%d/api/webrtc?src=%s"

	// presenceGrace is how long a dropped motion feed may stay down before every camera's presence is
	// cleared. Frigate doesn't replay current state when a client connects, so clearing on every blip would
	// report someone absent who is still standing there; but a feed down for long enough that a lights-off
	// transition could have been missed must fail safe to "nobody there".
	presenceGrace = 30 * time.Second

	motionReconnectMin = time.Second
	motionReconnectMax = 30 * time.Second
)

// CameraConfig includes basic configuration data for a specific camera identified using its Name
type CameraConfig struct {
	Name         string `mapstructure:"name"`
	Manufacturer string `mapstructure:"manufacturer"`
	ModelID      string `mapstructure:"model_id"`
}

// FrigateBridge is a bridge between the Frigate NVR system and the house.
type FrigateBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	client                 *frigate.Client
	cameraRestreamHostname string
	cameraRestreamHTTPPort int

	// mu guards cameras' contents: Refresh and the motion stream both update them from separate
	// goroutines.
	mu      sync.Mutex
	cameras map[string]*Camera

	// occupancyLabels is the set of object labels that make a camera report occupancy, and active is the
	// last-reported count of each, per camera.
	occupancyLabels map[string]bool
	active          map[string]map[string]int

	// publish pushes a camera's current state to the house; it defaults to svc.UpdateDevice and is a
	// field only so tests can observe publishes without a running bridge service.
	publish func(*Camera)

	// presenceGrace overrides the package default; tests shorten it.
	presenceGrace time.Duration
}

// NewFrigateBridge returns a new instance of the Frigate bridge.
func NewFrigateBridge(logger *zap.Logger, svc *bridge.Service, client *frigate.Client, cameraRestreamHostname string, cameraRestreamHTTPPort int) *FrigateBridge {
	b := &api2.Bridge{
		Id:           viper.GetString("bridge.id"),
		IsReachable:  true,
		ModelId:      "Frigate11",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Ip: &api2.Address_Ip{
					Host: client.GetIP(),
					Port: int32(client.GetPort()),
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}
	return &FrigateBridge{
		logger:                 logger,
		svc:                    svc,
		b:                      b,
		client:                 client,
		cameraRestreamHostname: cameraRestreamHostname,
		cameraRestreamHTTPPort: cameraRestreamHTTPPort,
		cameras:                map[string]*Camera{},
		occupancyLabels:        map[string]bool{"person": true},
		active:                 map[string]map[string]int{},
		publish:                func(c *Camera) { svc.UpdateDevice(c.ToDevice()) },
		presenceGrace:          presenceGrace,
	}
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (fb *FrigateBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	fb.logger.Error("received unsupported command - shouldn't happen")
	return nil, bridge.ErrUnsupportedCommand
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (fb *FrigateBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	fb.b.Config.Name = config.Name
	fb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Setup loads the configured cameras into the bridge for use. It then retrieves initial state and errors if it can't reach the Frigate API.
func (fb *FrigateBridge) Setup(ctx context.Context, cameras []CameraConfig) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	for _, camera := range cameras {
		fb.cameras[camera.Name] = fb.newCamera(camera)
	}

	config, err := fb.client.GetConfig(ctx)
	if err != nil {
		fb.logger.Error("unable to get config from frigate",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get config from frigate")
	}
	stats, err := fb.client.GetStats(ctx)
	if err != nil {
		fb.logger.Error("unable to get stats from frigate",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get stats from frigate")
	}

	for cameraName, frigateCameraConfig := range config.Cameras {
		ep, err := url.Parse(fmt.Sprintf(cameraRestreamFormat, fb.cameraRestreamHostname, cameraName))
		if err != nil {
			fb.logger.Error("unable to parse camera restream endpoint as url", zap.Error(err))
			return err
		}
		whepEp, err := url.Parse(fmt.Sprintf(cameraRestreamWHEPFormat, fb.cameraRestreamHostname, fb.cameraRestreamHTTPPort, cameraName))
		if err != nil {
			fb.logger.Error("unable to parse camera restream WHEP endpoint as url", zap.Error(err))
			return err
		}

		if camera, cameraPresent := fb.cameras[cameraName]; cameraPresent {
			camera.Enabled = frigateCameraConfig.Enabled
			camera.Endpoint = ep
			camera.WHEPEndpoint = whepEp

			if cameraStats, statsPresent := stats.Cameras[cameraName]; statsPresent {
				camera.Active = (cameraStats.CameraFPS > 0)
				camera.LastActivity = time.Now() // TODO: use the 'events' feed for this
			}

			fb.cameras[cameraName] = camera
			fb.publish(camera)
		} else {
			// In this case we haven't gotten an initial config for this camera but we can mark the Model and Manufacturer as unknown
			camera := fb.newCamera(CameraConfig{Name: frigateCameraConfig.Name, Manufacturer: "Unknown", ModelID: "Unknown"})
			camera.Enabled = frigateCameraConfig.Enabled
			camera.Endpoint = ep
			camera.WHEPEndpoint = whepEp

			if cameraStats, statsPresent := stats.Cameras[cameraName]; statsPresent {
				camera.Active = (cameraStats.CameraFPS > 0)
				camera.LastActivity = time.Now() // TODO: use the 'events' feed for this
			}

			fb.cameras[cameraName] = camera
			fb.publish(camera)
		}
	}

	fb.b.ModelId = stats.Service.Version

	return nil
}

func (fb *FrigateBridge) newCamera(config CameraConfig) *Camera {
	idBytes := sha256.Sum256([]byte(fmt.Sprintf("%s:%s", fb.client.GetIP(), config.Name)))

	return &Camera{
		ID:           hex.EncodeToString(idBytes[:]),
		Name:         config.Name,
		Manufacturer: config.Manufacturer,
		ModelID:      config.ModelID,
		Enabled:      false,
		Active:       false,
	}
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. This bridge has no
// device traits eligible for asynchronous commands, so it always returns ErrAsyncCommandsNotSupported.
func (fb *FrigateBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation it queries
// the Frigate API and returns the current state of the cameras.
func (fb *FrigateBridge) Refresh(ctx context.Context) error {
	stats, err := fb.client.GetStats(ctx)
	if err != nil {
		fb.logger.Error("unable to get stats from frigate",
			zap.Error(err))
		return status.Error(codes.Internal, "unable to get stats from frigate")
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()

	for cameraName, camera := range fb.cameras {
		if cameraStats, statsPresent := stats.Cameras[cameraName]; statsPresent {
			camera.Active = (cameraStats.CameraFPS > 0)
			camera.LastActivity = time.Now() // TODO: use the 'events' feed for this
			fb.cameras[cameraName] = camera
			fb.publish(camera)
		}
	}
	return nil
}

// SetOccupancyLabels sets which object labels (e.g. "person") make a camera report occupancy. Call before Run.
func (fb *FrigateBridge) SetOccupancyLabels(labels []string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	fb.occupancyLabels = map[string]bool{}
	for _, l := range labels {
		fb.occupancyLabels[l] = true
	}
}

// setMotion records a camera's raw pixel-motion state and republishes it if that changed. Cameras this
// bridge isn't configured for are ignored.
func (fb *FrigateBridge) setMotion(name string, motion bool) {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	camera, ok := fb.cameras[name]
	if !ok || camera.MotionDetected == motion {
		return
	}
	camera.MotionDetected = motion
	fb.publish(camera)
}

// setActiveCount records how many active objects of label are on a camera, and republishes the camera if
// whether it has any object of an occupancy label changed. Labels outside occupancyLabels and cameras this
// bridge isn't configured for are ignored.
func (fb *FrigateBridge) setActiveCount(name, label string, count int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	camera, ok := fb.cameras[name]
	if !ok || !fb.occupancyLabels[label] {
		return
	}

	if fb.active[name] == nil {
		fb.active[name] = map[string]int{}
	}
	fb.active[name][label] = count

	occupied := false
	for l, n := range fb.active[name] {
		if n > 0 && fb.occupancyLabels[l] {
			occupied = true
		}
	}
	if camera.OccupancyDetected == occupied {
		return
	}
	camera.OccupancyDetected = occupied
	fb.publish(camera)
}

// clearPresence forgets every active-object count and marks every camera as having neither motion nor
// occupancy. Used when the feed drops: Frigate only publishes changes, so a value still held would never be
// reset by the update we missed, leaving presence-triggered policies stuck.
func (fb *FrigateBridge) clearPresence() {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	fb.active = map[string]map[string]int{}
	for _, camera := range fb.cameras {
		if camera.MotionDetected || camera.OccupancyDetected {
			camera.MotionDetected = false
			camera.OccupancyDetected = false
			fb.publish(camera)
		}
	}
}

// watchMotion keeps a websocket to Frigate open for the life of ctx, applying its motion and active-object
// updates and reconnecting with backoff after a drop. Presence is held across a brief drop and only cleared
// if the feed stays down for presenceGrace.
func (fb *FrigateBridge) watchMotion(ctx context.Context) {
	backoff := motionReconnectMin

	// staleTimer fires clearPresence once the feed has been down for presenceGrace. It's stopped whenever a
	// connection is established, and only ever touched from this goroutine (OnConnect runs synchronously
	// inside StreamFeed).
	var staleTimer *time.Timer
	defer func() {
		if staleTimer != nil {
			staleTimer.Stop()
		}
	}()

	for ctx.Err() == nil {
		started := time.Now()
		err := fb.client.StreamFeed(ctx, frigate.FeedHandlers{
			OnConnect: func() {
				if staleTimer != nil {
					staleTimer.Stop()
				}
			},
			OnMotion:      fb.setMotion,
			OnActiveCount: fb.setActiveCount,
		})
		if ctx.Err() != nil {
			return
		}
		fb.logger.Error("frigate motion feed dropped, reconnecting", zap.Error(err), zap.Duration("backoff", backoff))

		if staleTimer == nil {
			staleTimer = time.AfterFunc(fb.presenceGrace, fb.clearPresence)
		} else {
			staleTimer.Reset(fb.presenceGrace)
		}

		// A feed that held for a while was healthy, so start the backoff over rather than keep growing it.
		if time.Since(started) > motionReconnectMax {
			backoff = motionReconnectMin
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, motionReconnectMax)
	}
}

// Run begins the process of polling the sensor and reporting back the state.
func (fb *FrigateBridge) Run(ctx context.Context) {
	fb.Refresh(ctx)

	go fb.watchMotion(ctx)

	refreshTimer := time.NewTicker(time.Second * time.Duration(viper.GetInt("bridge.refresh_interval")))
	fb.logger.Info("beginning refresh loop", zap.Int("refresh_interval", viper.GetInt("bridge.refresh_interval")))
	for {
		select {
		case <-refreshTimer.C:
			if err := fb.Refresh(ctx); err != nil {
				fb.logger.Error("unable to get cameras from frigate",
					zap.Error(err))
				continue
			}
			fb.logger.Debug("refreshed")
		case <-ctx.Done():
			fb.logger.Info("run context cancelled")
			return
		}
	}
}
