/*
Copyright 2021 The TestGrid Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"

	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/GoogleCloudPlatform/testgrid/config"
	apipb "github.com/GoogleCloudPlatform/testgrid/pb/api/v1"
	statepb "github.com/GoogleCloudPlatform/testgrid/pb/state"
	"github.com/GoogleCloudPlatform/testgrid/pkg/tabulator"
	"github.com/GoogleCloudPlatform/testgrid/util/gcs"
)

// findDashboardTab locates dashboard tab in config, given a dashboard and tab name.
func findDashboardTab(cfg *cachedConfig, dashboardInput string, tabInput string) (string, string, string, error) {
	if cfg == nil || cfg.Config == nil {
		return "", "", "", errors.New("empty config")
	}

	dashboardKey := config.Normalize(dashboardInput)
	tabKey := config.Normalize(tabInput)

	dashboardName, ok := cfg.NormalDashboard[dashboardKey]
	if !ok {
		return "", "", "", fmt.Errorf("Dashboard {%q} not found", dashboardKey)
	}
	tabName, ok := cfg.NormalDashboardTab[dashboardKey][tabKey]
	if !ok {
		return dashboardName, "", "", fmt.Errorf("Tab {%q} not found", tabKey)
	}

	for _, tab := range cfg.Config.Dashboards[dashboardName].DashboardTab {
		if tab.Name == tabName {
			return dashboardName, tabName, tab.TestGroupName, nil
		}
	}

	return dashboardName, tabName, "", fmt.Errorf("Test group not found")
}

// GroupGrid fetch tab group name grid info (columns, rows, ..etc)
func (s Server) GroupGrid(ctx context.Context, configPath *gcs.Path, groupName string) (*statepb.Grid, error) {
	groupPath, err := configPath.ResolveReference(&url.URL{Path: path.Join(s.GridPathPrefix, groupName)})
	if err != nil {
		return nil, fmt.Errorf("resolve: %v", err)
	}
	grid, _, err := gcs.DownloadGrid(ctx, s.Client, *groupPath)
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	return grid, err
}

// Grid fetch tab and grid info (columns, rows, ..etc)
func (s Server) Grid(ctx context.Context, scope string, dashboardName, tabName, testGroupNanme string) (*statepb.Grid, error) {
	configPath, _, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}
	if s.TabPathPrefix == "" { // TODO(chases2): Delete; all APIs should be configured to use Tabulator now
		return s.GroupGrid(ctx, configPath, testGroupNanme)
	}
	path, err := tabulator.TabStatePath(*configPath, s.TabPathPrefix, dashboardName, tabName)
	if err != nil {
		return nil, fmt.Errorf("tab state path: %v", err)
	}
	grid, _, err := gcs.DownloadGrid(ctx, s.Client, *path)
	return grid, err
}

// decodeRLE decodes the run length encoded data
//   [0, 3, 5, 4] -> [0, 0, 0, 5, 5, 5, 5]
func decodeRLE(encodedData []int32) []int32 {
	var decodedResult []int32
	encodedDataLength := len(encodedData)
	if encodedDataLength%2 == 0 {
		for encodedDataIdx := 0; encodedDataIdx < encodedDataLength; encodedDataIdx += 2 {
			for cellRepeatCount := encodedData[encodedDataIdx+1]; cellRepeatCount > 0; cellRepeatCount-- {
				decodedResult = append(decodedResult, encodedData[encodedDataIdx])
			}
		}
	}
	return decodedResult
}

// ListHeaders returns dashboard tab headers
func (s *Server) ListHeaders(ctx context.Context, req *apipb.ListHeadersRequest) (*apipb.ListHeadersResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cfg, err := s.getConfig(ctx, logrus.WithContext(ctx), req.GetScope())
	if err != nil {
		return nil, err
	}
	cfg.Mutex.RLock()
	defer cfg.Mutex.RUnlock()

	dashboardName, tabName, testGroupName, err := findDashboardTab(cfg, req.GetDashboard(), req.GetTab())
	if err != nil {
		return nil, err
	}

	grid, err := s.Grid(ctx, req.GetScope(), dashboardName, tabName, testGroupName)
	if err != nil {
		return nil, fmt.Errorf("Dashboard {%q} or tab {%q} not found", req.GetDashboard(), req.GetTab())
	}
	if grid == nil {
		return nil, errors.New("grid not found")
	}

	var dashboardTabResponse apipb.ListHeadersResponse
	for _, gColumn := range grid.Columns {
		// TODO(#683): Remove timestamp conversion math
		millis := gColumn.Started
		sec := millis / 1000
		nanos := math.Mod(millis, 1000) * 1e6
		column := apipb.ListHeadersResponse_Header{
			Name:  gColumn.Name,
			Build: gColumn.Build,
			Started: &timestamp.Timestamp{
				Seconds: int64(sec),
				Nanos:   int32(nanos),
			},
			Extra:      gColumn.Extra,
			HotlistIds: gColumn.HotlistIds,
		}
		dashboardTabResponse.Headers = append(dashboardTabResponse.Headers, &column)
	}
	return &dashboardTabResponse, nil
}

// ListHeadersHTTP returns dashboard tab headers
// Response json: ListHeadersResponse
func (s Server) ListHeadersHTTP(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	req := apipb.ListHeadersRequest{
		Scope:     r.URL.Query().Get(scopeParam),
		Dashboard: vars["dashboard"],
		Tab:       vars["tab"],
	}
	resp, err := s.ListHeaders(r.Context(), &req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	s.writeJSON(w, &resp)
}

// ListRows returns dashboard tab rows
func (s *Server) ListRows(ctx context.Context, req *apipb.ListRowsRequest) (*apipb.ListRowsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	// this should be factored out of this function
	cfg, err := s.getConfig(ctx, logrus.WithContext(ctx), req.GetScope())
	if err != nil {
		return nil, err
	}
	cfg.Mutex.RLock()
	defer cfg.Mutex.RUnlock()

	dashboardName, tabName, testGroupName, err := findDashboardTab(cfg, req.GetDashboard(), req.GetTab())
	if err != nil {
		return nil, err
	}

	grid, err := s.Grid(ctx, req.GetScope(), dashboardName, tabName, testGroupName)
	if err != nil {
		return nil, fmt.Errorf("Dashboard {%q} or tab {%q} not found", req.GetDashboard(), req.GetTab())
	}
	if grid == nil {
		return nil, errors.New("grid not found")
	}

	dashboardTabResponse := apipb.ListRowsResponse{
		Rows: make([]*apipb.ListRowsResponse_Row, 0, len(grid.Rows)),
	}
	for _, gRow := range grid.Rows {
		cellsCount := len(gRow.CellIds)
		row := apipb.ListRowsResponse_Row{
			Name:   gRow.Name,
			Issues: gRow.Issues,
			Alert:  gRow.AlertInfo,
			Cells:  make([]*apipb.ListRowsResponse_Cell, 0, cellsCount),
		}
		gRowDecodedResults := decodeRLE(gRow.Results)
		// loop through CellIds, Messages, Icons slices and build cell struct objects
		for cellIdx := 0; cellIdx < cellsCount; cellIdx++ {
			cell := apipb.ListRowsResponse_Cell{
				Result:  gRowDecodedResults[cellIdx],
				CellId:  gRow.CellIds[cellIdx],
				Message: gRow.Messages[cellIdx],
				Icon:    gRow.Icons[cellIdx],
			}
			row.Cells = append(row.Cells, &cell)
		}
		dashboardTabResponse.Rows = append(dashboardTabResponse.Rows, &row)
	}
	return &dashboardTabResponse, nil
}

// ListRowsHTTP returns dashboard tab rows
// Response json: ListRowsResponse
func (s Server) ListRowsHTTP(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	req := apipb.ListRowsRequest{
		Scope:     r.URL.Query().Get(scopeParam),
		Dashboard: vars["dashboard"],
		Tab:       vars["tab"],
	}
	resp, err := s.ListRows(r.Context(), &req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	s.writeJSON(w, &resp)
}
