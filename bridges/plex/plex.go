package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"net/http"
	"sync"

	"github.com/LukeHagar/plexgo"
	"github.com/LukeHagar/plexgo/models/operations"
	"github.com/hekmon/plexwebhooks"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rmrobinson/house/api/device"
	"github.com/rmrobinson/house/api/trait"
	"github.com/rmrobinson/house/service/bridge"
)

var errPlexServerMissingCapabilities = errors.New("plex server capabilities are empty")

func webhookEventTypeToPlaybackState(event plexwebhooks.EventType) trait.Media_PlaybackState {
	switch event {
	case plexwebhooks.EventTypePause:
		return trait.Media_PS_PAUSED
	case plexwebhooks.EventTypePlay:
		return trait.Media_PS_PLAYING
	case plexwebhooks.EventTypeResume:
		return trait.Media_PS_PLAYING
	case plexwebhooks.EventTypeScrobble:
		return trait.Media_PS_COMPLETED
	case plexwebhooks.EventTypeStop:
		return trait.Media_PS_STOPPED
	default:
		return trait.Media_PS_UNSPECIFIED
	}
}

func webhookPayloadToDevice(payload *plexwebhooks.Payload) *device.Device {
	var artURL *string
	if len(payload.Metadata.Art) > 0 {
		artURL = &payload.Metadata.Art
	}
	var showDetails *trait.Media_ShowDetails
	var movieDetails *trait.Media_MovieDetails

	if payload.Metadata.Type == plexwebhooks.MediaTypeEpisode {
		showDetails = &trait.Media_ShowDetails{
			Id:             payload.Metadata.GUID.String(),
			EpisodeTitle:   payload.Metadata.Title,
			EpisodeSummary: payload.Metadata.Summary,
			ReleaseYear:    int32(payload.Metadata.Year),
			ContentRating:  payload.Metadata.ContentRating,
			ArtUrl:         artURL,

			SeasonId:    payload.Metadata.ParentKey,
			SeasonTitle: payload.Metadata.ParentTitle,
			ShowId:      payload.Metadata.GrandparentKey,
			ShowTitle:   payload.Metadata.GrandparentTitle,
		}
	} else if payload.Metadata.Type == plexwebhooks.MediaTypeMovie {
		movieDetails = &trait.Media_MovieDetails{
			Id:            payload.Metadata.GUID.String(),
			Title:         payload.Metadata.Title,
			Summary:       payload.Metadata.Summary,
			Studio:        payload.Metadata.Studio,
			ReleaseYear:   int32(payload.Metadata.Year),
			ContentRating: payload.Metadata.ContentRating,
			ArtUrl:        artURL,
		}
	}

	return &device.Device{
		Id: payload.Player.UUID,
		Config: &device.Device_Config{
			Name: payload.Player.Title,
		},
		Address: &device.Device_Address{
			Address: payload.Player.PublicAddress.String(),
		},
		LastSeen: timestamppb.Now(),
		Details: &device.Device_MediaPlayer{
			MediaPlayer: &device.MediaPlayer{
				Media: &trait.Media{
					Attributes: &trait.Media_Attributes{},
					State: &trait.Media_State{
						DeviceState:     trait.Media_DEVICE_STATE_ACTIVE,
						PlaybackState:   webhookEventTypeToPlaybackState(payload.Event),
						ShowDetails:     showDetails,
						MovieDetails:    movieDetails,
						PlaybackLengthS: payload.Metadata.Duration.Seconds(),
					},
				},
			},
		},
	}
}

// Plex exposes logic to interact with the Plex instance.
type Plex struct {
	logger *zap.Logger
	svc    *bridge.Service

	serverURL string
	apiKey    string

	api *plexgo.PlexAPI

	id   string
	name string

	plexDeviceCache     map[string]operations.Device
	plexDeviceCacheLock sync.Mutex
}

// NewPlex creates a new instance of the Plex struct.
func NewPlex(logger *zap.Logger, svc *bridge.Service, serverURL string, apiKey string) *Plex {
	return &Plex{
		logger:          logger,
		svc:             svc,
		serverURL:       serverURL,
		apiKey:          apiKey,
		plexDeviceCache: map[string]operations.Device{},
	}
}

// Start confirms that the API is accessible and retrieves some initial metadata
func (p *Plex) Start(ctx context.Context) error {
	opts := []plexgo.SDKOption{
		plexgo.WithSecurity(p.apiKey),
	}
	if len(p.serverURL) < 1 {
		p.logger.Info("no plex url specified, defaulting to plex.tv")
	} else {
		opts = append(opts, plexgo.WithServerURL(p.serverURL))
	}

	plexClient := plexgo.New(opts...)
	p.api = plexClient

	return p.Refresh(ctx)
}

// Refresh refreshes the server information cached in this struct.
func (p *Plex) Refresh(ctx context.Context) error {
	res, err := p.api.Server.GetServerCapabilities(context.Background())
	if err != nil {
		p.logger.Error("unable to retrieve plex server capabilities", zap.Error(err))
		return err
	} else if res.Object == nil {
		p.logger.Error("plex server capabilites are empty")
		return errPlexServerMissingCapabilities
	}

	p.id = *res.Object.MediaContainer.MachineIdentifier
	p.name = *res.Object.MediaContainer.FriendlyName

	getDevicesResp, err := p.api.Server.GetDevices(ctx)
	if err != nil {
		p.logger.Error("unable to get plex devices",
			zap.Error(err))
		return err
	}

	p.plexDeviceCacheLock.Lock()
	defer p.plexDeviceCacheLock.Unlock()

	// Reset the entries in the cache if the platform info is present.
	p.plexDeviceCache = map[string]operations.Device{}
	for _, plexDevice := range getDevicesResp.Object.MediaContainer.Device {
		if plexDevice.Platform == nil || len(*plexDevice.Platform) < 1 {
			continue
		}
		p.plexDeviceCache[*plexDevice.ClientIdentifier] = plexDevice
	}

	return nil
}

func (p *Plex) getArtwork(ctx context.Context, path string) (image.Image, error) {
	_ = fmt.Sprintf("%s%s?X-Plex-Token=%s", p.serverURL, path, p.apiKey)
	// TODO: get the image and parse it
	return nil, errors.New("not implemented")
}

func (p *Plex) handleWebhook(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	multiPartReader, err := r.MultipartReader()
	if err != nil {
		if err == http.ErrNotMultipart || err == http.ErrMissingBoundary {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		_, wErr := w.Write([]byte(err.Error()))
		if wErr != nil {
			p.logger.Error("failed to write error message",
				zap.Error(err), zap.Error(wErr))
		}
		p.logger.Error("unable to create multipart reader from request, ignoring",
			zap.Error(err))
		return
	}

	payload, thumb, err := plexwebhooks.Extract(multiPartReader)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, wErr := w.Write([]byte(err.Error()))
		if wErr != nil {
			p.logger.Error("failed to write error message",
				zap.Error(err), zap.Error(wErr))
		}
		p.logger.Error("unable to extract webhook content from reader",
			zap.Error(err))
		return
	}

	if thumb != nil {
		p.logger.Debug("thumbnail present")
		// TODO: what is best to do with this?
	}

	updatedDevice := webhookPayloadToDevice(payload)

	p.plexDeviceCacheLock.Lock()
	if d, found := p.plexDeviceCache[updatedDevice.Id]; found {
		updatedDevice.ModelDescription = d.Platform
	}
	p.plexDeviceCacheLock.Unlock()

	p.svc.UpdateDevice(updatedDevice)
}
