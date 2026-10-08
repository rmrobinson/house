package frigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	apiConfigPath = "/api/config"
	apiStatsPath  = "/api/stats"
	wsPath        = "/ws"

	// wsReadTimeout bounds how long the websocket may stay silent before it's considered dead. Frigate
	// publishes its stats topic every few seconds, so a healthy connection is never quiet for this long.
	wsReadTimeout = 60 * time.Second
)

// CameraConfig contains some of the configured fields in a camera. This is only a partial definition.
type CameraConfig struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	Audio struct {
		Enabled bool `json:"enabled"`
	} `json:"audio"`

	Detect struct {
		Enabled bool `json:"enabled"`
	} `json:"detect"`

	FaceRecognition struct {
		Enabled bool `json:"enabled"`
	} `json:"face_recognition"`

	Ffmpeg struct {
		Inputs []struct {
			Path string `json:"path"`
		} `json:"inputs"`
	} `json:"ffmpeg"`
}

// ConfigResponse contains some of the fields returned from the /api/config endpoint. This is only a partial definition.
type ConfigResponse struct {
	Cameras map[string]CameraConfig `json:"cameras"`
}

// CameraStats contains the available stats for a given camera.
type CameraStats struct {
	AudioDBFS        float32 `json:"audio_dBFS"`
	AudioRMS         float32 `json:"audio_rms"`
	CameraFPS        float32 `json:"camera_fps"`
	CapturePID       int     `json:"capture_pid"`
	DetectionEnabled bool    `json:"detection_enabled"`
	DetectionFPS     float32 `json:"detection_fps"`
	FfmpegPID        int     `json:"ffmpeg_pid"`
	PID              int     `json:"pid"`
	ProcessFPS       float32 `json:"process_fps"`
	SkippedFPS       float32 `json:"skipped_fps"`
}

// ServiceStats contains some of the available stats for the Frigate service.
type ServiceStats struct {
	Version string `json:"version"`
}

// StatsResponse contains some of the fields returned from the /api/stats endpoint. This is only a partial definition.
type StatsResponse struct {
	Cameras map[string]CameraStats `json:"cameras"`
	Service ServiceStats           `json:"service"`
}

// Client is used to interact with an instance of the Frigate NVR API.
type Client struct {
	logger      *zap.Logger
	client      *http.Client
	apiEndpoint *url.URL
}

// NewClient creates a new instance of the Frigate API client.
func NewClient(logger *zap.Logger, client *http.Client, apiEndpoint *url.URL) *Client {
	return &Client{
		logger:      logger,
		apiEndpoint: apiEndpoint,
		client:      client,
	}
}

// GetIP returns the IP address or hostname of the configured endpoint.
func (c *Client) GetIP() string {
	return c.apiEndpoint.Hostname()
}

// GetPort returns the port of the configured endpoint; 0 if not available or not specified.
func (c *Client) GetPort() int {
	port := c.apiEndpoint.Port()
	if port == "" {
		return 0
	}
	numPort, err := strconv.Atoi(port)
	if err != nil {
		c.logger.Error("port invalid", zap.Error(err), zap.String("port", port))
		return 0
	}

	return numPort
}

// GetStats queries the stats HTTP endpoint and returns its response.
func (c *Client) GetStats(ctx context.Context) (*StatsResponse, error) {
	apiStats := &StatsResponse{}
	err := c.apiRequest(ctx, apiStatsPath, apiStats)
	return apiStats, err
}

// GetConfig queries the config HTTP endpoint and returns its response.
func (c *Client) GetConfig(ctx context.Context) (*ConfigResponse, error) {
	apiConfig := &ConfigResponse{}
	err := c.apiRequest(ctx, apiConfigPath, apiConfig)
	return apiConfig, err
}

func (c *Client) apiRequest(ctx context.Context, path string, apiResp any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s://%s/%s", c.apiEndpoint.Scheme, c.apiEndpoint.Host, path), nil)
	if err != nil {
		c.logger.Error("unable to create http request", zap.Error(err))
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("unable to get config from frigate", zap.Error(err))
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		c.logger.Error("got failure response code", zap.Int("response_code", resp.StatusCode))
		return errors.New("unsuccessful request")
	} else if resp.Header.Get("Content-Type") != "application/json" {
		c.logger.Error("got a non-json response", zap.String("content_type", resp.Header.Get("Content-Type")))
		return errors.New("unsuccessful request")
	}

	if err := json.NewDecoder(resp.Body).Decode(apiResp); err != nil {
		c.logger.Error("unable to decode response body", zap.Error(err))
		return err
	}

	return nil
}

// wsMessage is one frame on Frigate's /ws feed. Payload's shape depends on Topic, so it's left raw.
type wsMessage struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// FeedHandlers receives the updates StreamFeed extracts from Frigate's websocket feed. Either may be nil.
type FeedHandlers struct {
	// OnMotion is called when a camera's raw pixel-motion state changes. That signal also fires on
	// shadows, lighting changes and animals.
	OnMotion func(camera string, motion bool)

	// OnActiveCount is called with the number of currently-active objects of a label on a camera, from
	// Frigate's "<camera>/<label>/active" topics. "Active" excludes objects Frigate has judged
	// stationary (a parked car, someone sitting still).
	OnActiveCount func(camera, label string, count int)
}

// StreamFeed connects to Frigate's websocket feed and dispatches its motion and active-object updates to h
// until the connection drops or ctx is cancelled, then returns the reason.
//
// Frigate only publishes when a value changes, so the caller owns what a dropped connection means for the
// state it was tracking.
func (c *Client) StreamFeed(ctx context.Context, h FeedHandlers) error {
	scheme := "ws"
	if c.apiEndpoint.Scheme == "https" {
		scheme = "wss"
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, fmt.Sprintf("%s://%s%s", scheme, c.apiEndpoint.Host, wsPath), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Unblock the read below as soon as ctx is cancelled.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	for {
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		parts := strings.Split(msg.Topic, "/")
		switch {
		case len(parts) == 2 && parts[1] == "motion":
			// "<camera>/motion": "ON" or "OFF".
			var state string
			if err := json.Unmarshal(msg.Payload, &state); err != nil {
				c.logger.Warn("unparseable motion payload", zap.String("topic", msg.Topic), zap.Error(err))
				continue
			}
			if h.OnMotion != nil {
				h.OnMotion(parts[0], state == "ON")
			}
		case len(parts) == 3 && parts[2] == "active" && parts[1] != "all":
			// "<camera>/<label>/active"; "all" is Frigate's aggregate across labels.
			var count int
			if err := json.Unmarshal(msg.Payload, &count); err != nil {
				c.logger.Warn("unparseable active-object count", zap.String("topic", msg.Topic), zap.Error(err))
				continue
			}
			if h.OnActiveCount != nil {
				h.OnActiveCount(parts[0], parts[1], count)
			}
		}
	}
}
