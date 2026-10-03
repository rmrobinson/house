package houseview

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"

	api2 "github.com/rmrobinson/house/api"
	apiTrait "github.com/rmrobinson/house/api/trait"
)

func TestPropertiesToView(t *testing.T) {
	// nil Properties (no linked Sensor has reported anything yet) is every
	// field "" - room.html shows that as "Unknown".
	assert.Equal(t, Properties{}, PropertiesToView(nil))

	occupied := true
	tempC := 22.0
	lux := int32(150)
	aqi := int32(42)
	co2 := int32(700)
	voc := int32(120)
	radon := int32(40)
	powerW := 12.3
	pv := PropertiesToView(&api2.Room_Properties{
		Occupied:        &occupied,
		TemperatureC:    &tempC,
		LightLevelLux:   &lux,
		AirQualityIndex: &aqi,
		Co2Ppm:          &co2,
		VocPpb:          &voc,
		RadonBqM3:       &radon,
		PowerDrawW:      &powerW,
	})
	assert.Equal(t, "Yes", pv.Occupied)
	assert.Equal(t, "22.0°C", pv.TemperatureC)
	assert.Equal(t, "150 lux", pv.LightLevelLux)
	assert.Equal(t, "42", pv.AirQualityIndex)
	assert.Equal(t, "700 ppm", pv.Co2Ppm)
	assert.Equal(t, "120 ppb", pv.VocPpb)
	assert.Equal(t, "40 Bq/m³", pv.RadonBqM3)
	assert.Equal(t, "12.3 W", pv.PowerDrawW)

	// A metric with no sensor contributing to it - False is distinguished
	// from "unset" the same way, but only via the pointer itself.
	notOccupied := false
	pv = PropertiesToView(&api2.Room_Properties{Occupied: &notOccupied})
	assert.Equal(t, "No", pv.Occupied)
	assert.Equal(t, "", pv.TemperatureC)
}

func TestMediaSummary(t *testing.T) {
	assert.Equal(t, "", MediaSummary(nil, ""))
	assert.Equal(t, "", MediaSummary(&apiTrait.Media{}, ""))
	assert.Equal(t, "", MediaSummary(&apiTrait.Media{}, "Plex"))

	song := &apiTrait.Media{State: &apiTrait.Media_State{
		PlaybackState: apiTrait.Media_PS_PLAYING,
		MediaType:     apiTrait.Media_TYPE_SONG,
		SongDetails: &apiTrait.Media_SongDetails{
			SongName: "Bohemian Rhapsody",
			Artists:  []string{"Queen"},
		},
	}}
	assert.Equal(t, "Bohemian Rhapsody — Queen", MediaSummary(song, ""))

	paused := &apiTrait.Media{State: &apiTrait.Media_State{
		PlaybackState: apiTrait.Media_PS_PAUSED,
		MediaType:     apiTrait.Media_TYPE_SONG,
		SongDetails:   &apiTrait.Media_SongDetails{SongName: "Bohemian Rhapsody"},
	}}
	assert.Equal(t, "Paused — Bohemian Rhapsody", MediaSummary(paused, ""))

	// Stopped/completed media carries no visible summary even if stale
	// details are still sitting in State from the last active session.
	stopped := &apiTrait.Media{State: &apiTrait.Media_State{
		PlaybackState: apiTrait.Media_PS_STOPPED,
		MediaType:     apiTrait.Media_TYPE_SONG,
		SongDetails:   &apiTrait.Media_SongDetails{SongName: "Bohemian Rhapsody"},
	}}
	assert.Equal(t, "", MediaSummary(stopped, ""))

	show := &apiTrait.Media{State: &apiTrait.Media_State{
		PlaybackState: apiTrait.Media_PS_PLAYING,
		MediaType:     apiTrait.Media_TYPE_SHOW,
		ShowDetails: &apiTrait.Media_ShowDetails{
			ShowTitle:    "Stranger Things",
			EpisodeTitle: "Chapter One",
		},
	}}
	assert.Equal(t, "Stranger Things — Chapter One", MediaSummary(show, ""))

	// Confirmed live against a real Google TV Streamer running Plex's
	// Android TV app: CASTV2 reports metadataType MOVIE with artwork but no
	// title - appName is the only identifiable text available in that case.
	movieNoTitle := &apiTrait.Media{State: &apiTrait.Media_State{
		PlaybackState: apiTrait.Media_PS_PLAYING,
		MediaType:     apiTrait.Media_TYPE_MOVIE,
		MovieDetails:  &apiTrait.Media_MovieDetails{ArtUrl: proto.String("https://example.com/art.jpg")},
	}}
	assert.Equal(t, "", MediaSummary(movieNoTitle, ""))
	assert.Equal(t, "Plex", MediaSummary(movieNoTitle, "Plex"))

	movieWithTitle := &apiTrait.Media{State: &apiTrait.Media_State{
		PlaybackState: apiTrait.Media_PS_PLAYING,
		MediaType:     apiTrait.Media_TYPE_MOVIE,
		MovieDetails:  &apiTrait.Media_MovieDetails{Title: "Inception"},
	}}
	assert.Equal(t, "Inception", MediaSummary(movieWithTitle, "Plex"))
}

func TestPropertiesToView_BatteryRuntime(t *testing.T) {
	for mins, want := range map[int32]string{0: "0 min", 42: "42 min", 60: "1h 0m", 125: "2h 5m"} {
		m := mins
		assert.Equal(t, want, PropertiesToView(&api2.Room_Properties{BatteryRuntimeMins: &m}).BatteryRuntime)
	}
	assert.Equal(t, "", PropertiesToView(&api2.Room_Properties{}).BatteryRuntime)
}
