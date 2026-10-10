package health

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestBuildResponsePassesWhenEverythingIsHealthy(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})
	checkedAt := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	store.UpdateService(ServiceState{ServiceName: "ring", IsHealthy: true, LastChecked: checkedAt})
	store.UpdateService(ServiceState{ServiceName: "sphere", IsHealthy: true, LastChecked: checkedAt})

	response := BuildResponse(store, "https://solian.app")

	if response.Status != StatusPass {
		t.Fatalf("status = %q, want %q", response.Status, StatusPass)
	}
	if status := response.HTTPStatus(); status != http.StatusOK {
		t.Fatalf("HTTPStatus() = %d, want %d", status, http.StatusOK)
	}
	if response.Output != "" {
		t.Fatalf("output = %q, want it omitted for a passing service", response.Output)
	}
	if len(response.Checks) != 2 {
		t.Fatalf("checks = %+v, want one entry per service", response.Checks)
	}
	ring := response.Checks["ring"]
	if len(ring) != 1 {
		t.Fatalf("checks[ring] = %+v, want a single-element array", ring)
	}
	if ring[0].ComponentID != "ring" || ring[0].ComponentType != "component" {
		t.Fatalf("check = %+v, want the service named as a component", ring[0])
	}
	if ring[0].Status != StatusPass || ring[0].Time != "2026-10-04T11:00:00Z" {
		t.Fatalf("check = %+v, want pass at the recorded time", ring[0])
	}
	if response.Links["self"] != "https://solian.app/health" {
		t.Fatalf("links = %+v, want the self link", response.Links)
	}
	if got := ring[0].Links["self"]; got != "https://solian.app/health/ring" {
		t.Fatalf("check links = %+v, want %q", ring[0].Links, "https://solian.app/health/ring")
	}
}

func TestBuildResponseWarnsWhenOnlyNonCoreServicesAreDown(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})
	store.UpdateService(ServiceState{ServiceName: "ring", IsHealthy: true, LastChecked: time.Now()})
	store.UpdateService(ServiceState{ServiceName: "sphere", IsHealthy: false, LastChecked: time.Now()})

	response := BuildResponse(store, "")

	if response.Status != StatusWarn {
		t.Fatalf("status = %q, want %q", response.Status, StatusWarn)
	}
	if status := response.HTTPStatus(); status != http.StatusOK {
		t.Fatalf("HTTPStatus() = %d, want 2xx for warn", status)
	}
	if response.Output == "" {
		t.Fatal("expected output to describe the warn state")
	}
	if got := response.Checks["sphere"][0].Status; got != StatusFail {
		t.Fatalf("checks[sphere] status = %q, want %q", got, StatusFail)
	}
	if response.Links != nil {
		t.Fatalf("links = %+v, want no links without a self URL", response.Links)
	}
}

func TestBuildResponseFailsWhenCoreServiceIsDown(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})
	store.UpdateService(ServiceState{ServiceName: "ring", IsHealthy: false, LastChecked: time.Now()})

	response := BuildResponse(store, "")

	if response.Status != StatusFail {
		t.Fatalf("status = %q, want %q", response.Status, StatusFail)
	}
	if status := response.HTTPStatus(); status != http.StatusServiceUnavailable {
		t.Fatalf("HTTPStatus() = %d, want 5xx for fail", status)
	}
	if response.Output == "" {
		t.Fatal("expected output to describe the failure")
	}
}

func TestBuildResponseFailsWhenCoreServiceWasNeverChecked(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})

	response := BuildResponse(store, "")

	if response.Status != StatusFail || response.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("response = %+v, want a failing document before the first check", response)
	}
}

func TestBuildResponseMarshalsDraftFieldNames(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})
	store.UpdateService(ServiceState{ServiceName: "ring", IsHealthy: true, LastChecked: time.Unix(0, 0).UTC()})
	store.UpdateService(ServiceState{ServiceName: "sphere", IsHealthy: false, LastChecked: time.Unix(0, 0).UTC()})

	payload, err := json.Marshal(BuildResponse(store, "https://solian.app"))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var document struct {
		Status string `json:"status"`
		Checks map[string][]struct {
			ComponentID   string `json:"componentId"`
			ComponentType string `json:"componentType"`
			Status        string `json:"status"`
			Time          string `json:"time"`
		} `json:"checks"`
		Links map[string]string `json:"links"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if document.Status != StatusWarn {
		t.Fatalf("status = %q, want %q", document.Status, StatusWarn)
	}
	if document.Checks["ring"][0].Status != StatusPass || document.Checks["sphere"][0].Status != StatusFail {
		t.Fatalf("checks = %+v, want per-service statuses", document.Checks)
	}
	if document.Checks["ring"][0].Time != "1970-01-01T00:00:00Z" {
		t.Fatalf("time = %q, want an RFC3339 timestamp", document.Checks["ring"][0].Time)
	}
	if document.Links["self"] != "https://solian.app/health" {
		t.Fatalf("links = %+v, want the self link", document.Links)
	}
	if MediaTypeHealthJSON != "application/health+json" {
		t.Fatalf("MediaTypeHealthJSON = %q", MediaTypeHealthJSON)
	}
}

func TestBuildServiceResponsePassesForAHealthyService(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})
	store.UpdateService(ServiceState{ServiceName: "ring", IsHealthy: true, LastChecked: time.Unix(0, 0).UTC()})
	store.UpdateService(ServiceState{ServiceName: "sphere", IsHealthy: false, LastChecked: time.Unix(0, 0).UTC()})

	response, tracked := BuildServiceResponse(store, "ring", "https://api.solian.app")

	if !tracked {
		t.Fatal("ring is tracked, want tracked = true")
	}
	if response.Status != StatusPass || response.HTTPStatus() != http.StatusOK {
		t.Fatalf("response = %+v, want a passing document", response)
	}
	if len(response.Checks) != 1 {
		t.Fatalf("checks = %+v, want only the requested service", response.Checks)
	}
	if response.Checks["ring"][0].Status != StatusPass {
		t.Fatalf("check = %+v, want pass", response.Checks["ring"][0])
	}
	if response.Links["self"] != "https://api.solian.app/health/ring" {
		t.Fatalf("links = %+v, want the per-service self link", response.Links)
	}
}

func TestBuildServiceResponseFailsForAnUnhealthyService(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})
	store.UpdateService(ServiceState{ServiceName: "ring", IsHealthy: true, LastChecked: time.Unix(0, 0).UTC()})
	store.UpdateService(ServiceState{ServiceName: "sphere", IsHealthy: false, LastChecked: time.Unix(0, 0).UTC()})

	response, tracked := BuildServiceResponse(store, "sphere", "https://api.solian.app")

	if !tracked {
		t.Fatal("sphere is tracked, want tracked = true")
	}
	if response.Status != StatusFail || response.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("response = %+v, want a failing document", response)
	}
	if response.Output == "" {
		t.Fatal("expected output to describe the failing service")
	}
}

func TestBuildServiceResponseReportsUntrackedServices(t *testing.T) {
	store := NewReadinessStore([]string{"ring"})
	store.UpdateService(ServiceState{ServiceName: "ring", IsHealthy: true, LastChecked: time.Unix(0, 0).UTC()})

	response, tracked := BuildServiceResponse(store, "ghost", "https://api.solian.app")

	if tracked {
		t.Fatal("ghost is not tracked, want tracked = false")
	}
	if response.Status != StatusFail {
		t.Fatalf("status = %q, want %q so the caller can answer 404", response.Status, StatusFail)
	}
	if response.Output == "" {
		t.Fatal("expected output to name the unknown service")
	}
}
