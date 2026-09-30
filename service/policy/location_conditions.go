package policy

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// SunEventParams parameterizes the "schedule.sun-event" condition type: a
// momentary trigger (see SunEventCondition) at each day's sunrise or sunset
// for the house's location. Latitude/longitude come from
// HomeAPI.GetHouseState("location.latitude"/"location.longitude"); see
// HomeAPI's doc comment for that contract, and LocationHomeAPI for a ready
// way to answer it.
//
// TZ picks the timezone the day boundary and the pulse are reported in (an
// IANA zone name, e.g. "America/Toronto"); if empty,
// HomeAPI.GetHouseState("location.timezone") is used, falling back to the
// engine process's local zone if that's also unavailable - see resolveTZ.
// OffsetMinutes shifts the trigger from the exact sunrise/sunset instant:
// negative fires earlier (e.g. -30 for civil twilight, 30 minutes before
// sunset), positive later.
type SunEventParams struct {
	Event         string // "sunrise" or "sunset"
	OffsetMinutes int
	TZ            string
}

// SunWindowParams parameterizes the "schedule.daylight" condition type: true
// while the current time, in the house's location/timezone, is between
// today's sunrise and sunset. Wrap with ExprNot for "is it dark"/"is it
// night". See SunEventParams for where latitude/longitude/timezone come
// from.
type SunWindowParams struct {
	TZ string
}

// DateRangeParams parameterizes the "schedule.date-range" condition type:
// true while today's (month, day), in the house's timezone (TZ, or
// HomeAPI.GetHouseState("location.timezone") if TZ is empty - see
// resolveTZ), falls within [StartMonth,StartDay, EndMonth,EndDay] inclusive,
// year-independent. A range whose end falls earlier in the year than its
// start wraps across New Year's (e.g. December 20 - January 5, for a
// "holiday season" policy); a single-day range (start == end) recurs every
// year - e.g. Christmas is StartMonth:12, StartDay:25, EndMonth:12,
// EndDay:25; "only in December" is StartMonth:12, StartDay:1, EndMonth:12,
// EndDay:31.
type DateRangeParams struct {
	StartMonth, StartDay int
	EndMonth, EndDay     int
	TZ                   string
}

// locationReader resolves the house's latitude/longitude (degrees) via
// HomeAPI.GetHouseState's "location.latitude"/"location.longitude" keys,
// holding the last successfully read value across a failed re-read - the
// same "hold the last known value" convention attribute.threshold's read
// closure uses for a failed GetState - so a transient error doesn't
// zero out a sun calculation already in flight.
type locationReader struct {
	home   HomeAPI
	logger *zap.Logger

	mu    sync.Mutex
	have  bool
	lat   float64
	lon   float64
}

func newLocationReader(home HomeAPI, logger *zap.Logger) *locationReader {
	return &locationReader{home: home, logger: logger}
}

// read implements the locate signature SunEventCondition/SunWindowCondition
// expect.
func (r *locationReader) read() (lat, lon float64, ok bool) {
	latVal, latErr := r.home.GetHouseState("location.latitude")
	lonVal, lonErr := r.home.GetHouseState("location.longitude")

	var lat64, lon64 float64
	var convErr error
	if latErr == nil && lonErr == nil {
		if lat64, convErr = toFloat64(latVal); convErr == nil {
			lon64, convErr = toFloat64(lonVal)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if latErr == nil && lonErr == nil && convErr == nil {
		r.lat, r.lon, r.have = lat64, lon64, true
		return r.lat, r.lon, true
	}

	if !r.have {
		r.logger.Warn("location unavailable: GetHouseState(\"location.latitude\"/\"location.longitude\") failed and no prior reading to fall back on",
			zap.NamedError("latErr", latErr), zap.NamedError("lonErr", lonErr), zap.NamedError("convErr", convErr))
		return 0, 0, false
	}
	r.logger.Warn("location read failed, holding last known value",
		zap.NamedError("latErr", latErr), zap.NamedError("lonErr", lonErr), zap.NamedError("convErr", convErr))
	return r.lat, r.lon, true
}

// resolveTZ returns the time.Location named by override if non-empty, else
// by HomeAPI.GetHouseState("location.timezone") if that's set to a valid IANA
// zone name, else the engine process's local zone - the same fallback
// schedule.daily applies to an unset/invalid TZ, extended with the
// house-state lookup in between.
func resolveTZ(home HomeAPI, override string, logger *zap.Logger) *time.Location {
	tzName := override
	if tzName == "" {
		if v, err := home.GetHouseState("location.timezone"); err == nil {
			if s, ok := v.(string); ok {
				tzName = s
			}
		}
	}
	if tzName == "" {
		return time.Local
	}

	loc, err := time.LoadLocation(tzName)
	if err != nil {
		logger.Warn("schedule: invalid timezone, using local", zap.String("tz", tzName), zap.Error(err))
		return time.Local
	}
	return loc
}

// normalizeMonthDay clamps month to [1,12] and day to [1,31] by wraparound,
// warning if either was out of range - the same lightweight-normalization
// style RegisterBuiltinConditionTypes applies to schedule.daily's
// Hour/Minute. It doesn't validate day against month's actual length (e.g.
// {2, 30} passes through as-is): DateRangeCondition compares (month, day)
// pairs lexically and never constructs a calendar date from them, so an
// authored day that a given month never reaches simply never matches in
// that month, which is a harmless (if probably unintended) no-op rather
// than a crash.
func normalizeMonthDay(month, day int, label string, logger *zap.Logger) (int, int) {
	origMonth, origDay := month, day
	if month < 1 || month > 12 {
		month = ((month-1)%12+12)%12 + 1
	}
	if day < 1 || day > 31 {
		day = ((day-1)%31+31)%31 + 1
	}
	if month != origMonth || day != origDay {
		logger.Warn("schedule.date-range: month/day out of range, normalizing",
			zap.String("field", label),
			zap.Int("month", origMonth), zap.Int("day", origDay),
			zap.Int("normalizedMonth", month), zap.Int("normalizedDay", day))
	}
	return month, day
}

// RegisterLocationConditionTypes registers the condition types built on the
// house's location and timezone: "schedule.sun-event"
// (SunEventCondition), "schedule.daylight" (SunWindowCondition), and
// "schedule.date-range" (DateRangeCondition). See SunEventParams,
// SunWindowParams and DateRangeParams for how each sources latitude/
// longitude/timezone from HomeAPI.GetHouseState.
//
// No HomeAPI implementation in this codebase backs the "location.*"
// house-state keys from a real source yet (bridgehome's GetHouseState is
// unconditionally ErrNotImplemented, staying strictly scoped to
// bridge.proto - see its own doc comment): wrap whatever HomeAPI a
// deployment uses with LocationHomeAPI to make these three condition types
// usable today.
//
// Callers follow the same ordering rule as RegisterBuiltinConditionTypes.
func RegisterLocationConditionTypes(e *Engine) {
	RegisterConditionType(e.registry, "schedule.sun-event", func(p SunEventParams) Condition {
		sunset := false
		switch p.Event {
		case "sunset":
			sunset = true
		case "sunrise":
			sunset = false
		default:
			e.logger.Warn("schedule.sun-event: Event must be \"sunrise\" or \"sunset\", defaulting to sunrise",
				zap.String("event", p.Event))
		}

		loc := resolveTZ(e.home, p.TZ, e.logger)
		locate := newLocationReader(e.home, e.logger).read
		return NewSunEventCondition(loc, sunset, time.Duration(p.OffsetMinutes)*time.Minute, locate)
	})

	RegisterConditionType(e.registry, "schedule.daylight", func(p SunWindowParams) Condition {
		loc := resolveTZ(e.home, p.TZ, e.logger)
		locate := newLocationReader(e.home, e.logger).read
		return NewSunWindowCondition(loc, locate)
	})

	RegisterConditionType(e.registry, "schedule.date-range", func(p DateRangeParams) Condition {
		startMonth, startDay := normalizeMonthDay(p.StartMonth, p.StartDay, "Start", e.logger)
		endMonth, endDay := normalizeMonthDay(p.EndMonth, p.EndDay, "End", e.logger)
		loc := resolveTZ(e.home, p.TZ, e.logger)
		return NewDateRangeCondition(loc, startMonth, startDay, endMonth, endDay)
	})
}
