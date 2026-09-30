package policy

import (
	"math"
	"time"
)

// sunTimesUTC computes the sunrise and sunset instants, in UTC, for the
// calendar day dateUTC (only its year/month/day matter) at an observer
// located at latDeg/lonDeg (degrees, positive north/east), using NOAA's
// low-precision solar position algorithm (accurate to roughly a minute -
// plenty for scheduling a light, not for a sundial).
//
// solved is false if the sun does not both rise and set that day at that
// latitude (the polar day/night case): sunrise/sunset are then zero, and
// alwaysUp says whether the sun stays above (true) or below (false) the
// horizon for the entire day.
func sunTimesUTC(dateUTC time.Time, latDeg, lonDeg float64) (sunrise, sunset time.Time, solved, alwaysUp bool) {
	y, m, d := dateUTC.Date()

	// Julian day at solar noon UTC for the given date - the input to every
	// step below. Using noon rather than midnight keeps the single JD value
	// representative of the whole day; the algorithm's accuracy doesn't
	// warrant anything finer.
	jd := julianDay(y, int(m), d) + 0.5
	t := (jd - 2451545.0) / 36525.0 // Julian century

	geomMeanLongSun := math.Mod(280.46646+t*(36000.76983+t*0.0003032), 360)
	geomMeanAnomSun := 357.52911 + t*(35999.05029-0.0001537*t)
	eccentEarthOrbit := 0.016708634 - t*(0.000042037+0.0000001267*t)

	mRad := deg2rad(geomMeanAnomSun)
	sunEqOfCtr := math.Sin(mRad)*(1.914602-t*(0.004817+0.000014*t)) +
		math.Sin(2*mRad)*(0.019993-0.000101*t) +
		math.Sin(3*mRad)*0.000289

	sunTrueLong := geomMeanLongSun + sunEqOfCtr
	sunAppLong := sunTrueLong - 0.00569 - 0.00478*math.Sin(deg2rad(125.04-1934.136*t))

	meanObliqEcliptic := 23 + (26+(21.448-t*(46.815+t*(0.00059-t*0.001813)))/60)/60
	obliqCorr := meanObliqEcliptic + 0.00256*math.Cos(deg2rad(125.04-1934.136*t))

	sunDeclin := math.Asin(math.Sin(deg2rad(obliqCorr)) * math.Sin(deg2rad(sunAppLong)))

	varY := math.Pow(math.Tan(deg2rad(obliqCorr/2)), 2)
	eqTime := 4 * rad2deg(
		varY*math.Sin(2*deg2rad(geomMeanLongSun))-
			2*eccentEarthOrbit*math.Sin(mRad)+
			4*eccentEarthOrbit*varY*math.Sin(mRad)*math.Cos(2*deg2rad(geomMeanLongSun))-
			0.5*varY*varY*math.Sin(4*deg2rad(geomMeanLongSun))-
			1.25*eccentEarthOrbit*eccentEarthOrbit*math.Sin(2*mRad),
	)

	// The hour angle at sunrise/sunset, for a horizon depressed 0.833deg to
	// account for atmospheric refraction and the sun's apparent radius. If
	// this ratio falls outside [-1, 1], acos has no solution: the sun never
	// crosses the horizon that day at all (polar day if the ratio is below
	// -1, since cos(hourAngle) would need to exceed 1 to reach the horizon;
	// polar night if above 1).
	haArg := math.Cos(deg2rad(90.833))/(math.Cos(deg2rad(latDeg))*math.Cos(sunDeclin)) - math.Tan(deg2rad(latDeg))*math.Tan(sunDeclin)
	if haArg > 1 {
		return time.Time{}, time.Time{}, false, false // polar night
	}
	if haArg < -1 {
		return time.Time{}, time.Time{}, false, true // polar day
	}
	haSunrise := rad2deg(math.Acos(haArg))

	solarNoonMin := 720 - 4*lonDeg - eqTime // minutes from UTC midnight
	sunriseMin := solarNoonMin - haSunrise*4
	sunsetMin := solarNoonMin + haSunrise*4

	base := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	sunrise = base.Add(time.Duration(sunriseMin * float64(time.Minute)))
	sunset = base.Add(time.Duration(sunsetMin * float64(time.Minute)))
	return sunrise, sunset, true, false
}

// julianDay returns the Julian day number for the given Gregorian
// calendar date at 0h UTC.
func julianDay(year, month, day int) float64 {
	if month <= 2 {
		year--
		month += 12
	}
	a := math.Floor(float64(year) / 100)
	b := 2 - a + math.Floor(a/4)
	return math.Floor(365.25*float64(year+4716)) + math.Floor(30.6001*float64(month+1)) + float64(day) + b - 1524.5
}

func deg2rad(deg float64) float64 { return deg * math.Pi / 180 }
func rad2deg(rad float64) float64 { return rad * 180 / math.Pi }
