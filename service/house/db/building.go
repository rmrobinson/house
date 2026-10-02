package db

// Location captures the latitude/longitude coordinates of a specific place.
type Location struct {
	Latitude  float64
	Longitude float64
}

// Building describes a physical building and the properties of it.
type Building struct {
	ID       string
	Name     string
	TZ       string
	Location Location
	// AvailableModes are the values Mode may be set to via SetBuildingMode.
	// Nil/empty means no mode is currently settable.
	AvailableModes []string
	// Mode is the building's current mode (one of AvailableModes, or "" if
	// never set) - set via SetBuildingMode, not CreateBuilding/UpdateBuilding.
	Mode string
	// Version is an opaque token minted fresh on every create/update, used
	// for optimistic concurrency the same way device.Device.version is.
	Version string
}
