package protoreflectutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiDevice "github.com/rmrobinson/house/api/device"
)

func TestOneofMessage(t *testing.T) {
	d := &apiDevice.Device{Details: &apiDevice.Device_Sensor{Sensor: &apiDevice.Sensor{}}}

	msg, name, ok := OneofMessage(d, "details")
	require.True(t, ok)
	assert.Equal(t, "sensor", string(name))
	assert.NotNil(t, msg)

	// No field of the oneof set.
	_, _, ok = OneofMessage(&apiDevice.Device{}, "details")
	assert.False(t, ok)

	// No oneof by that name on this message at all.
	_, _, ok = OneofMessage(d, "not_a_real_oneof")
	assert.False(t, ok)
}
