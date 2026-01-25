package frigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"go.uber.org/zap"
)

const (
	apiConfigPath = "/api/config"
	apiStatsPath  = "/api/stats"
)

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

type ConfigResponse struct {
	Cameras map[string]CameraConfig `json:"cameras"`
}

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

type StatsResponse struct {
	Cameras map[string]CameraStats `json:"cameras"`
}

type Client struct {
	logger      *zap.Logger
	client      *http.Client
	apiEndpoint *url.URL
}

func NewClient(logger *zap.Logger, client *http.Client, apiEndpoint *url.URL) *Client {
	return &Client{
		logger:      logger,
		apiEndpoint: apiEndpoint,
		client:      client,
	}
}

func (c *Client) GetIP() string {
	return c.apiEndpoint.Hostname()
}

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

func (c *Client) GetStats(ctx context.Context) (*StatsResponse, error) {
	apiStats := &StatsResponse{}
	err := c.apiRequest(ctx, apiStatsPath, apiStats)
	return apiStats, err
}

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
