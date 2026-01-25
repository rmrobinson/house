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
	apiConfigPath        = "/api/config"
	cameraRestreamFormat = "rtsp://%s:8554/%s"
)

type apiCamera struct {
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

type apiConfig struct {
	Cameras map[string]apiCamera `json:"cameras"`
}

type Camera struct {
	Name    string
	Enabled bool

	Endpoint       *url.URL
	MotionDetected bool
}

type Client struct {
	logger      *zap.Logger
	client      *http.Client
	apiEndpoint *url.URL
	cameraHost  string
}

func NewClient(logger *zap.Logger, client *http.Client, apiEndpoint *url.URL, cameraHost string) *Client {
	return &Client{
		logger:      logger,
		apiEndpoint: apiEndpoint,
		client:      client,
		cameraHost:  cameraHost,
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

func (c *Client) GetCameras(ctx context.Context) ([]Camera, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s://%s/%s", c.apiEndpoint.Scheme, c.apiEndpoint.Host, apiConfigPath), nil)
	if err != nil {
		c.logger.Error("unable to create http request", zap.Error(err))
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("unable to get config from frigate", zap.Error(err))
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		c.logger.Error("got failure response code", zap.Int("response_code", resp.StatusCode))
		return nil, errors.New("unsuccessful request")
	} else if resp.Header.Get("Content-Type") != "application/json" {
		c.logger.Error("got a non-json response", zap.String("content_type", resp.Header.Get("Content-Type")))
		return nil, errors.New("unsuccessful request")
	}

	apiConfig := &apiConfig{}
	if err := json.NewDecoder(resp.Body).Decode(apiConfig); err != nil {
		c.logger.Error("unable to decode response body", zap.Error(err))
		return nil, err
	}

	cameras := []Camera{}
	for _, camera := range apiConfig.Cameras {
		ep, err := url.Parse(fmt.Sprintf(cameraRestreamFormat, c.cameraHost, camera.Name))
		if err != nil {
			c.logger.Error("unable to format url for camera stream", zap.String("camera_name", camera.Name), zap.String("format", cameraRestreamFormat))
			continue
		}
		cameras = append(cameras, Camera{
			Name:     camera.Name,
			Enabled:  camera.Enabled,
			Endpoint: ep,
		})
	}

	return cameras, nil
}
