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
	// Version is an opaque token minted fresh on every create/update, used
	// for optimistic concurrency the same way device.Device.version is.
	Version string

	Devices []Device
}
