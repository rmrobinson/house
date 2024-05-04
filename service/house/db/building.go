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
}
