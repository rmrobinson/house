package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestNewWebOSBridge_PartitionsConfigsByUUID(t *testing.T) {
	configs := []deviceConfig{
		{UUID: "u1", Host: "192.168.1.10", Name: "Living Room"},
		{UUID: "", Host: "192.168.1.11", Name: "Bedroom", MAC: "AA:BB:CC:DD:EE:FF", HasTuner: true},
		{UUID: "", Host: ""}, // no uuid and no host - nothing to key on, dropped
	}

	wb := NewWebOSBridge(zaptest.NewLogger(t), nil, configs)

	require.Len(t, wb.devices, 1)
	assert.Equal(t, "192.168.1.10", wb.devices["u1"].cfg.Host)

	require.Len(t, wb.pending, 1)
	assert.Equal(t, "192.168.1.11", wb.pending[0].Host)
	assert.Equal(t, "Bedroom", wb.pending[0].Name)
}

func TestMergeDiscovered_MatchesPendingEntryByHostAndPromotesIt(t *testing.T) {
	wb := NewWebOSBridge(zaptest.NewLogger(t), nil, []deviceConfig{
		{UUID: "", Host: "192.168.1.11", Name: "Bedroom", MAC: "AA:BB:CC:DD:EE:FF", HasTuner: true},
	})
	require.Len(t, wb.pending, 1)

	wb.mergeDiscovered([]discovered{{UUID: "real-uuid", Host: "192.168.1.11"}})

	assert.Empty(t, wb.pending, "matched pending entry should be removed")
	require.Contains(t, wb.devices, "real-uuid")
	got := wb.devices["real-uuid"].cfg
	assert.Equal(t, "192.168.1.11", got.Host)
	assert.Equal(t, "Bedroom", got.Name, "pre-seeded name must survive the merge")
	assert.Equal(t, "AA:BB:CC:DD:EE:FF", got.MAC, "pre-seeded mac must survive the merge")
	assert.True(t, got.HasTuner, "pre-seeded has_tuner must survive the merge")
}

func TestMergeDiscovered_UnmatchedDeviceStartsWithEmptyConfig(t *testing.T) {
	wb := NewWebOSBridge(zaptest.NewLogger(t), nil, nil)

	wb.mergeDiscovered([]discovered{{UUID: "new-uuid", Host: "192.168.1.99"}})

	require.Contains(t, wb.devices, "new-uuid")
	assert.Equal(t, "192.168.1.99", wb.devices["new-uuid"].cfg.Host)
	assert.Empty(t, wb.devices["new-uuid"].cfg.MAC)
}

// TestLookupDevice_ReturnsSnapshotNotSharedPointer guards against a real
// data race: ProcessCommand/ProcessCommandAsync read the returned
// *webosDevice's session/cfg outside wb.mu, while mergeDiscovered can
// reassign those same fields on wb.devices' shared entry under wb.mu at any
// time (e.g. a host change from SSDP re-discovery). lookupDevice must hand
// back a copy so a concurrent reassignment can never race with a command
// already in flight.
func TestLookupDevice_ReturnsSnapshotNotSharedPointer(t *testing.T) {
	wb := NewWebOSBridge(zaptest.NewLogger(t), nil, []deviceConfig{
		{UUID: "u1", Host: "192.168.1.10"},
	})
	wb.devices["u1"].session = &fakeSession{}

	snapshot, err := wb.lookupDevice("u1")
	require.NoError(t, err)

	// Mutate the shared entry exactly like mergeDiscovered's host-change path
	// would, after the snapshot was handed out.
	wb.devices["u1"].cfg.Host = "192.168.1.99"
	wb.devices["u1"].session = &fakeSession{}

	assert.Equal(t, "192.168.1.10", snapshot.cfg.Host, "snapshot must not observe a later reassignment of the shared entry")
	assert.NotSame(t, wb.devices["u1"].session, snapshot.session, "snapshot must hold its own session reference")
}

func TestDeviceConfigsLocked_IncludesUnmatchedPendingEntries(t *testing.T) {
	wb := NewWebOSBridge(zaptest.NewLogger(t), nil, []deviceConfig{
		{UUID: "u1", Host: "192.168.1.10"},
		{UUID: "", Host: "192.168.1.11", Name: "Bedroom"},
	})

	configs := wb.deviceConfigsLocked()

	// Both the known device and the still-unmatched pending entry must
	// round-trip through a config rewrite - otherwise a persistClientKey
	// call for u1 would silently drop the Bedroom pre-seed before it's
	// ever matched by discovery.
	assert.Len(t, configs, 2)
}
