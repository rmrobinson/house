package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Sink accepts points for storage. Write must never block on the backing
// store: callers sit on a live update stream that must not be back-pressured.
type Sink interface {
	Write(points ...Point)
}

// InfluxConfig configures an InfluxWriter against an InfluxDB v2 server.
type InfluxConfig struct {
	URL    string
	Org    string
	Bucket string
	Token  string

	// FlushInterval is how often buffered points are sent. Default 5s.
	FlushInterval time.Duration
	// BatchSize is the most lines sent in one request. Default 5000.
	BatchSize int
	// MaxBuffered bounds points held while Influx is slow or down; beyond it
	// the oldest are dropped. Default 100000.
	MaxBuffered int
	// HTTPClient overrides the default (10s timeout) client; for tests.
	HTTPClient *http.Client
}

// InfluxWriter buffers points and writes them to InfluxDB v2 as line
// protocol over its HTTP write API. It speaks that one endpoint directly
// rather than pulling in influxdb-client-go and its dependency tree for what
// is a single POST.
type InfluxWriter struct {
	logger   *zap.Logger
	cfg      InfluxConfig
	writeURL string
	client   *http.Client

	mu  sync.Mutex
	buf []string

	dropped atomic.Int64
	written atomic.Int64
}

// NewInfluxWriter creates a writer; call Run to start flushing.
func NewInfluxWriter(logger *zap.Logger, cfg InfluxConfig) (*InfluxWriter, error) {
	if cfg.URL == "" || cfg.Org == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("influx url, org and bucket are required")
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 5000
	}
	if cfg.MaxBuffered <= 0 {
		cfg.MaxBuffered = 100000
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	q := url.Values{}
	q.Set("org", cfg.Org)
	q.Set("bucket", cfg.Bucket)
	q.Set("precision", "ns")

	return &InfluxWriter{
		logger:   logger,
		cfg:      cfg,
		writeURL: strings.TrimRight(cfg.URL, "/") + "/api/v2/write?" + q.Encode(),
		client:   client,
	}, nil
}

// Write queues points. It never blocks; if the buffer is full the oldest
// points are discarded and counted (see Dropped).
func (w *InfluxWriter) Write(points ...Point) {
	lines := make([]string, 0, len(points))
	for _, p := range points {
		if l := p.Line(); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return
	}

	w.mu.Lock()
	w.buf = append(w.buf, lines...)
	w.trimLocked()
	w.mu.Unlock()
}

// trimLocked drops the oldest buffered lines beyond MaxBuffered.
func (w *InfluxWriter) trimLocked() {
	if over := len(w.buf) - w.cfg.MaxBuffered; over > 0 {
		w.buf = append([]string(nil), w.buf[over:]...)
		w.dropped.Add(int64(over))
	}
}

// Dropped is the number of points discarded because Influx could not keep up.
func (w *InfluxWriter) Dropped() int64 { return w.dropped.Load() }

// Written is the number of points Influx has accepted.
func (w *InfluxWriter) Written() int64 { return w.written.Load() }

// Pending is the number of points currently buffered.
func (w *InfluxWriter) Pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.buf)
}

// Run flushes on an interval until ctx is done, then makes one last attempt
// so a clean shutdown doesn't lose the tail.
func (w *InfluxWriter) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			w.Flush(flushCtx)
			cancel()
			return
		case <-t.C:
			w.Flush(ctx)
		}
	}
}

// Flush sends everything buffered, in BatchSize chunks, stopping at the first
// failure (the failed chunk is put back for the next flush).
func (w *InfluxWriter) Flush(ctx context.Context) {
	for ctx.Err() == nil {
		w.mu.Lock()
		n := min(len(w.buf), w.cfg.BatchSize)
		if n == 0 {
			w.mu.Unlock()
			return
		}
		batch := append([]string(nil), w.buf[:n]...)
		w.buf = w.buf[n:]
		w.mu.Unlock()

		if retry := w.deliver(ctx, batch); len(retry) > 0 {
			w.mu.Lock()
			w.buf = append(retry, w.buf...)
			w.trimLocked()
			w.mu.Unlock()
			return
		}
	}
}

// deliver sends lines and returns those that should be retried later.
//
// A batch Influx rejects as malformed is bisected until the offending line(s)
// are isolated and dropped: Influx rejects a whole batch for one bad line, and
// retrying it verbatim would wedge the queue forever.
func (w *InfluxWriter) deliver(ctx context.Context, lines []string) (retry []string) {
	res, err := w.send(ctx, lines)
	switch res {
	case sendOK:
		w.written.Add(int64(len(lines)))
		return nil
	case sendRetry:
		w.logger.Warn("influx write failed, will retry", zap.Error(err), zap.Int("points", len(lines)))
		return lines
	}

	if len(lines) == 1 {
		w.logger.Error("influx rejected point, dropping it", zap.Error(err), zap.String("line", lines[0]))
		w.dropped.Add(1)
		return nil
	}
	mid := len(lines) / 2
	return append(w.deliver(ctx, lines[:mid]), w.deliver(ctx, lines[mid:])...)
}

type sendResult int

const (
	sendOK sendResult = iota
	// sendRetry covers outages, throttling and anything that looks like a
	// configuration problem (bad token or bucket): keep the data, since fixing
	// the config makes it deliverable and the bounded buffer caps the cost.
	sendRetry
	// sendRejected means this payload itself is the problem.
	sendRejected
)

func (w *InfluxWriter) send(ctx context.Context, lines []string) (sendResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.writeURL, strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		return sendRejected, err
	}
	req.Header.Set("Authorization", "Token "+w.cfg.Token)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := w.client.Do(req)
	if err != nil {
		return sendRetry, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return sendOK, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err = fmt.Errorf("influx returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return sendRejected, err
	}
	return sendRetry, err
}
