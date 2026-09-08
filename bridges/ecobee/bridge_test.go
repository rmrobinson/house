package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/rmrobinson/house/api/command"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/bridges/lib/homekitctrl"
	"github.com/rmrobinson/house/service/bridge"
)

func testConfig() ecobeeConfig {
	return ecobeeConfig{
		AccessoryName: "ecobee",
		Thermostat:    houseDeviceConfig{ID: "ecobee-main", Name: "Main Floor Thermostat"},
		Sensors: []sensorConfig{
			{ID: "ecobee-downstairs", Name: "Downstairs", AccessoryID: 4297248826},
		},
	}
}

// seedPairingStore populates a fresh Store with a plausible identity/accessory pairing so
// ensureConnected's precondition checks pass, letting tests exercise the discover/connect steps
// themselves rather than failing before reaching them.
func seedPairingStore(t *testing.T, store homekitctrl.Store, accessoryName string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, store.SaveControllerIdentity(&homekitctrl.ControllerIdentity{
		PairingID: "11111111-1111-1111-1111-111111111111", PublicKey: pub, PrivateKey: priv,
	}))

	accPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, store.SaveAccessory(&homekitctrl.AccessoryRecord{
		Name: accessoryName, PairingID: "AA:BB:CC:DD:EE:FF", PublicKey: accPub,
	}))
}

// testBridge builds an EcobeeBridge wired to a fakeController, bypassing real mDNS
// discovery/pair-verify - see hapController's doc comment for why that's the right seam to fake
// at for this package's own tests.
func testBridge(t *testing.T, cfg ecobeeConfig) (*EcobeeBridge, *fakeController) {
	t.Helper()

	logger := zaptest.NewLogger(t)
	svc := bridge.NewService(logger)
	store := homekitctrl.NewFileStore(t.TempDir() + "/pairing.json")
	seedPairingStore(t, store, cfg.AccessoryName)

	eb := NewEcobeeBridge(logger, svc, store, cfg)
	svc.RegisterHandler(eb, eb.Bridge())

	fc := newFakeController()
	eb.conn.ctrl = fc

	return eb, fc
}

// seedThermostat writes raw values keyed by thermostat characteristic ID into fc, mirroring what
// a real ReadCharacteristics response for aid=thermostatAID would contain.
func seedThermostat(fc *fakeController, raw map[uint64]any) {
	for id, v := range raw {
		fc.set(homekitctrl.CharID{AccessoryID: thermostatAID, CharacteristicID: id}, v)
	}
}

func TestRefreshBuildsDevicesFromController(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	seedThermostat(fc, baselineThermostatValues(t))
	fc.set(homekitctrl.CharID{AccessoryID: 4297248826, CharacteristicID: remoteSensorCharCurrentTemperature}, 23.3)
	fc.set(homekitctrl.CharID{AccessoryID: 4297248826, CharacteristicID: remoteSensorCharBatteryLevel}, 100)

	require.NoError(t, eb.Refresh(context.Background()))

	eb.mu.Lock()
	thermostat := eb.lastThermostat
	sensor := eb.lastSensors["ecobee-downstairs"]
	eb.mu.Unlock()

	require.NotNil(t, thermostat)
	assert.True(t, thermostat.Address.IsReachable)
	assert.Equal(t, trait.Thermostat_COOL, thermostat.GetThermostat().GetThermostat().GetState().CurrentMode)

	require.NotNil(t, sensor)
	assert.True(t, sensor.Address.IsReachable)
	assert.Equal(t, float32(23.3), sensor.GetSensor().AirProperties.State.TemperatureC)
}

func TestRefreshFailsOnIncompleteReadWithoutPublishing(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	raw := baselineThermostatValues(t)
	delete(raw, charCurrentTemperature)
	seedThermostat(fc, raw)

	err := eb.Refresh(context.Background())
	assert.Error(t, err)

	eb.mu.Lock()
	defer eb.mu.Unlock()
	assert.Nil(t, eb.lastThermostat, "an incomplete read should never publish a device built from it")
}

func TestRefreshMarksExistingDevicesUnreachableOnConnectFailure(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	seedThermostat(fc, baselineThermostatValues(t))
	fc.set(homekitctrl.CharID{AccessoryID: 4297248826, CharacteristicID: remoteSensorCharCurrentTemperature}, 23.3)
	require.NoError(t, eb.Refresh(context.Background()))

	// Simulate the connection having dropped and rediscovery failing to find the accessory
	// again - the scenario markAllUnreachable exists for.
	eb.conn.ctrl = nil
	eb.conn.discover = func(ctx context.Context, pairingID string, timeout time.Duration) (*homekitctrl.DiscoveredAccessory, error) {
		return nil, assert.AnError
	}

	err := eb.Refresh(context.Background())
	assert.Error(t, err)

	eb.mu.Lock()
	defer eb.mu.Unlock()
	assert.False(t, eb.lastThermostat.Address.IsReachable)
	assert.False(t, eb.lastSensors["ecobee-downstairs"].Address.IsReachable)
}

func TestEnsureConnectedUsesInjectedDiscoverAndConnect(t *testing.T) {
	eb, _ := testBridge(t, testConfig())
	eb.conn.ctrl = nil // force a real (fake) reconnect rather than returning the cached one

	newFC := newFakeController()
	connectCalled := false
	eb.conn.discover = func(ctx context.Context, pairingID string, timeout time.Duration) (*homekitctrl.DiscoveredAccessory, error) {
		return &homekitctrl.DiscoveredAccessory{Name: "fake", PairingID: pairingID, IPs: []net.IP{net.ParseIP("127.0.0.1")}, Port: 1234}, nil
	}
	eb.conn.connect = func(ctx context.Context, host string, identity *homekitctrl.ControllerIdentity, accessory *homekitctrl.AccessoryRecord) (hapController, error) {
		connectCalled = true
		return newFC, nil
	}

	ctrl, err := eb.conn.ensureConnected(context.Background())
	require.NoError(t, err)
	assert.True(t, connectCalled)
	assert.Same(t, newFC, ctrl)
}

func TestProcessCommandUnknownDeviceReturnsNotFound(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	seedThermostat(fc, baselineThermostatValues(t))
	require.NoError(t, eb.Refresh(context.Background()))

	_, err := eb.ProcessCommand(context.Background(), &command.Command{
		DeviceId: "does-not-exist",
		Details:  &command.Command_SetThermostatMode{SetThermostatMode: &command.SetThermostatMode{Mode: trait.Thermostat_HEAT}},
	})
	assert.ErrorIs(t, err, bridge.ErrDeviceNotFound)
}

func TestProcessCommandSensorDeviceReturnsUnsupported(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	seedThermostat(fc, baselineThermostatValues(t))
	fc.set(homekitctrl.CharID{AccessoryID: 4297248826, CharacteristicID: remoteSensorCharCurrentTemperature}, 23.3)
	require.NoError(t, eb.Refresh(context.Background()))

	_, err := eb.ProcessCommand(context.Background(), &command.Command{
		DeviceId: "ecobee-downstairs",
		Details:  &command.Command_SetThermostatMode{SetThermostatMode: &command.SetThermostatMode{Mode: trait.Thermostat_HEAT}},
	})
	assert.ErrorIs(t, err, bridge.ErrUnsupportedCommand)
}

func TestProcessCommandAppliesWriteAndOptimisticUpdate(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	seedThermostat(fc, baselineThermostatValues(t))
	require.NoError(t, eb.Refresh(context.Background()))

	updated, err := eb.ProcessCommand(context.Background(), &command.Command{
		DeviceId: "ecobee-main",
		Details:  &command.Command_SetThermostatMode{SetThermostatMode: &command.SetThermostatMode{Mode: trait.Thermostat_HEAT}},
	})
	require.NoError(t, err)
	assert.Equal(t, trait.Thermostat_HEAT, updated.GetThermostat().GetThermostat().GetState().TargetMode)

	require.Len(t, fc.writes, 1)
	assert.Equal(t, charTargetHeatingCoolingState, fc.writes[0].CharacteristicID)
	assert.Equal(t, 1, fc.writes[0].Value)
}

func TestProcessCommandWriteFailureForcesReconnect(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	seedThermostat(fc, baselineThermostatValues(t))
	require.NoError(t, eb.Refresh(context.Background()))

	fc.writeErr = assert.AnError

	_, err := eb.ProcessCommand(context.Background(), &command.Command{
		DeviceId: "ecobee-main",
		Details:  &command.Command_SetThermostatMode{SetThermostatMode: &command.SetThermostatMode{Mode: trait.Thermostat_HEAT}},
	})
	assert.Error(t, err)

	eb.conn.mu.Lock()
	defer eb.conn.mu.Unlock()
	assert.Nil(t, eb.conn.ctrl, "a write failure should force the next call to reconnect from scratch")
	assert.True(t, fc.closed)
}

// TestProcessCommandSetTemperatureUsesLiveModeNotStaleCache is the regression test for the
// staleness fix in ProcessCommand: SetTemperature must route against the accessory's *current*
// mode, not whatever the last poll happened to see.
func TestProcessCommandSetTemperatureUsesLiveModeNotStaleCache(t *testing.T) {
	eb, fc := testBridge(t, testConfig())
	raw := baselineThermostatValues(t)
	raw[charTargetHeatingCoolingState] = 2 // COOL as of the last poll
	seedThermostat(fc, raw)
	require.NoError(t, eb.Refresh(context.Background()))

	// The thermostat's mode changed directly on the unit (or via another controller) since that
	// poll, without the bridge having re-polled yet.
	fc.set(homekitctrl.CharID{AccessoryID: thermostatAID, CharacteristicID: charTargetHeatingCoolingState}, 1) // HEAT, live

	updated, err := eb.ProcessCommand(context.Background(), &command.Command{
		DeviceId: "ecobee-main",
		Details:  &command.Command_SetTemperature{SetTemperature: &command.SetTemperature{SetpointCelsius: 22}},
	})
	require.NoError(t, err)

	state := updated.GetThermostat().GetThermostat().GetState()
	require.NotNil(t, state.HeatSetpointCelsius, "should route to the heat setpoint per the live HEAT mode, not the stale cached COOL")
	assert.Nil(t, state.CoolSetpointCelsius)
	assert.Equal(t, float32(22), *state.HeatSetpointCelsius)
}
