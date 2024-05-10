package db

// Device captures metadata linking the physical location of a device to a room.
type Device struct {
	ID     string
	RoomID string
}
