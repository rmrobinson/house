package policy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestSunTimesUTCEquatorHasRoughlyTwelveHourDays covers the invariant that
// holds everywhere near the equator year-round: sunrise and sunset are
// close to 12 hours apart, whatever the date.
func TestSunTimesUTCEquatorHasRoughlyTwelveHourDays(t *testing.T) {
	for _, date := range []time.Time{
		time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC),
	} {
		sunrise, sunset, solved, _ := sunTimesUTC(date, 0, 0)
		if assert.True(t, solved, "date %v", date) {
			dayLength := sunset.Sub(sunrise)
			assert.InDelta(t, 12*float64(time.Hour), float64(dayLength), float64(15*time.Minute), "date %v", date)
		}
	}
}

// TestSunTimesUTCNorthernHemisphereSeasonalSwing covers the invariant that a
// mid-northern latitude has much longer days in its summer than its winter.
func TestSunTimesUTCNorthernHemisphereSeasonalSwing(t *testing.T) {
	const lat, lon = 43.7, -79.4 // Toronto-ish

	summerSunrise, summerSunset, summerSolved, _ := sunTimesUTC(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), lat, lon)
	winterSunrise, winterSunset, winterSolved, _ := sunTimesUTC(time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC), lat, lon)

	require := func(cond bool, msg string) {
		if !cond {
			t.Fatal(msg)
		}
	}
	require(summerSolved, "expected a solved summer sunrise/sunset")
	require(winterSolved, "expected a solved winter sunrise/sunset")

	summerLength := summerSunset.Sub(summerSunrise)
	winterLength := winterSunset.Sub(winterSunrise)

	assert.Greater(t, summerLength, 14*time.Hour, "summer day should be long")
	assert.Less(t, winterLength, 10*time.Hour, "winter day should be short")
	assert.Greater(t, summerLength, winterLength)
}

// TestSunTimesUTCPolarDayAndNight covers the two "no solution" edges: a high
// latitude has 24-hour daylight around its summer solstice and 24-hour
// darkness around its winter solstice.
func TestSunTimesUTCPolarDayAndNight(t *testing.T) {
	const lat, lon = 78.0, 15.0 // Svalbard-ish, well inside the Arctic Circle

	_, _, solved, alwaysUp := sunTimesUTC(time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC), lat, lon)
	assert.False(t, solved)
	assert.True(t, alwaysUp, "should be polar day at the summer solstice")

	_, _, solved, alwaysUp = sunTimesUTC(time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC), lat, lon)
	assert.False(t, solved)
	assert.False(t, alwaysUp, "should be polar night at the winter solstice")
}

// TestSunTimesUTCSunriseBeforeSunset is a basic sanity check across a
// scattering of latitudes/dates: whenever there's a solution, sunrise must
// precede sunset on the same UTC calendar day.
func TestSunTimesUTCSunriseBeforeSunset(t *testing.T) {
	lats := []float64{-60, -30, 0, 30, 45, 60}
	dates := []time.Time{
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
	}
	for _, lat := range lats {
		for _, date := range dates {
			sunrise, sunset, solved, _ := sunTimesUTC(date, lat, -79.4)
			if solved {
				assert.True(t, sunrise.Before(sunset), "lat=%v date=%v", lat, date)
			}
		}
	}
}
