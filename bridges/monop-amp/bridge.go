package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/spf13/viper"
	"github.com/tarm/serial"

	monopamp "github.com/rmrobinson/monoprice-amp-go"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
	"github.com/rmrobinson/house/service/lib/configutil"
)

const (
	maxZoneID            = 6
	maxChannelID         = 6
	commandSpaceInterval = time.Millisecond * 100

	// The amp's native ranges are exposed through the API as-is, with no scaling: volume is
	// reported via Volume.Attributes.maximum_level, while treble/bass/balance (which have no
	// range attributes on the AudioOutput trait) are always in these ranges, with the midpoint
	// being 'flat'/'centred'.
	maxVolumeLevel  = 38
	maxToneLevel    = 14
	maxBalanceLevel = 20
)

var (
	// ErrSpeakerRefreshFailed is returned if the bridge wasn't able to refresh the speaker state
	ErrSpeakerRefreshFailed = status.Error(codes.Internal, "speaker refresh failed")
	// ErrSpeakerCommandFailed is returned if the bridge failed to execute the specified command
	ErrSpeakerCommandFailed = status.Error(codes.Internal, "speaker command failed")
	// ErrZoneUnavailable is returned if the speaker is configured but the amplifier didn't report that zone.
	ErrZoneUnavailable = status.Error(codes.Unavailable, "the amplifier did not report this zone")
)

type inputDetails struct {
	ID          int    `mapstructure:"id"`
	Name        string `mapstructure:"name"`
	Description string `mapstructure:"description"`
	Active      bool   `mapstructure:"active"`
}

type speakerDetails struct {
	ID          int    `mapstructure:"id"`
	Name        string `mapstructure:"name"`
	Description string `mapstructure:"description"`
	Active      bool   `mapstructure:"active"`

	lastSeen  time.Time
	reachable bool
}

// zone is the subset of *monopamp.Zone the bridge uses; it exists so tests can fake the amp.
type zone interface {
	ID() string
	Refresh() error
	State() *monopamp.State
	SetPower(on bool) error
	SetMute(on bool) error
	SetVolume(level int) error
	SetTreble(level int) error
	SetBass(level int) error
	SetBalance(level int) error
	SetSourceChannel(channelID int) error
}

// amplifier hands out the zones the amp reported. Zone returns nil if the amp didn't report id.
type amplifier interface {
	Zone(id int) zone
}

// serialAmp adapts the library's amplifier, taking care that a missing zone is a nil interface
// rather than an interface holding a nil *monopamp.Zone.
type serialAmp struct {
	a *monopamp.SerialAmplifier
}

func (s serialAmp) Zone(id int) zone {
	z := s.a.Zone(id)
	if z == nil {
		return nil
	}
	return z
}

// MonopriceAmpBridge is a bridge to the Monoprice amplifier.
type MonopriceAmpBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	configPath  string
	bridgeID    string
	usbPath     string
	usbBaudRate int

	inputs []inputDetails

	// mu guards speakers, bridgeReachable and every call into amp/zones (zone state is a
	// cached struct the library mutates in place).
	mu              sync.Mutex
	speakers        map[int]speakerDetails
	bridgeReachable bool

	port *serial.Port
	amp  amplifier
}

// NewMonopriceAmpBridge creates a new bridge to a Monoprice amplifier.
func NewMonopriceAmpBridge(logger *zap.Logger, svc *bridge.Service, configPath string, usbPath string, usbBaudRate int, inputs []inputDetails, speakers []speakerDetails) *MonopriceAmpBridge {
	bridgeID := viper.GetString("bridge.id")
	b := &api2.Bridge{
		Id:           bridgeID,
		IsReachable:  true,
		ModelId:      "MP1",
		Manufacturer: "Faltung Networks",
		Config: &api2.Bridge_Config{
			Name:        viper.GetString("bridge.name"),
			Description: viper.GetString("bridge.description"),
			Address: &api2.Address{
				Usb: &api2.Address_Usb{
					Path: usbPath,
				},
			},
		},
		State: &api2.Bridge_State{
			IsPaired: true,
		},
	}

	speakersMap := map[int]speakerDetails{}
	for _, speaker := range speakers {
		speakersMap[speaker.ID] = speaker
	}

	return &MonopriceAmpBridge{
		logger:          logger,
		svc:             svc,
		b:               b,
		configPath:      configPath,
		bridgeID:        bridgeID,
		usbPath:         usbPath,
		usbBaudRate:     usbBaudRate,
		inputs:          inputs,
		speakers:        speakersMap,
		bridgeReachable: true,
	}
}

// lookupLocked resolves a device ID to a configured, active speaker and the zone the amp
// reported for it. mu must be held.
func (mpb *MonopriceAmpBridge) lookupLocked(deviceID string) (int, zone, error) {
	if !mpb.isValidDeviceID(deviceID) {
		mpb.logger.Info("received command for invalid device id", zap.String("device_id", deviceID))
		return 0, nil, bridge.ErrDeviceNotFound
	}

	speakerID, err := strconv.Atoi(strings.TrimPrefix(deviceID, mpb.bridgeID+":"))
	if err != nil {
		mpb.logger.Info("received command for malformed device id", zap.String("device_id", deviceID))
		return 0, nil, bridge.ErrDeviceNotFound
	}
	if speaker, ok := mpb.speakers[speakerID]; !ok || !speaker.Active {
		mpb.logger.Info("received command for unconfigured or inactive speaker", zap.String("device_id", deviceID))
		return 0, nil, bridge.ErrDeviceNotFound
	}

	z := mpb.amp.Zone(speakerID)
	if z == nil {
		mpb.logger.Error("amplifier did not report configured zone", zap.Int("speaker_id", speakerID))
		return 0, nil, ErrZoneUnavailable
	}
	return speakerID, z, nil
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (mpb *MonopriceAmpBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	mpb.mu.Lock()
	defer mpb.mu.Unlock()

	speakerID, z, err := mpb.lookupLocked(cmd.DeviceId)
	if err != nil {
		return nil, err
	}

	if err := mpb.applyLocked(cmd, z); err != nil {
		return nil, err
	}

	speaker := mpb.speakers[speakerID]
	speaker.lastSeen = time.Now()
	speaker.reachable = true
	mpb.speakers[speakerID] = speaker

	return mpb.speakerToDevice(speakerID, z.State()), nil
}

// applyLocked executes cmd against z. The amp supports the OnOff, Volume (absolute/relative/mute),
// Input and AudioOutput commands. Values are validated against the amp's native ranges here since
// the bridge API layer only checks that the device supports the command type.
func (mpb *MonopriceAmpBridge) applyLocked(cmd *command.Command, z zone) error {
	fail := func(what string, err error) error {
		mpb.logger.Error("unable to "+what, zap.String("device_id", cmd.DeviceId), zap.Error(err))
		return ErrSpeakerCommandFailed
	}

	if cmd.GetOnOff() != nil {
		if err := z.SetPower(cmd.GetOnOff().GetOn()); err != nil {
			return fail("set power", err)
		}
	} else if cmd.GetVolumeAbsolute() != nil {
		level := cmd.GetVolumeAbsolute().Level
		if level < 0 || level > maxVolumeLevel {
			return bridge.ErrArgumentNotSupportedByDevice
		}
		if err := z.SetVolume(int(level)); err != nil {
			return fail("set volume", err)
		}
	} else if cmd.GetVolumeRelative() != nil {
		level := int32(z.State().Volume) + cmd.GetVolumeRelative().Delta
		level = max(0, min(level, maxVolumeLevel))
		if err := z.SetVolume(int(level)); err != nil {
			return fail("adjust volume", err)
		}
	} else if cmd.GetMute() != nil {
		if err := z.SetMute(cmd.GetMute().IsMuted); err != nil {
			return fail("set mute", err)
		}
	} else if cmd.GetInput() != nil {
		if !mpb.hasInput(cmd.GetInput().InputId) {
			return bridge.ErrArgumentNotSupportedByDevice
		}
		if err := z.SetSourceChannel(inputIDFromAPI(cmd.GetInput().InputId)); err != nil {
			return fail("set input", err)
		}
	} else if ao := cmd.GetAudioOutput(); ao != nil {
		// Validate everything before sending anything so a bad value doesn't leave the
		// zone half-updated.
		if (ao.TrebleLevel != nil && !inRange(*ao.TrebleLevel, maxToneLevel)) ||
			(ao.BassLevel != nil && !inRange(*ao.BassLevel, maxToneLevel)) ||
			(ao.Balance != nil && !inRange(*ao.Balance, maxBalanceLevel)) {
			return bridge.ErrArgumentNotSupportedByDevice
		}

		if ao.TrebleLevel != nil {
			if err := z.SetTreble(int(*ao.TrebleLevel)); err != nil {
				return fail("set treble", err)
			}
			time.Sleep(commandSpaceInterval)
		}
		if ao.BassLevel != nil {
			if err := z.SetBass(int(*ao.BassLevel)); err != nil {
				return fail("set bass", err)
			}
			time.Sleep(commandSpaceInterval)
		}
		if ao.Balance != nil {
			if err := z.SetBalance(int(*ao.Balance)); err != nil {
				return fail("set balance", err)
			}
		}
	} else {
		mpb.logger.Error("received unsupported command - shouldn't happen")
		return bridge.ErrUnsupportedCommand
	}
	return nil
}

func inRange(v, upper int32) bool {
	return v >= 0 && v <= upper
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (mpb *MonopriceAmpBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	mpb.mu.Lock()
	mpb.b.Config.Name = config.Name
	mpb.b.Config.Description = config.Description
	mpb.mu.Unlock()

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	return configutil.PersistValues(mpb.configPath,
		configutil.KeyValue{KeyPath: "bridge.name", Value: config.Name},
		configutil.KeyValue{KeyPath: "bridge.description", Value: config.Description})
}

// ProcessCommandAsync is present to conform to the bridge.Handler interface. The amp's serial
// protocol has no command confirmation to wait on, so there is no async path.
func (mpb *MonopriceAmpBridge) ProcessCommandAsync(ctx context.Context, cmd *command.Command) error {
	return bridge.ErrAsyncCommandsNotSupported
}

// Refresh re-reads every active zone from the amplifier and publishes the result. A zone that
// fails doesn't stop the others from being refreshed; it is published as unreachable and the
// failures are returned together.
func (mpb *MonopriceAmpBridge) Refresh(ctx context.Context) error {
	devices, err := mpb.refresh()
	for _, d := range devices {
		mpb.svc.UpdateDevice(d)
	}
	return err
}

// refresh does the locked half of Refresh: it returns the devices to publish and any failures.
func (mpb *MonopriceAmpBridge) refresh() ([]*device.Device, error) {
	mpb.mu.Lock()
	defer mpb.mu.Unlock()

	var devices []*device.Device
	var errs []error
	anyReachable := false
	for speakerID, speaker := range mpb.speakers {
		if !speaker.Active {
			continue
		}

		var state *monopamp.State
		var err error
		if z := mpb.amp.Zone(speakerID); z == nil {
			err = ErrZoneUnavailable
		} else if err = z.Refresh(); err == nil {
			state = z.State()
		} else {
			// Keep serving the last state we had, just flagged unreachable.
			state = z.State()
		}

		if err != nil {
			mpb.logger.Error("unable to refresh speaker zone", zap.Int("speaker_id", speakerID), zap.Error(err))
			errs = append(errs, fmt.Errorf("speaker %d: %w", speakerID, err))
			speaker.reachable = false
		} else {
			speaker.reachable = true
			speaker.lastSeen = time.Now()
			anyReachable = true
		}
		mpb.speakers[speakerID] = speaker
		devices = append(devices, mpb.speakerToDevice(speakerID, state))
	}

	mpb.updateBridgeReachableLocked(anyReachable || len(devices) == 0)

	if len(errs) > 0 {
		return devices, errors.Join(append([]error{ErrSpeakerRefreshFailed}, errs...)...)
	}
	return devices, nil
}

// updateBridgeReachableLocked publishes a bridge update when reachability flips. The service
// keeps the pointer it was registered with, so the update is sent as a clone and mpb.b is only
// changed afterwards (changing it first would make the service's own change check see no diff).
func (mpb *MonopriceAmpBridge) updateBridgeReachableLocked(reachable bool) {
	if reachable == mpb.bridgeReachable {
		return
	}
	mpb.bridgeReachable = reachable

	updated := proto.Clone(mpb.b).(*api2.Bridge)
	updated.IsReachable = reachable
	mpb.svc.UpdateBridge(updated)
	mpb.b.IsReachable = reachable
}

// Run refreshes the amplifier every interval until ctx is cancelled. Wall keypads can change the
// amp's state without the bridge hearing about it, so this is how those changes are noticed.
func (mpb *MonopriceAmpBridge) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		mpb.logger.Info("periodic refresh disabled")
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	mpb.logger.Info("beginning refresh loop", zap.Duration("refresh_interval", interval))
	for {
		select {
		case <-ticker.C:
			if err := mpb.Refresh(ctx); err != nil {
				mpb.logger.Error("unable to refresh amplifier state", zap.Error(err))
				continue
			}
			mpb.logger.Debug("refreshed")
		case <-ctx.Done():
			mpb.logger.Info("run context cancelled")
			return
		}
	}
}

// hasInput reports whether id matches one of the configured, active inputs.
func (mpb *MonopriceAmpBridge) hasInput(id string) bool {
	for _, input := range mpb.inputs {
		if input.Active && fmt.Sprintf("%d", input.ID) == id {
			return true
		}
	}
	return false
}

// Start validates that the supplied inputs and speakers are correct, and then attempts to connect
// to the amplifier over USB. An error here means that the bridge won't be operating.
func (mpb *MonopriceAmpBridge) Start(ctx context.Context) error {
	for _, input := range mpb.inputs {
		if input.ID > maxChannelID || input.ID <= 0 {
			mpb.logger.Error("input ID out of range",
				zap.Int("input.id", input.ID), zap.String("input.name", input.Name))

			return errors.New("input ID out of range")
		}
	}
	for speakerIdx, speaker := range mpb.speakers {
		if speaker.ID > maxZoneID || speaker.ID <= 0 {
			mpb.logger.Error("speaker ID out of range",
				zap.Int("speaker.id", speaker.ID), zap.String("speaker.name", speaker.Name))

			return errors.New("speaker ID out of range")
		}
		// Speaker ID is valid, record now as it's last seen time and save it
		speaker.lastSeen = time.Now()
		mpb.speakers[speakerIdx] = speaker
	}

	c := &serial.Config{
		Name: mpb.usbPath,
		Baud: mpb.usbBaudRate,
	}
	port, err := serial.OpenPort(c)
	if err != nil {
		mpb.logger.Error("error initializing serial port",
			zap.String("port_path", mpb.usbPath),
			zap.Int("baud_rate", mpb.usbBaudRate),
			zap.Error(err),
		)
		return err
	}
	mpb.port = port

	amp, err := monopamp.NewSerialAmplifier(port)
	if err != nil {
		mpb.logger.Error("error initializing monoprice amp library",
			zap.String("port_path", mpb.usbPath),
			zap.Error(err),
		)
		return err
	}
	mpb.amp = serialAmp{a: amp}

	// Individual zones failing here isn't fatal: they're published as unreachable, and the
	// refresh loop recovers them if they come back.
	if err := mpb.Refresh(ctx); err != nil {
		mpb.logger.Warn("initial refresh was incomplete", zap.Error(err))
	}
	return nil
}

// Close ensures the underlying serial port is closed
func (mpb *MonopriceAmpBridge) Close() error {
	if mpb.port == nil {
		return nil
	}
	return mpb.port.Close()
}

// speakerToDevice builds the device for a speaker. state may be nil (zone never reported), which
// is rendered as an all-zero, unreachable device. mu must be held.
func (mpb *MonopriceAmpBridge) speakerToDevice(speakerID int, state *monopamp.State) *device.Device {
	speaker := mpb.speakers[speakerID]
	if state == nil {
		state = &monopamp.State{}
	}

	desc := "Monoprice Amplifier Speaker Output"
	balance := int32(state.Balance)

	name := speaker.Name
	if name == "" {
		name = fmt.Sprintf("Speaker %d", speakerID)
	}

	inputs := []*trait.Input_InputDetails{}
	for _, input := range mpb.inputs {
		if !input.Active {
			continue
		}
		inputs = append(inputs, &trait.Input_InputDetails{
			Id:   inputIDToAPI(input.ID),
			Name: input.Name,
		})
	}

	return &device.Device{
		Id:               mpb.speakerIDToDeviceID(speakerID),
		ModelId:          "10761",
		ModelDescription: &desc,
		Manufacturer:     "Monoprice",
		Config: &device.Device_Config{
			Name:        name,
			Description: speaker.Description,
		},
		Address: &device.Device_Address{
			Address:     fmt.Sprintf("%d%d", 1, speakerID),
			IsReachable: speaker.reachable,
		},
		LastSeen: timestamppb.New(speaker.lastSeen),
		Details: &device.Device_AvReceiver{
			AvReceiver: &device.AVReceiver{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{
						CanControl: true,
					},
					State: &trait.OnOff_State{
						IsOn: state.IsOn,
					},
				},
				Volume: &trait.Volume{
					Attributes: &trait.Volume_Attributes{
						CanControl:   true,
						CanMute:      true,
						MaximumLevel: maxVolumeLevel,
					},
					State: &trait.Volume_State{
						IsMuted: state.IsMuteOn,
						Level:   int32(state.Volume),
					},
				},
				Input: &trait.Input{
					Attributes: &trait.Input_Attributes{
						CanControl: true,
						Inputs:     inputs,
						IsOrdered:  true,
					},
					State: &trait.Input_State{
						CurrentInputId: inputIDToAPI(state.SourceChannelID),
					},
				},
				AudioOutput: &trait.AudioOutput{
					Attributes: &trait.AudioOutput_Attributes{
						CanControl: true,
						IsStereo:   true,
					},
					State: &trait.AudioOutput_State{
						TrebleLevel: int32(state.Treble),
						BassLevel:   int32(state.Bass),
						Balance:     &balance,
					},
				},
			},
		},
	}
}

func (mpb *MonopriceAmpBridge) isValidDeviceID(id string) bool {
	return strings.HasPrefix(id, mpb.bridgeID+":")
}

func (mpb *MonopriceAmpBridge) speakerIDToDeviceID(speakerID int) string {
	return fmt.Sprintf("%s:%d", mpb.bridgeID, speakerID)
}

func inputIDToAPI(original int) string {
	return fmt.Sprintf("%d", original)
}

func inputIDFromAPI(original string) int {
	inputID, _ := strconv.Atoi(original)
	return inputID
}
