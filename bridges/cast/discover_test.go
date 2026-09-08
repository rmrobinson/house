package main

import (
	"context"
	"net"
	"testing"

	"github.com/hashicorp/mdns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestDeviceConfigFromEntry(t *testing.T) {
	tests := []struct {
		name  string
		entry *mdns.ServiceEntry
		want  deviceConfig
		ok    bool
	}{
		{
			name: "media player from ca bit 0",
			entry: &mdns.ServiceEntry{
				Name:       "Google-Home-Mini-abc._googlecast._tcp.local.",
				AddrV4:     net.ParseIP("192.168.1.41"),
				InfoFields: []string{"id=abc123", "ca=199172", "fn=Kitchen Home Mini", "md=Google Home Mini"},
			},
			want: deviceConfig{UUID: "abc123", Host: "192.168.1.41", Name: "Kitchen Home Mini", Kind: "media_player"},
			ok:   true,
		},
		{
			name: "television from ca bit 0",
			entry: &mdns.ServiceEntry{
				Name:       "Google-TV-Streamer-def._googlecast._tcp.local.",
				AddrV4:     net.ParseIP("192.168.1.42"),
				InfoFields: []string{"id=def456", "ca=465413", "fn=Living Room TV"},
			},
			want: deviceConfig{UUID: "def456", Host: "192.168.1.42", Name: "Living Room TV", Kind: "television"},
			ok:   true,
		},
		{
			name: "missing id is skipped",
			entry: &mdns.ServiceEntry{
				AddrV4:     net.ParseIP("192.168.1.43"),
				InfoFields: []string{"ca=199172", "fn=No ID"},
			},
			ok: false,
		},
		{
			name: "missing address is skipped",
			entry: &mdns.ServiceEntry{
				InfoFields: []string{"id=ghi789", "ca=199172"},
			},
			ok: false,
		},
		{
			name: "falls back to deprecated Addr when AddrV4 is nil",
			entry: &mdns.ServiceEntry{
				Addr:       net.ParseIP("fe80::1"),
				InfoFields: []string{"id=jkl012", "ca=199172"},
			},
			want: deviceConfig{UUID: "jkl012", Host: "fe80::1", Kind: "media_player"},
			ok:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := deviceConfigFromEntry(tt.entry)
			require.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestParseTXT(t *testing.T) {
	got := parseTXT([]string{"id=abc123", "ca=199172", "no-equals-sign", "fn=My Speaker"})
	assert.Equal(t, map[string]string{"id": "abc123", "ca": "199172", "fn": "My Speaker"}, got)
}

func TestDiscoverDevices(t *testing.T) {
	lookup := func(ctx context.Context, params *mdns.QueryParam) error {
		params.Entries <- &mdns.ServiceEntry{
			Name:       "Google-Home-Mini-abc._googlecast._tcp.local.",
			AddrV4:     net.ParseIP("192.168.1.41"),
			InfoFields: []string{"id=abc123", "ca=199172", "fn=Kitchen Home Mini"},
		}
		// No "id" field - must be silently skipped, not surfaced as an error.
		params.Entries <- &mdns.ServiceEntry{
			Name:       "Cast-Group-xyz._googlecast._tcp.local.",
			AddrV4:     net.ParseIP("192.168.1.44"),
			InfoFields: []string{"md=Google Cast Group"},
		}
		return nil
	}

	got := discoverDevices(context.Background(), zaptest.NewLogger(t), lookup)
	require.Len(t, got, 1)
	assert.Equal(t, deviceConfig{UUID: "abc123", Host: "192.168.1.41", Name: "Kitchen Home Mini", Kind: "media_player"}, got[0])
}

func TestDiscoverDevices_LookupErrorReturnsWhateverWasFoundBeforeIt(t *testing.T) {
	lookup := func(ctx context.Context, params *mdns.QueryParam) error {
		params.Entries <- &mdns.ServiceEntry{
			AddrV4:     net.ParseIP("192.168.1.41"),
			InfoFields: []string{"id=abc123"},
		}
		return assert.AnError
	}

	got := discoverDevices(context.Background(), zaptest.NewLogger(t), lookup)
	require.Len(t, got, 1)
	assert.Equal(t, "abc123", got[0].UUID)
}

func TestMergeDiscovered(t *testing.T) {
	configured := []deviceConfig{
		{UUID: "known", Host: "192.168.1.10", Name: "Configured Name", Kind: "television"},
	}
	discovered := []deviceConfig{
		// Already configured: only Host should be refreshed, Name/Kind must
		// stay whatever the operator configured even though mDNS disagrees.
		{UUID: "known", Host: "192.168.1.99", Name: "mDNS Name", Kind: "media_player"},
		// Not configured: appended as-is.
		{UUID: "new", Host: "192.168.1.20", Name: "New Speaker", Kind: "media_player"},
	}

	got := mergeDiscovered(configured, discovered)

	require.Len(t, got, 2)
	assert.Equal(t, deviceConfig{UUID: "known", Host: "192.168.1.99", Name: "Configured Name", Kind: "television"}, got[0],
		"host refreshed, but name/kind stay config-authoritative")
	assert.Equal(t, deviceConfig{UUID: "new", Host: "192.168.1.20", Name: "New Speaker", Kind: "media_player"}, got[1])
}

func TestMergeDiscovered_NothingDiscoveredLeavesConfiguredUntouched(t *testing.T) {
	configured := []deviceConfig{{UUID: "known", Host: "192.168.1.10"}}
	got := mergeDiscovered(configured, nil)
	assert.Equal(t, configured, got)
}

// TestMergeDiscovered_DoesNotMutateCallersSlice guards against aliasing:
// mergeDiscovered must return an independent slice, not one sharing
// configured's backing array — otherwise refreshing a host in the returned
// value would silently mutate whatever the caller originally passed in.
func TestMergeDiscovered_DoesNotMutateCallersSlice(t *testing.T) {
	configured := []deviceConfig{{UUID: "known", Host: "192.168.1.10"}}
	discovered := []deviceConfig{{UUID: "known", Host: "192.168.1.99"}}

	got := mergeDiscovered(configured, discovered)

	require.Len(t, got, 1)
	assert.Equal(t, "192.168.1.99", got[0].Host)
	assert.Equal(t, "192.168.1.10", configured[0].Host, "the caller's original slice must be untouched")
}
