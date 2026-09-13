package facade

import (
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

	cfg, err := LoadConfig(zaptest.NewLogger(t))
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadConfig_MissingSelfAddressIsError(t *testing.T) {
	resetViper(t)
	viper.Set("bridge.id", "test-id")
	viper.Set("facade.bridges", []string{"192.168.1.50:17010"})

	_, err := LoadConfig(zaptest.NewLogger(t))
	assert.Error(t, err)
}

func TestLoadConfig_RejectsSelfReferentialUpstream(t *testing.T) {
	resetViper(t)
	viper.Set("bridge.id", "test-id")
	viper.Set("bridge.address", "192.168.1.5:1337")
	viper.Set("facade.bridges", []string{"192.168.1.50:17010", "192.168.1.5:1337"})

	_, err := LoadConfig(zaptest.NewLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "own address")
}

func TestLoadConfig_Success(t *testing.T) {
	resetViper(t)
	viper.Set("bridge.id", "test-id")
	viper.Set("bridge.address", "192.168.1.5:1337")
	viper.Set("bridge.name", "Home Facade")
	viper.Set("facade.bridges", []string{"192.168.1.50:17010"})

	cfg, err := LoadConfig(zaptest.NewLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "test-id", cfg.BridgeID)
	assert.Equal(t, "Home Facade", cfg.BridgeName)
	assert.Equal(t, "192.168.1.5:1337", cfg.SelfAddress)
	assert.Equal(t, []string{"192.168.1.50:17010"}, cfg.UpstreamAddrs)
}
