package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap/zaptest"
)

func TestRefreshCachedValuesWhileCharging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	charger := NewCharger(logger, srv.URL, &http.Client{})

	if err := charger.refreshCachedValues(); err != nil {
		t.Errorf("unable to refresh cached values; got err %s\n", err.Error())
	} else if charger.cachedVitals == nil || charger.cachedLifetime == nil || charger.cachedVersion == nil {
		t.Error("cached values missing")
	}

	if !charger.cachedVitals.VehicleConnected {
		t.Error("vehicle not showing as connected but should be")
	} else if charger.cachedVitals.SessionDurationSeconds != 1213 {
		t.Errorf("charging session duration incorrect")
	} else if charger.cachedLifetime.EnergyWattHours != 79588 {
		t.Errorf("lifetime watt hours incorrect")
	} else if charger.cachedVersion.PartNumber != "1734412-02-D" {
		t.Errorf("incorrect part number")
	}
}
