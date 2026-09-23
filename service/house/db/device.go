package db

// Device captures metadata linking the physical location of a device to a room.
type Device struct {
	ID     string
	RoomID string
	// Version is an opaque token minted fresh on every link/move, used for
	// optimistic concurrency the same way Building/Floor/Room.Version is.
	Version string
}
