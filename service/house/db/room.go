package db

// RoomType describes the primary purpose of the room. Useful for selecting an icon to show the room.
type RoomType int

const (
	Unspecified RoomType = iota
	Bedroom
	Bathroom
	Office
	Foyer
	Landing
	Porch
	Kitchen
	LivingRoom
	DiningRoom
	FamilyRoom
	FurnaceRoom
	UtilityRoom
)

// AggregationStrategy selects how a room's linked Sensor devices are
// combined into one of its Properties. AggregationUnspecified means "use
// the metric's documented default" - see Room.Aggregation.
type AggregationStrategy int32

const (
	AggregationUnspecified AggregationStrategy = iota
	AggregationLatest
	AggregationAverage
	AggregationMin
	AggregationMax
	AggregationSum
	AggregationAny
)

// AggregationConfig overrides the per-metric aggregation default for a room.
// A field left at AggregationUnspecified still falls back to that metric's
// documented default - see Room.Aggregation.
type AggregationConfig struct {
	OccupancyStrategy   AggregationStrategy
	TemperatureStrategy AggregationStrategy
	LightStrategy       AggregationStrategy
	AirQualityStrategy  AggregationStrategy
	PowerStrategy       AggregationStrategy
}

// Room describes a part of the house with a logical purpose, usually a separate space.
type Room struct {
	ID      string
	FloorID string
	// BuildingID is denormalized from the owning Floor for query
	// convenience - it's derived server-side from FloorID, never set
	// independently.
	BuildingID string
	Name       string
	Type       RoomType
	// Aggregation overrides the room's per-metric aggregation strategy. Nil
	// means no override has ever been configured for this room - every
	// metric uses its documented default, the same net effect as a non-nil
	// AggregationConfig whose fields are all AggregationUnspecified.
	Aggregation *AggregationConfig
	// Version is an opaque token minted fresh on every create/update, used
	// for optimistic concurrency the same way device.Device.version is.
	Version string

	Devices []Device
}
