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
	ID         string
	BuildingID string
	Name       string
	Type       RoomType

	Devices []Device
}
