package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

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

type Charger struct {
	logger *zap.Logger

	baseURL string
	client  *http.Client

	lastRefreshed  time.Time
	cachedVitals   *vitalAPIResponse
	cachedLifetime *lifetimeAPIResponse
	cachedVersion  *versionAPIResponse

	bridge *ChargerBridge
}

func NewCharger(logger *zap.Logger, baseURL string, client *http.Client) *Charger {
	return &Charger{
		logger:  logger,
		baseURL: baseURL,
		client:  client,
	}
}

func (c *Charger) Refresh() error {
	if err := c.refreshCachedValues(); err != nil {
		c.logger.Info("error refreshing cached values", zap.Error(err))
		return err
	}

	c.lastRefreshed = time.Now()

	c.bridge.updateDevice(c.deviceFromCachedState())
	return nil
}

func (c *Charger) getDataFromAPI(path string, apiPayload interface{}) error {
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		c.logger.Error("unable to create http request for vitals", zap.Error(err))
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Info("unable to make api get", zap.String("path", path), zap.Error(err))
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.logger.Info("got non-200 response code from api request, returning error", zap.String("path", path), zap.Int("response_code", resp.StatusCode))
		return errors.New("charger response non-200")
	}

	err = json.NewDecoder(resp.Body).Decode(apiPayload)
	if err != nil {
		c.logger.Info("unable to decode api response body", zap.String("path", path), zap.Error(err))
		return err
	}

	return nil
}

func (c *Charger) refreshCachedValues() error {
	vitals := &vitalAPIResponse{}
	lifetime := &lifetimeAPIResponse{}
	version := &versionAPIResponse{}

	if err := c.getDataFromAPI(fmt.Sprintf("%s/api/1/vitals", c.baseURL), vitals); err != nil {
		c.logger.Error("unable to query vitals api", zap.Error(err))
		return err
	}
	if err := c.getDataFromAPI(fmt.Sprintf("%s/api/1/lifetime", c.baseURL), lifetime); err != nil {
		c.logger.Error("unable to query lifetime api", zap.Error(err))
		return err
	}
	if err := c.getDataFromAPI(fmt.Sprintf("%s/api/1/version", c.baseURL), version); err != nil {
		c.logger.Error("unable to query version api", zap.Error(err))
		return err
	}

	c.cachedVitals = vitals
	c.cachedLifetime = lifetime
	c.cachedVersion = version

	return nil
}

func (c *Charger) deviceFromCachedState() *device.Device {
	if c.cachedVitals == nil || c.cachedVersion == nil || c.cachedLifetime == nil {
		return nil
	}

	modelName := "Tesla Wall Connector"
	var chargingSession *trait.ChargingSession
	if c.cachedVitals.SessionDurationSeconds > 0 {
		chargingSession = &trait.ChargingSession{
			Attributes: &trait.ChargingSession_Attributes{},
			State: &trait.ChargingSession_State{
				DurationS: int32(c.cachedVitals.SessionDurationSeconds),
				EnergyWh:  c.cachedVitals.SessionEnergyWattHours,
			},
		}
	}

	var vehiclePower *trait.Power
	if c.cachedVitals.VehicleConnected {
		vehiclePower = &trait.Power{
			Attributes: &trait.Power_Attributes{},
			State: &trait.Power_State{
				CurrentA: c.cachedVitals.VehicleCurrentAmps,
			},
		}
	}

	return &device.Device{
		Id:           c.cachedVersion.SerialNumber,
		ModelId:      c.cachedVersion.PartNumber,
		Manufacturer: "Tesla",
		ModelName:    &modelName,
		Details: &device.Device_EvCharger{
			EvCharger: &device.EVCharger{
				OnOff: &trait.OnOff{
					Attributes: &trait.OnOff_Attributes{
						CanControl: false,
					},
					State: &trait.OnOff_State{
						IsOn: c.cachedVitals.ContactorClosed,
					},
				},
				WallPower: &trait.Power{
					Attributes: &trait.Power_Attributes{},
					State: &trait.Power_State{
						VoltageV:    c.cachedVitals.GridVoltage,
						FrequencyHz: &c.cachedVitals.GridFrequencyHertz,
					},
				},
				ExteriorConditions: &trait.AirProperties{
					Attributes: &trait.AirProperties_Attributes{},
					State: &trait.AirProperties_State{
						TemperatureC: c.cachedVitals.HandleTemperatureCelsius,
					},
				},
				ChargingSession: chargingSession,
				VehiclePower:    vehiclePower,
			},
		},
	}
}
