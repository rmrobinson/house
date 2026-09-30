package facade

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// resetViper isolates a test from viper's global config state, which
// LoadConfig reads from directly.
func resetViper(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
}

func TestLoadConfig_NoFacadeBridgesReturnsNilConfig(t *testing.T) {
	resetViper(t)

	cfg, err := LoadConfig(zaptest.NewLogger(t), 1337, filepath.Join(t.TempDir(), "bridgefacade.yaml"))
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadConfig_MissingHostIsError(t *testing.T) {
	resetViper(t)
	viper.Set("bridge.id", "test-id")
	viper.Set("facade.bridges", []map[string]string{{"address": "192.168.1.50:17010"}})

	_, err := LoadConfig(zaptest.NewLogger(t), 1337, filepath.Join(t.TempDir(), "bridgefacade.yaml"))
	assert.Error(t, err)
}

func TestLoadConfig_RejectsSelfReferentialUpstream(t *testing.T) {
	resetViper(t)
	viper.Set("bridge.id", "test-id")
	viper.Set("bridge.host", "192.168.1.5")
	viper.Set("facade.bridges", []map[string]string{
		{"address": "192.168.1.50:17010"},
		{"address": "192.168.1.5:1337"},
	})

	_, err := LoadConfig(zaptest.NewLogger(t), 1337, filepath.Join(t.TempDir(), "bridgefacade.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "own address")
}

func TestLoadConfig_Success(t *testing.T) {
	resetViper(t)
	viper.Set("bridge.id", "test-id")
	viper.Set("bridge.host", "192.168.1.5")
	viper.Set("bridge.name", "Home Facade")
	viper.Set("facade.bridges", []map[string]string{
		{"address": "192.168.1.50:17010", "server_name": "esphome.example.house.internal"},
	})

	cfg, err := LoadConfig(zaptest.NewLogger(t), 1337, filepath.Join(t.TempDir(), "bridgefacade.yaml"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "test-id", cfg.BridgeID)
	assert.Equal(t, "Home Facade", cfg.BridgeName)
	assert.Equal(t, "192.168.1.5:1337", cfg.SelfAddress)
	assert.Equal(t, []UpstreamBridge{{Address: "192.168.1.50:17010", ServerName: "esphome.example.house.internal"}}, cfg.UpstreamBridges)
}

func TestLoadConfig_GeneratesAndPersistsMissingBridgeID(t *testing.T) {
	resetViper(t)
	viper.Set("bridge.host", "192.168.1.5")
	viper.Set("facade.bridges", []map[string]string{{"address": "192.168.1.50:17010"}})

	configPath := filepath.Join(t.TempDir(), "bridgefacade.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("bridge:\n  host: \"192.168.1.5\"\n"), 0o644))

	cfg, err := LoadConfig(zaptest.NewLogger(t), 1337, configPath)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, cfg.BridgeID)

	persisted, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(persisted), cfg.BridgeID, "LoadConfig must persist the generated bridge.id back to configPath")
}
