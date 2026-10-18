package metrics

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	api2 "github.com/rmrobinson/house/api"
)

const (
	// DefaultMaxPoints is used when a request leaves max_points unset.
	DefaultMaxPoints = 120
	// MaxMaxPoints caps max_points: a sparkline never needs more, and it
	// bounds what one request can make Influx aggregate.
	MaxMaxPoints = 1000
	// MaxSeriesRange caps end-start. The rollup bucket keeps ~548d.
	MaxSeriesRange = 366 * 24 * time.Hour
	// MaxSeriesDevices caps device_ids, bounding the series one request can
	// make Influx read and merge.
	MaxSeriesDevices = 64
	// MaxDeviceIDLen caps one device id. Real ids (UUIDs, MACs, IEEE
	// addresses, "<bridge-uuid>:<n>") are far shorter; the cap keeps a request
	// from smuggling an enormous literal into the Flux text.
	MaxDeviceIDLen = 256
)

// seriesDevices validates the request's device selection and returns the
// device(s) to query: exactly one of device_id or device_ids, de-duplicated.
func seriesDevices(req *api2.GetSeriesRequest) (id string, ids []string, err error) {
	if req.GetDeviceId() != "" && len(req.GetDeviceIds()) > 0 {
		return "", nil, status.Error(codes.InvalidArgument, "set one of device_id and device_ids, not both")
	}
	if len(req.GetDeviceIds()) == 0 {
		if req.GetDeviceId() == "" {
			return "", nil, status.Error(codes.InvalidArgument, "device_id is required")
		}
		if len(req.GetDeviceId()) > MaxDeviceIDLen {
			return "", nil, status.Errorf(codes.InvalidArgument, "device_id must not exceed %d bytes", MaxDeviceIDLen)
		}
		return req.GetDeviceId(), nil, nil
	}

	// Rejected as soon as the distinct count passes the cap, so an enormous
	// list can't make the server build a set before being refused.
	seen := make(map[string]bool)
	for _, d := range req.GetDeviceIds() {
		if d == "" {
			return "", nil, status.Error(codes.InvalidArgument, "device_ids must not contain an empty id")
		}
		if len(d) > MaxDeviceIDLen {
			return "", nil, status.Errorf(codes.InvalidArgument, "device ids must not exceed %d bytes", MaxDeviceIDLen)
		}
		if !seen[d] {
			if len(ids) == MaxSeriesDevices {
				return "", nil, status.Errorf(codes.InvalidArgument, "device_ids must not exceed %d devices", MaxSeriesDevices)
			}
			seen[d] = true
			ids = append(ids, d)
		}
	}
	// One device needs no merging, and takes the plain single-device query.
	if len(ids) == 1 {
		return ids[0], nil, nil
	}
	return "", ids, nil
}

func seriesCombine(c api2.GetSeriesRequest_Combine) (Combine, error) {
	switch c {
	case api2.GetSeriesRequest_COMBINE_UNSPECIFIED, api2.GetSeriesRequest_COMBINE_MEAN:
		return CombineMean, nil
	case api2.GetSeriesRequest_COMBINE_MAX:
		return CombineMax, nil
	case api2.GetSeriesRequest_COMBINE_SUM:
		return CombineSum, nil
	default:
		return CombineMean, status.Errorf(codes.InvalidArgument, "unknown combine %d", int32(c))
	}
}

// Server implements api2.MetricsServiceServer over a SeriesQuerier.
type Server struct {
	api2.UnimplementedMetricsServiceServer

	logger  *zap.Logger
	querier SeriesQuerier
	now     func() time.Time
}

// NewServer constructs a Server.
func NewServer(logger *zap.Logger, querier SeriesQuerier) *Server {
	return &Server{logger: logger, querier: querier, now: time.Now}
}

// SplitAttribute splits a "<trait>.<leaf>" key at its first dot. The leaf may
// itself contain dots (nested message fields); trait names never do.
func SplitAttribute(attribute string) (trait, leaf string, ok bool) {
	trait, leaf, found := strings.Cut(attribute, ".")
	if !found || trait == "" || leaf == "" {
		return "", "", false
	}
	return trait, leaf, true
}

// GetSeries implements api2.MetricsServiceServer.
func (s *Server) GetSeries(ctx context.Context, req *api2.GetSeriesRequest) (*api2.GetSeriesResponse, error) {
	deviceID, deviceIDs, err := seriesDevices(req)
	if err != nil {
		return nil, err
	}
	combine, err := seriesCombine(req.GetCombine())
	if err != nil {
		return nil, err
	}
	trait, leaf, ok := SplitAttribute(req.GetAttribute())
	if !ok {
		return nil, status.Error(codes.InvalidArgument, `attribute must be "<trait>.<leaf>", e.g. "air_quality.co2_ppm"`)
	}
	if req.GetStart() == nil {
		return nil, status.Error(codes.InvalidArgument, "start is required")
	}
	if req.GetMaxPoints() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_points must not be negative")
	}

	start := req.GetStart().AsTime()
	end := s.now()
	if req.GetEnd() != nil {
		end = req.GetEnd().AsTime()
	}
	if !end.After(start) {
		return nil, status.Error(codes.InvalidArgument, "end must be after start")
	}
	if end.Sub(start) > MaxSeriesRange {
		return nil, status.Errorf(codes.InvalidArgument, "range must not exceed %s", MaxSeriesRange)
	}

	maxPoints := int(req.GetMaxPoints())
	switch {
	case maxPoints == 0:
		maxPoints = DefaultMaxPoints
	case maxPoints > MaxMaxPoints:
		maxPoints = MaxMaxPoints
	}

	res, err := s.querier.Series(ctx, SeriesQuery{
		DeviceID:  deviceID,
		DeviceIDs: deviceIDs,
		Combine:   combine,
		Trait:     trait,
		Field:     leaf,
		Start:     start,
		End:       end,
		MaxPoints: maxPoints,
	})
	if err != nil {
		// A cancelled or timed-out caller is not a store outage, and clients
		// that retry on Unavailable shouldn't retry it.
		switch {
		case errors.Is(err, context.Canceled):
			return nil, status.FromContextError(context.Canceled).Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, status.FromContextError(context.DeadlineExceeded).Err()
		}
		s.logger.Warn("series query failed",
			zap.String("device_id", deviceID), zap.Int("device_count", len(deviceIDs)),
			zap.String("attribute", req.GetAttribute()), zap.Error(err))
		return nil, status.Error(codes.Unavailable, "unable to query metrics store")
	}

	resp := &api2.GetSeriesResponse{
		Points:     make([]*api2.SeriesPoint, 0, len(res.Points)),
		Resolution: durationpb.New(res.Window),
	}
	for _, p := range res.Points {
		resp.Points = append(resp.Points, &api2.SeriesPoint{Time: timestamppb.New(p.Time), Value: p.Value})
	}
	return resp, nil
}
