package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

func TestRefreshCachedValuesWhileCharging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")
		if r.URL.Path == "/api/1/vitals" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"contactor_closed":true,"vehicle_connected":true,"session_s":1213,"grid_v":240.8,"grid_hz":59.685,"vehicle_current_a":47.0,"currentA_a":0.0,"currentB_a":47.0,"currentC_a":0.0,"currentN_a":0.0,"voltageA_v":115.2,"voltageB_v":238.6,"voltageC_v":115.3,"relay_coil_v":5.8,"pcba_temp_c":32.6,"handle_temp_c":20.3,"mcu_temp_c":29.1,"uptime_s":200835,"input_thermopile_uv":-692,"prox_v":1.5,"pilot_high_v":5.9,"pilot_low_v":-11.9,"session_energy_wh":3654.600,"config_status":4,"evse_state":10,"current_alerts":[]}`))
		} else if r.URL.Path == "/api/1/lifetime" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"contactor_cycles":4,"contactor_cycles_loaded":2,"alert_count":8,"thermal_foldbacks":0,"avg_startup_temp":0.0,"charge_starts":4,"energy_wh":79588,"connector_cycles":2,"uptime_s":344373,"charging_time_s":9903}`))
		} else if r.URL.Path == "/api/1/version" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"firmware_version":"24.36.3+g3f585e7b51cc72","git_branch":"HEAD","part_number":"1734412-02-D","serial_number":"1234ASDF"}`))
		}
	}))
	defer srv.Close()

	logger := zaptest.NewLogger(t)
	charger := NewCharger(logger, strings.TrimPrefix(srv.URL, "http://"), &http.Client{})

	state, err := charger.State()
	if err != nil {
		t.Errorf("unable to refresh cached values; got err %s\n", err.Error())
	} else if state.vitals == nil || state.lifetime == nil || state.version == nil {
		t.Error("values missing")
	}

	if !state.vitals.VehicleConnected {
		t.Error("vehicle not showing as connected but should be")
	} else if state.vitals.SessionDurationSeconds != 1213 {
		t.Errorf("charging session duration incorrect")
	} else if state.lifetime.EnergyWattHours != 79588 {
		t.Errorf("lifetime watt hours incorrect")
	} else if state.version.PartNumber != "1734412-02-D" {
		t.Errorf("incorrect part number")
	}
}

func TestToDevice_WallPowerIsVoltageTimesCurrent(t *testing.T) {
	for name, tc := range map[string]struct {
		vitals vitalAPIResponse
		wantW  float64
	}{
		// The payload from TestRefreshCachedValuesWhileCharging: a split-phase install, with the
		// whole 47A on currentB_a. 3654.6Wh over 1213s averages ~10.9kW while ramping up, so a
		// 11.3kW instantaneous figure is consistent.
		"charging on one phase": {
			vitals: vitalAPIResponse{GridVoltage: 240.8, CurrentBAmps: 47},
			wantW:  240.8 * 47,
		},
		// Current split across phases is summed, not double counted or dropped.
		"current across phases": {
			vitals: vitalAPIResponse{GridVoltage: 240, CurrentAAmps: 10, CurrentBAmps: 12, CurrentCAmps: 8},
			wantW:  240 * 30,
		},
		// The neutral return isn't extra draw.
		"neutral excluded": {
			vitals: vitalAPIResponse{GridVoltage: 240, CurrentBAmps: 20, CurrentNAmps: 20},
			wantW:  240 * 20,
		},
		"idle standby draw": {
			vitals: vitalAPIResponse{GridVoltage: 245.9, CurrentBAmps: 0.2},
			wantW:  245.9 * 0.2,
		},
		"no current": {
			vitals: vitalAPIResponse{GridVoltage: 245.9},
			wantW:  0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cs := &ChargerState{
				vitals:   &tc.vitals,
				lifetime: &lifetimeAPIResponse{},
				version:  &versionAPIResponse{SerialNumber: "1234ASDF"},
			}

			wall := cs.toDevice().GetEvCharger().GetWallPower().GetState()
			assert.InDelta(t, tc.wantW, wall.GetPowerW(), 0.001)
			// Voltage and current are still reported alongside the derived power.
			assert.Equal(t, tc.vitals.GridVoltage, wall.GetVoltageV())
		})
	}
}
