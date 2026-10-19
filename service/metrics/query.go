package metrics

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WatermarkStore reports how far a device's backfill has got.
type WatermarkStore interface {
	// LatestBackfill returns the timestamp of the newest source=backfill
	// point recorded for deviceID at or after notBefore. ok is false if there
	// is none.
	LatestBackfill(ctx context.Context, deviceID string, notBefore time.Time) (at time.Time, ok bool, err error)
}

// InfluxQuerier answers WatermarkStore from InfluxDB v2's Flux query API.
// Reading the watermark back from Influx, rather than keeping it in memory or
// on disk, means a restarted or relocated metricsd resumes exactly where the
// data says it left off.
type InfluxQuerier struct {
	queryURL     string
	bucket       string
	rollupBucket string
	token        string
	client       *http.Client
}

// NewInfluxQuerier creates a querier from the same config the writer uses.
func NewInfluxQuerier(cfg InfluxConfig) (*InfluxQuerier, error) {
	if cfg.URL == "" || cfg.Org == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("influx url, org and bucket are required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	rollup := cfg.RollupBucket
	if rollup == "" {
		rollup = DefaultRollupBucket
	}
	q := url.Values{}
	q.Set("org", cfg.Org)
	return &InfluxQuerier{
		queryURL:     strings.TrimRight(cfg.URL, "/") + "/api/v2/query?" + q.Encode(),
		bucket:       cfg.Bucket,
		rollupBucket: rollup,
		token:        cfg.Token,
		client:       client,
	}, nil
}

// fluxString quotes s as a Flux string literal. "${" is escaped because it
// starts an interpolation inside Flux strings; a bare "$" is left alone, as
// `\$` on its own is not an escape the Flux spec defines.
func fluxString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "${", `\${`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// LatestBackfill implements WatermarkStore.
//
// Only _time is kept before grouping: a device's fields have different value
// types (int, float, bool), and Flux refuses to merge them into one table for
// max() ("schema collision") if _value and _field are still present.
func (q *InfluxQuerier) LatestBackfill(ctx context.Context, deviceID string, notBefore time.Time) (time.Time, bool, error) {
	flux := fmt.Sprintf(`from(bucket: %s)
  |> range(start: %s)
  |> filter(fn: (r) => r._measurement == %s and r.source == %s and r.device_id == %s)
  |> keep(columns: ["_time"])
  |> group()
  |> max(column: "_time")`,
		fluxString(q.bucket),
		notBefore.UTC().Format(time.RFC3339),
		fluxString(StateMeasurement), fluxString(SourceBackfill), fluxString(deviceID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.queryURL, strings.NewReader(flux))
	if err != nil {
		return time.Time{}, false, err
	}
	req.Header.Set("Authorization", "Token "+q.token)
	req.Header.Set("Content-Type", "application/vnd.flux")
	req.Header.Set("Accept", "application/csv")

	resp, err := q.client.Do(req)
	if err != nil {
		return time.Time{}, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return time.Time{}, false, fmt.Errorf("influx query returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return parseLatestTime(resp.Body)
}

// parseLatestTime finds the "_time" column in Influx's CSV and returns the
// newest value in it. An empty result (no matching points) is not an error.
func parseLatestTime(r io.Reader) (time.Time, bool, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	var latest time.Time
	var found bool
	timeCol := -1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse influx csv: %w", err)
		}

		// Each table starts with its own header row.
		if idx := indexOf(rec, "_time"); idx >= 0 {
			timeCol = idx
			continue
		}
		if timeCol < 0 || timeCol >= len(rec) {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, rec[timeCol])
		if err != nil {
			continue
		}
		if !found || t.After(latest) {
			latest, found = t, true
		}
	}
	return latest, found, nil
}

func indexOf(rec []string, s string) int {
	for i, v := range rec {
		if v == s {
			return i
		}
	}
	return -1
}
