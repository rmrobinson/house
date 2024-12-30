package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/spf13/viper"
	"github.com/tarm/serial"

	monopamp "github.com/rmrobinson/monoprice-amp-go"

	api2 "github.com/rmrobinson/house/api"
	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

const (
	maxZoneID            = 6
	maxChannelID         = 6
	commandSpaceInterval = time.Millisecond * 100
)

var (
	// ErrSpeakerRefreshFailed is returned if the bridge wasn't able to refresh the speaker state
	ErrSpeakerRefreshFailed = status.Error(codes.Internal, "speaker refresh failed")
	// ErrSpeakerCommandFailed is returne if the bridge failed to execute the specified command
	ErrSpeakerCommandFailed = status.Error(codes.Internal, "speaker command failed")
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

	lastSeen time.Time
}

// MonopriceAmpBridge is a bridge to the Monoprice amplifier.
type MonopriceAmpBridge struct {
	logger *zap.Logger
	svc    *bridge.Service
	b      *api2.Bridge

	bridgeID    string
	usbPath     string
	usbBaudRate int

	inputs   []inputDetails
	speakers map[int]speakerDetails

	port *serial.Port
	amp  *monopamp.SerialAmplifier
}

// NewMonopriceAmpBridge creates a new charger bridge
func NewMonopriceAmpBridge(logger *zap.Logger, svc *bridge.Service, usbPath string, usbBaudRate int, inputs []inputDetails, speakers []speakerDetails) *MonopriceAmpBridge {
	bridgeID := viper.GetString("bridge.name")
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

	mpb := &MonopriceAmpBridge{
		logger:      logger,
		svc:         svc,
		b:           b,
		bridgeID:    bridgeID,
		usbPath:     usbPath,
		usbBaudRate: usbBaudRate,
		inputs:      inputs,
		speakers:    speakersMap,
	}

	return mpb
}

// ProcessCommand takes a given command request and attempts to execute it.
// We only worry about processing valid commands for the given device traits.
func (mpb *MonopriceAmpBridge) ProcessCommand(ctx context.Context, cmd *command.Command) (*device.Device, error) {
	if !mpb.isValidDeviceID(cmd.DeviceId) {
		mpb.logger.Info("received command for invalid device id",
			zap.String("device_id", cmd.DeviceId))

		return nil, bridge.ErrDeviceNotFound
	}

	speakerID := mpb.deviceIDToSpeakerID(cmd.DeviceId)
	zone := mpb.amp.Zone(speakerID)

	// The amp supports the OnOff, Volume, Input and AudioOutput commands.
	if cmd.GetOnOff() != nil {
		if err := zone.SetPower(cmd.GetOnOff().GetOn()); err != nil {
			mpb.logger.Error("unable to set power",
				zap.String("device_id", cmd.DeviceId),
				zap.Error(err))
			return nil, ErrSpeakerCommandFailed
		}
	} else if cmd.GetVolume() != nil {
		if err := zone.SetVolume(volumeFromAPI(cmd.GetVolume().Volume)); err != nil {
			mpb.logger.Error("unable to set volume",
				zap.String("device_id", cmd.DeviceId),
				zap.Int32("volume", cmd.GetVolume().Volume),
				zap.Error(err))
			return nil, ErrSpeakerCommandFailed
		}
	} else if cmd.GetInput() != nil {
		if err := zone.SetSourceChannel(inputIDFromAPI(cmd.GetInput().InputId)); err != nil {
			mpb.logger.Error("unable to set input",
				zap.String("input_id", cmd.GetInput().InputId),
				zap.Error(err))
			return nil, ErrSpeakerCommandFailed
		}
	} else if cmd.GetAudioOutput() != nil {
		if cmd.GetAudioOutput().TrebleLevel != nil {
			if err := zone.SetTreble(noteFromAPI(*cmd.GetAudioOutput().TrebleLevel)); err != nil {
				mpb.logger.Error("unable to set treble",
					zap.Int32("trebel_level", *cmd.GetAudioOutput().TrebleLevel),
					zap.Error(err))
			}
			time.Sleep(commandSpaceInterval)
		}
		if cmd.GetAudioOutput().BassLevel != nil {
			if err := zone.SetBass(noteFromAPI(*cmd.GetAudioOutput().BassLevel)); err != nil {
				mpb.logger.Error("unable to set bass",
					zap.Int32("bass_level", *cmd.GetAudioOutput().BassLevel),
					zap.Error(err))
			}
			time.Sleep(commandSpaceInterval)
		}
		if cmd.GetAudioOutput().Balance != nil {
			if err := zone.SetBalance(balanceFromAPI(*cmd.GetAudioOutput().Balance)); err != nil {
				mpb.logger.Error("unable to set balance",
					zap.Int32("balance", *cmd.GetAudioOutput().Balance),
					zap.Error(err))
			}
		}
	} else {
		mpb.logger.Error("received unsupported command - shouldn't happen")
		return nil, bridge.ErrUnsupportedCommand
	}

	if speaker, found := mpb.speakers[speakerID]; found {
		speaker.lastSeen = time.Now()
		mpb.speakers[speakerID] = speaker
	}

	return mpb.speakerToDevice(speakerID, zone), nil
}

// SetBridgeConfig takes the supplied config params and saves them for future reference.
func (mpb *MonopriceAmpBridge) SetBridgeConfig(ctx context.Context, config bridge.Config) error {
	mpb.b.Config.Name = config.Name
	mpb.b.Config.Description = config.Description

	viper.Set("bridge.name", config.Name)
	viper.Set("bridge.description", config.Description)
	viper.WriteConfig()

	return nil
}

// Refresh is present to conform to the bridge.Handler interface. In this implementation the zones
// are refreshed and the devices are updated.
func (mpb *MonopriceAmpBridge) Refresh(ctx context.Context) error {
	for _, speaker := range mpb.speakers {
		if !speaker.Active {
			continue
		}

		zone := mpb.amp.Zone(speaker.ID)
		if err := zone.Refresh(); err != nil {
			mpb.logger.Error("unable to refresh speaker zone",
				zap.Error(err))
			return ErrSpeakerRefreshFailed
		}

		mpb.svc.UpdateDevice(mpb.speakerToDevice(speaker.ID, zone))
	}
	return nil
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
	mpb.amp = amp

	return mpb.Refresh(ctx)
}

// Close ensures the underlying serial port is closed
func (mpb *MonopriceAmpBridge) Close() error {
	if mpb.port == nil {
		return nil
	}
	return mpb.port.Close()
}

func (mpb *MonopriceAmpBridge) speakerToDevice(speakerID int, zone *monopamp.Zone) *device.Device {
	speaker := mpb.speakers[speakerID]

	desc := "Monoprice Amplifier Speaker Output"
	balance := balanceToAPI(zone.State().Balance)

	inputs := []*trait.Input_InputDetails{}
	for _, input := range mpb.inputs {
		inputs = append(inputs, &trait.Input_InputDetails{
			Id:   inputIDToAPI(input.ID),
			Name: input.Name,
		})
	}

	// TODO: convert volume and treble/bass levels to a normalized 1-100 value

	return &device.Device{
		Id:               mpb.speakerIDToDeviceID(speakerID),
		ModelId:          "10761",
		ModelDescription: &desc,
		Manufacturer:     "Monoprice",
		Config: &device.Device_Config{
			Name: fmt.Sprintf("Speaker %d", zone.ID()),
		},
		Address: &device.Device_Address{
			Address: zone.ID(),
		},
		LastSeen: timestamppb.New(speaker.lastSeen),
		Details: &device.Device_AvReceiver{
			AvReceiver: &device.AVReceiver{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{
						CanControl: true,
					},
					State: &trait.OnOff_State{
						IsOn: zone.State().IsOn,
					},
				},
				Volume: &trait.Volume{
					Attributes: &trait.Volume_Attributes{
						CanControl:   true,
						CanMute:      true,
						MaximumLevel: 100,
					},
					State: &trait.Volume_State{
						IsMuted: zone.State().IsMuteOn,
						Level:   volumeToAPI(zone.State().Volume),
					},
				},
				Input: &trait.Input{
					Attributes: &trait.Input_Attributes{
						CanControl: true,
						Inputs:     inputs,
						IsOrdered:  true,
					},
					State: &trait.Input_State{
						CurrentInputId: fmt.Sprintf("%d", zone.State().SourceChannelID),
					},
				},
				AudioOutput: &trait.AudioOutput{
					Attributes: &trait.AudioOutput_Attributes{
						CanControl: true,
					},
					State: &trait.AudioOutput_State{
						TrebleLevel: noteToAPI(zone.State().Treble),
						BassLevel:   noteToAPI(zone.State().Bass),
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

func (mpb *MonopriceAmpBridge) deviceIDToSpeakerID(deviceID string) int {
	speakerIDStr := strings.TrimPrefix(deviceID, fmt.Sprintf("%s:", mpb.bridgeID))
	speakerID, _ := strconv.Atoi(speakerIDStr)
	return speakerID
}

func inputIDToAPI(original int) string {
	return fmt.Sprintf("%d", original)
}

func inputIDFromAPI(original string) int {
	inputID, _ := strconv.Atoi(original)
	return inputID
}

func volumeFromAPI(original int32) int {
	return int((original * 38) / 100)
}

func volumeToAPI(original int) int32 {
	return int32((original * 100) / 38)
}

func balanceFromAPI(original int32) int {
	return int(original / 5)
}

func balanceToAPI(original int) int32 {
	return int32((original * 5) / 20)
}

func noteFromAPI(original int32) int {
	return int((original * 14) / 100)
}

func noteToAPI(original int) int32 {
	return int32((original * 100) / 14)
}
