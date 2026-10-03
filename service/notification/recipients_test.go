package notification

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDirectoryLookup(t *testing.T) {
	d := NewDirectory([]Recipient{
		{ID: "r", Name: "Robert", Channels: []Channel{{Type: "email", Address: "r@faltung.ca"}}},
	})

	got, ok := d.Lookup("r")
	assert.True(t, ok)
	assert.Equal(t, "Robert", got.Name)
	assert.Equal(t, []Channel{{Type: "email", Address: "r@faltung.ca"}}, got.Channels)

	_, ok = d.Lookup("nobody")
	assert.False(t, ok)
}

func TestDirectoryLookupDuplicateIDKeepsLastEntry(t *testing.T) {
	d := NewDirectory([]Recipient{
		{ID: "r", Name: "first"},
		{ID: "r", Name: "second"},
	})

	got, ok := d.Lookup("r")
	assert.True(t, ok)
	assert.Equal(t, "second", got.Name)
}
