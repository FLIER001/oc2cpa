package main

// management_test.go verifies the management surface offline: the register
// payload decodes against the host's lowercase wire keys, routes resolve to
// the expected handlers, and the usage endpoints expose stored entries.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestManagementRegistrationRoutes(t *testing.T) {
	payload, err := handleManagementRoutes([]byte(`{
		"Plugin": {"Name": "x"},
		"BasePath": "/v0/management",
		"ResourceBasePath": "/v0/resource/plugins/oc2cpa"
	}`))
	if err != nil {
		t.Fatalf("handleManagementRoutes: %v", err)
	}
	if got := loadedManagementBasePath(); got != "/v0/management" {
		t.Fatalf("management base not cached: %q", got)
	}
	if got := loadedResourceBasePath(); got != "/v0/resource/plugins/oc2cpa" {
		t.Fatalf("resource base not cached: %q", got)
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal register payload: %v", errMarshal)
	}
	var decoded struct {
		Routes []struct {
			Method string
			Path   string
		} `json:"routes"`
		Resources []struct {
			Path string
			Menu string
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode register payload: %v", err)
	}
	if len(decoded.Routes) != 2 || decoded.Routes[0].Method != http.MethodGet {
		t.Fatalf("unexpected routes: %s", raw)
	}
	if len(decoded.Resources) != 1 || decoded.Resources[0].Path != "/panel" || decoded.Resources[0].Menu == "" {
		t.Fatalf("unexpected resources: %s", raw)
	}
	for _, route := range decoded.Routes {
		if route.Path == "" || route.Path[0] != '/' {
			t.Fatalf("route path must stay relative under the prefix: %q", route.Path)
		}
	}
}

func TestHandleManagementResourcePanel(t *testing.T) {
	req, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/" + pluginRouteID + "/panel",
	})
	resp, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement resource: %v", err)
	}
	if resp.StatusCode != http.StatusOK || len(resp.Body) == 0 {
		t.Fatalf("panel response bad: status=%d", resp.StatusCode)
	}
	if string(resp.Body[:9]) != "<!doctype" {
		t.Fatalf("panel must return the embedded HTML page, got: %q", resp.Body[:min(len(resp.Body), 40)])
	}
}

func TestHandleManagementUnknownRoute(t *testing.T) {
	req, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + pluginRouteID + "/usage/unknown",
	})
	resp, err := handleManagement(req)
	if err != nil {
		t.Fatalf("handleManagement unknown: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route must 404, got: %d", resp.StatusCode)
	}
}

func TestUsageSummaryAndRecordsEndpoints(t *testing.T) {
	usageStoreMu.Lock()
	usageStore = []usageEntry{
		{Time: "2026-10-20T10:00:00+08:00", Model: "glm-5.3-flash-free", Stream: true, TotalTokens: 10, InputTokens: 8, OutputTokens: 2},
		{Time: "2026-10-20T10:01:00+08:00", Model: "big-pickle", Stream: false, Failed: true, Status: 500, TotalTokens: 3, InputTokens: 3},
	}
	usageStoreMu.Unlock()

	req, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + pluginRouteID + "/usage/summary",
	})
	resp, err := handleManagement(req)
	if err != nil {
		t.Fatalf("summary endpoint: %v", err)
	}
	var summary usageSummary
	if err := json.Unmarshal(resp.Body, &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Records != 2 || summary.All.Requests != 2 || summary.Streaming != 1 || summary.NonStreaming != 1 {
		t.Fatalf("summary aggregates wrong: %+v", summary)
	}
	if len(summary.Models) != 2 {
		t.Fatalf("per-model rows missing: %+v", summary.Models)
	}

	req, _ = json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + pluginRouteID + "/usage/records",
		Query:  map[string][]string{"limit": {"1"}},
	})
	resp, err = handleManagement(req)
	if err != nil {
		t.Fatalf("records endpoint: %v", err)
	}
	var records usageRecordsResponse
	if err := json.Unmarshal(resp.Body, &records); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	if records.Count != 1 || len(records.Records) != 1 || records.Records[0].TotalTokens != 3 {
		t.Fatalf("limit not respected: %+v", records)
	}
}
