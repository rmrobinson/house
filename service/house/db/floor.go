package db

// Floor describes one level of a Building - a physical floor, or a distinct
// exterior area (e.g. "Front Yard", "Back Yard"). Rooms belong to exactly
// one Floor.
type Floor struct {
	ID         string
	Name       string
	SortOrder  int32
	BuildingID string
	// Version is an opaque token minted fresh on every create/update, used
	// for optimistic concurrency the same way device.Device.version is.
	Version string
}
