package main

import (
	"fmt"
	"net/http"

	api2 "github.com/rmrobinson/house/api"
)

type buildingsPageData struct {
	Buildings []buildingView
}

func (s *Server) loadBuildingsPageData(r *http.Request) (buildingsPageData, error) {
	buildings, err := s.listBuildings(r.Context())
	if err != nil {
		return buildingsPageData{}, err
	}
	data := buildingsPageData{}
	for _, b := range buildings {
		data.Buildings = append(data.Buildings, buildingToView(b))
	}
	return data, nil
}

func (s *Server) handleBuildingsList(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadBuildingsPageData(r)
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "buildings", data)
}

func (s *Server) handleBuildingCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.httpError(w, r, err)
		return
	}

	name := r.FormValue("name")
	_, err := s.house.CreateBuilding(r.Context(), &api2.CreateBuildingRequest{
		Config: &api2.Building_Config{
			Name: name,
			Tz:   r.FormValue("tz"),
		},
	})

	flash, isError := successOrError(err, fmt.Sprintf("Building %q created", name))

	data, loadErr := s.loadBuildingsPageData(r)
	if loadErr != nil {
		s.httpError(w, r, loadErr)
		return
	}
	s.respond(w, "buildings", data, flash, isError)
}

type buildingPageData struct {
	Building buildingView
	Floors   []floorView
}

func (s *Server) loadBuildingPageData(r *http.Request, id string) (buildingPageData, error) {
	ctx := r.Context()
	b, err := s.house.GetBuilding(ctx, &api2.GetBuildingRequest{Id: id})
	if err != nil {
		return buildingPageData{}, err
	}
	floors, err := s.listFloors(ctx, id)
	if err != nil {
		return buildingPageData{}, err
	}

	data := buildingPageData{Building: buildingToView(b)}
	for _, f := range floors {
		data.Floors = append(data.Floors, floorToView(f))
	}
	return data, nil
}

func (s *Server) handleBuildingGet(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadBuildingPageData(r, r.PathValue("id"))
	if err != nil {
		s.httpError(w, r, err)
		return
	}
	s.renderPage(w, "building", data)
}

func (s *Server) handleBuildingDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.house.DeleteBuilding(r.Context(), &api2.DeleteBuildingRequest{Id: id})
	if err != nil {
		data, loadErr := s.loadBuildingPageData(r, id)
		if loadErr != nil {
			s.httpError(w, r, loadErr)
			return
		}
		s.respond(w, "building", data, grpcMessage(err), true)
		return
	}
	redirectAfterDelete(w, "/buildings")
}

// successOrError is the common "call the RPC, decide the flash message"
// shape shared by every create/update handler in this app.
func successOrError(err error, successMsg string) (msg string, isError bool) {
	if err != nil {
		return grpcMessage(err), true
	}
	return successMsg, false
}
