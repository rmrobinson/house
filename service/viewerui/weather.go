package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	weatherv1 "github.com/rmrobinson/house/api/weather/v1"
	"github.com/rmrobinson/house/service/lib/hub"
)

// weatherStaleAfter is how old the newest reading may be before the overview
// stops presenting it as current. The station reports every ~16s, so this
// only trips when the weather server or the station is down.
const weatherStaleAfter = 15 * time.Minute

// weatherIdleTimeout is how long the stream may go without a reading before
// it's presumed dead and re-dialled. The station reports every ~16s; a
// half-open connection (a laptop waking from sleep, a NAT dropping state)
// never errors on its own, so without this the feed would silently stall.
// A var so tests can shorten it.
var weatherIdleTimeout = 2 * time.Minute

// weatherFeed keeps the newest reading from weather-server's
// WeatherService.StreamReadings (github.com/rmrobinson/weather-server) and
// fans readings out so the overview's SSE stream can re-render on each.
type weatherFeed struct {
	hub *hub.Hub[*weatherv1.WeatherReading]

	mu     sync.Mutex
	latest *weatherv1.WeatherReading
}

// startWeatherFeed connects to the weather server and keeps reconnecting.
// psk, when set, is sent as the "authorization: psk <secret>" header the
// server requires if it was started with WEATHER_PSK.
func startWeatherFeed(ctx context.Context, logger *zap.Logger, conn *grpc.ClientConn, psk string) *weatherFeed {
	client := weatherv1.NewWeatherServiceClient(conn)
	f := &weatherFeed{
		hub: hub.New(logger, func(ctx context.Context) (func() (*weatherv1.WeatherReading, error), error) {
			if psk != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "psk "+psk)
			}
			ctx, cancel := context.WithCancel(ctx)
			stream, err := client.StreamReadings(ctx, &weatherv1.StreamRequest{})
			if err != nil {
				cancel()
				return nil, err
			}
			// Cancelling the stream's context makes the blocked Recv return,
			// which the hub treats as a dropped stream and reconnects.
			idle := time.AfterFunc(weatherIdleTimeout, cancel)
			return func() (*weatherv1.WeatherReading, error) {
				r, err := stream.Recv()
				if err != nil {
					idle.Stop()
					cancel()
					return nil, err
				}
				idle.Reset(weatherIdleTimeout)
				return r, nil
			}, nil
		}),
	}
	go f.hub.Run(ctx, "weather stream ended, reconnecting")

	updates, unsub := f.hub.Subscribe()
	go func() {
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-updates:
				f.mu.Lock()
				f.latest = r
				f.mu.Unlock()
			}
		}
	}()
	return f
}

// current returns the newest reading, or nil plus a short reason when there
// is no usable one. Safe on a nil feed (weather not configured).
func (f *weatherFeed) current(now time.Time) (*weatherv1.WeatherReading, string) {
	if f == nil {
		return nil, "no weather server configured"
	}
	f.mu.Lock()
	r := f.latest
	f.mu.Unlock()
	switch {
	case r == nil:
		return nil, "waiting for a reading"
	case r.GetTimestamp() != nil && now.Sub(r.GetTimestamp().AsTime()) > weatherStaleAfter:
		return nil, fmt.Sprintf("last reading %d min ago", int(now.Sub(r.GetTimestamp().AsTime()).Minutes()))
	}
	return r, ""
}

// exteriorTiles renders the exterior climate tiles; with no usable reading,
// one placeholder tile says why.
func exteriorTiles(r *weatherv1.WeatherReading, unavailable string) []tileView {
	if r == nil {
		return []tileView{{Label: "Weather", Value: "—", Note: unavailable}}
	}
	temp := tileView{Label: "Temp", Value: fmt.Sprintf("%.1f°C", r.GetTempC())}
	if d := r.GetFeelsLikeC() - r.GetTempC(); d >= 1 || d <= -1 {
		temp.Note = fmt.Sprintf("feels like %.0f°C", r.GetFeelsLikeC())
	}
	wind := tileView{Label: "Wind", Value: fmt.Sprintf("%.0f km/h %s", r.GetWindSpeedMs()*3.6, compass(r.GetWindDirDeg()))}
	if r.GetWindGustMs() > r.GetWindSpeedMs() {
		wind.Note = fmt.Sprintf("gusts %.0f km/h", r.GetWindGustMs()*3.6)
	}
	rain := tileView{Label: "Rain today", Value: fmt.Sprintf("%.1f mm", r.GetRainDailyMm())}
	if r.GetRainMmHr() > 0 {
		rain.Note = fmt.Sprintf("falling at %.1f mm/h", r.GetRainMmHr())
	}
	tiles := []tileView{temp}
	if c := conditionLabel(r.GetCondition()); c != "" {
		tiles = append(tiles, tileView{Label: "Conditions", Value: c})
	}
	return append(tiles,
		tileView{Label: "Humidity", Value: fmt.Sprintf("%.0f%%", r.GetHumidityPct()), Note: fmt.Sprintf("dew point %.0f°C", r.GetDewPointC())},
		wind,
		rain,
		tileView{Label: "UV index", Value: fmt.Sprintf("%.1f", r.GetUvIndex())},
		tileView{Label: "Pressure", Value: fmt.Sprintf("%.0f hPa", r.GetPressureHpa())},
	)
}

// conditionLabel turns WEATHER_CONDITION_MOSTLY_SUNNY into "Mostly sunny".
func conditionLabel(c weatherv1.WeatherCondition) string {
	if c == weatherv1.WeatherCondition_WEATHER_CONDITION_UNSPECIFIED {
		return ""
	}
	s := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(c.String(), "WEATHER_CONDITION_"), "_", " "))
	return strings.ToUpper(s[:1]) + s[1:]
}

func compass(deg float64) string {
	pts := []string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"}
	i := int((deg+11.25)/22.5) % 16
	if i < 0 {
		i += 16
	}
	return pts[i]
}
