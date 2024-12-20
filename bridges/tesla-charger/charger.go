package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
)

// TODO: <IP>/api/1/wifi_status

// returned from <IP>/api/1/vitals
type vitalAPIResponse struct {
	ContactorClosed          bool    `json:"contactor_closed"`
	VehicleConnected         bool    `json:"vehicle_connected"`
	SessionDurationSeconds   int     `json:"session_s"`
	GridVoltage              float64 `json:"grid_v"`
	GridFrequencyHertz       float64 `json:"grid_hz"`
	VehicleCurrentAmps       float64 `json:"vehicle_current_a"`
	CurrentAAmps             float64 `json:"currentA_a"`
	CurrentBAmps             float64 `json:"currentB_a"`
	CurrentCAmps             float64 `json:"currentC_a"`
	CurrentNAmps             float64 `json:"currentN_a"`
	VoltageAAmps             float64 `json:"voltageA_v"`
	VoltageBAmps             float64 `json:"voltageB_v"`
	VoltageCAmps             float64 `json:"voltageC_v"`
	RelayCoilVolts           float64 `json:"relay_coil_v"`
	PCBATemperatureCelsius   float32 `json:"pcba_temp_c"`
	HandleTemperatureCelsius float32 `json:"handle_temp_c"`
	MCUTemperatureCelsius    float32 `json:"mcu_temp_c"`
	UptimeSeconds            int     `json:"uptime_s"`
	InputTermopileUV         int     `json:"input_thermopile_uv"`
	ProxVoltage              float64 `json:"prox_v"`
	PilotHighVoltage         float64 `json:"pilot_high_v"`
	PilotLowVoltage          float64 `json:"pilot_low_v"`
	SessionEnergyWattHours   float64 `json:"session_energy_wh"`
	ConfigStatus             int     `json:"config_status"`
	EVSEState                int     `json:"evse_state"`
	CurrentAlerts            []int   `json:"current_alerts"`
	EVSENotReadyReasons      []int   `json:"evse_not_ready_reasons"`
}

// returned from <IP>/api/1/lifetime
type lifetimeAPIResponse struct {
	ContactorCycles           int     `json:"contactor_cycles"`
	ContactorCyclesLoaded     int     `json:"contactor_cycles_loaded"`
	AlertCount                int     `json:"alert_count"`
	ThermalFoldbacks          int     `json:"thermal_foldbacks"`
	AverageStartupTemperature float64 `json:"avg_startup_temp"`
	ChargeStarts              int     `json:"charge_starts"`
	EnergyWattHours           int     `json:"energy_wh"`
	ConnectorCycles           int     `json:"connector_cycles"`
	UptimeSeconds             int     `json:"uptime_s"`
	ChargingTimeSeconds       int     `json:"charging_time_s"`
}

// returned from <IP>/api/1/version
type versionAPIResponse struct {
	FirmwareVersion string `json:"firmware_version"`
	GitBranch       string `json:"git_branch"`
	PartNumber      string `json:"part_number"`
	SerialNumber    string `json:"serial_number"`
}

// ChargerState contains the snapshot of the state of the charger at a specified point in time.
type ChargerState struct {
	vitals   *vitalAPIResponse
	lifetime *lifetimeAPIResponse
	version  *versionAPIResponse

	retrievedAt time.Time
}

func (cs *ChargerState) toDevice() *device.Device {
	if cs.vitals == nil || cs.version == nil || cs.lifetime == nil {
		return nil
	}

	modelName := "Tesla Wall Connector"
	var chargingSession *trait.ChargingSession
	if cs.vitals.SessionDurationSeconds > 0 {
		chargingSession = &trait.ChargingSession{
			Attributes: &trait.ChargingSession_Attributes{},
			State: &trait.ChargingSession_State{
				DurationS: int32(cs.vitals.SessionDurationSeconds),
				EnergyWh:  cs.vitals.SessionEnergyWattHours,
			},
		}
	}

	var vehiclePower *trait.Power
	if cs.vitals.VehicleConnected {
		vehiclePower = &trait.Power{
			Attributes: &trait.Power_Attributes{},
			State: &trait.Power_State{
				CurrentA: cs.vitals.VehicleCurrentAmps,
			},
		}
	}

	return &device.Device{
		Id:           cs.version.SerialNumber,
		ModelId:      cs.version.PartNumber,
		Manufacturer: "Tesla",
		ModelName:    &modelName,
		LastSeen:     timestamppb.New(cs.retrievedAt),
		Details: &device.Device_EvCharger{
			EvCharger: &device.EVCharger{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{
						CanControl: false,
					},
					State: &trait.OnOff_State{
						IsOn: cs.vitals.ContactorClosed,
					},
				},
				WallPower: &trait.Power{
					Attributes: &trait.Power_Attributes{},
					State: &trait.Power_State{
						VoltageV:    cs.vitals.GridVoltage,
						FrequencyHz: &cs.vitals.GridFrequencyHertz,
					},
				},
				ExteriorConditions: &trait.AirProperties{
					Attributes: &trait.AirProperties_Attributes{},
					State: &trait.AirProperties_State{
						TemperatureC: cs.vitals.HandleTemperatureCelsius,
					},
				},
				ChargingSession: chargingSession,
				VehiclePower:    vehiclePower,
			},
		},
	}
}

// Charger provides access to the Tesla charger through its REST API.
type Charger struct {
	logger *zap.Logger

	ipAddr string
	client *http.Client
}

// NewCharger creates a new charger using the supplied IP for connectivity.
func NewCharger(logger *zap.Logger, chargerIP string, client *http.Client) *Charger {
	return &Charger{
		logger: logger,
		ipAddr: chargerIP,
		client: client,
	}
}

func (c *Charger) getDataFromAPI(path string, apiPayload interface{}) error {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s%s", c.ipAddr, path), nil)
	if err != nil {
		c.logger.Error("unable to create http request for vitals",
			zap.Error(err))
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Info("unable to make api get",
			zap.String("charger_ip", c.ipAddr),
			zap.String("path", path),
			zap.Error(err))
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.logger.Error("got non-200 response code from api request, returning error",
			zap.String("charger_ip", c.ipAddr),
			zap.String("path", path),
			zap.Int("response_code", resp.StatusCode))
		return errors.New("charger response non-200")
	} else if resp.Header.Get("Content-Type") != "application/json" {
		c.logger.Error("got non-json content type from api request, returning error",
			zap.String("charger_ip", c.ipAddr),
			zap.String("path", path),
			zap.String("content_type", resp.Header.Get("Content-Type")))
		return errors.New("charger response non-json")
	}

	err = json.NewDecoder(resp.Body).Decode(apiPayload)
	if err != nil {
		c.logger.Info("unable to decode api response body",
			zap.String("charger_ip", c.ipAddr),
			zap.String("path", path),
			zap.Error(err))
		return err
	}

	return nil
}

// State queries the charger and returns its current state.
func (c *Charger) State() (*ChargerState, error) {
	vitals := &vitalAPIResponse{}
	lifetime := &lifetimeAPIResponse{}
	version := &versionAPIResponse{}

	if err := c.getDataFromAPI("/api/1/vitals", vitals); err != nil {
		c.logger.Error("unable to query vitals api", zap.Error(err))
		return nil, err
	}
	if err := c.getDataFromAPI("/api/1/lifetime", lifetime); err != nil {
		c.logger.Error("unable to query lifetime api", zap.Error(err))
		return nil, err
	}
	if err := c.getDataFromAPI("/api/1/version", version); err != nil {
		c.logger.Error("unable to query version api", zap.Error(err))
		return nil, err
	}

	return &ChargerState{
		vitals:      vitals,
		lifetime:    lifetime,
		version:     version,
		retrievedAt: time.Now(),
	}, nil
}
