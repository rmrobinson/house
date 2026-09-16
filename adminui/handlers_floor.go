package main

import (
	"fmt"
	"net/http"
	"strconv"

	api2 "github.com/rmrobinson/house/api"
)

func (s *Server) handleFloorCreate(w http.ResponseWriter, r *http.Request) {
	buildingID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}

	name := r.FormValue("name")
	sortOrder, _ := strconv.Atoi(r.FormValue("sort_order"))

	_, err := s.house.CreateFloor(r.Context(), &api2.CreateFloorRequest{
		BuildingId: buildingID,
		Name:       name,
		SortOrder:  int32(sortOrder),
	})
	flash, isError := successOrError(err, fmt.Sprintf("Floor %q created", name))

	data, loadErr := s.loadBuildingPageData(r, buildingID)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "building", data, flash, isError)
}

type floorPageData struct {
	Floor    floorView
	Building buildingView
	Rooms    []roomView
}

func (s *Server) loadFloorPageData(r *http.Request, id string) (floorPageData, error) {
	ctx := r.Context()
	f, err := s.house.GetFloor(ctx, &api2.GetFloorRequest{Id: id})
	if err != nil {
		return floorPageData{}, err
	}
	b, err := s.house.GetBuilding(ctx, &api2.GetBuildingRequest{Id: f.GetBuildingId()})
	if err != nil {
		return floorPageData{}, err
	}
	rooms, err := s.listRoomsByFloor(ctx, id)
	if err != nil {
		return floorPageData{}, err
	}

	data := floorPageData{Floor: floorToView(f), Building: buildingToView(b)}
	for _, room := range rooms {
		data.Rooms = append(data.Rooms, roomToView(room))
	}
	return data, nil
}

func (s *Server) handleFloorGet(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadFloorPageData(r, r.PathValue("id"))
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "floor", data)
}

func (s *Server) handleFloorUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}

	sortOrder, _ := strconv.Atoi(r.FormValue("sort_order"))
	_, err := s.house.UpdateFloor(r.Context(), &api2.UpdateFloorRequest{
		Id:        id,
		Version:   r.FormValue("version"),
		Name:      r.FormValue("name"),
		SortOrder: int32(sortOrder),
	})
	flash, isError := successOrError(err, "Floor updated")

	data, loadErr := s.loadFloorPageData(r, id)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "floor", data, flash, isError)
}

func (s *Server) handleFloorDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Needed either way: to redirect up to the right building on success, or
	// to re-render the current floor page (still showing its child rooms) on
	// a blocked delete.
	data, loadErr := s.loadFloorPageData(r, id)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}

	if _, err := s.house.DeleteFloor(r.Context(), &api2.DeleteFloorRequest{Id: id}); err != nil {
		s.respond(w, "floor", data, grpcMessage(err), true)
		return
	}
	redirectAfterDelete(w, "/buildings/"+data.Building.ID)
}

func (s *Server) handleRoomCreate(w http.ResponseWriter, r *http.Request) {
	floorID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}

	name := r.FormValue("name")
	roomType, _ := strconv.Atoi(r.FormValue("type"))

	_, err := s.house.CreateRoom(r.Context(), &api2.CreateRoomRequest{
		FloorId: floorID,
		Config: &api2.Room_Config{
			Name: name,
			Type: int32(roomType),
		},
	})
	flash, isError := successOrError(err, fmt.Sprintf("Room %q created", name))

	data, loadErr := s.loadFloorPageData(r, floorID)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "floor", data, flash, isError)
}
