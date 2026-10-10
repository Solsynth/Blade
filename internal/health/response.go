package health

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// MediaTypeHealthJSON is the media type registered for health check responses
// by draft-inadarei-api-health-check-06, section 7.
const MediaTypeHealthJSON = "application/health+json"

// Health document status values defined in
// draft-inadarei-api-health-check-06, section 3.1.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
	StatusFail = "fail"
)

const (
	serviceID          = "blade"
	serviceDescription = "Solar Network API gateway"
)

// healthPath is where the gateway serves its health document; the per-service
// form appends "/{service}".
const healthPath = "/health"

// Check is one element of a "checks" entry: the health of a single
// sub-component or downstream dependency, per section 4 of the draft.
type Check struct {
	ComponentID   string            `json:"componentId,omitempty"`
	ComponentType string            `json:"componentType,omitempty"`
	Status        string            `json:"status"`
	Time          string            `json:"time,omitempty"`
	Links         map[string]string `json:"links,omitempty"`
}

// Response is a health check document as defined by
// draft-inadarei-api-health-check-06, section 3.
type Response struct {
	Status      string             `json:"status"`
	ServiceID   string             `json:"serviceId,omitempty"`
	Description string             `json:"description,omitempty"`
	Output      string             `json:"output,omitempty"`
	Checks      map[string][]Check `json:"checks,omitempty"`
	Links       map[string]string  `json:"links,omitempty"`
}

// HTTPStatus maps the document status onto the response code required by the
// draft: "fail" MUST use a 4xx-5xx code, "pass" and "warn" MUST use 2xx-3xx.
func (r Response) HTTPStatus() int {
	if r.Status == StatusFail {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// BuildResponse renders the readiness snapshot as a health check document.
// baseURL, when non-empty, is published as the "self" link and used to build
// the per-service link of every check, so clients can re-check a single
// service by response code instead of parsing the document.
func BuildResponse(store *ReadinessStore, baseURL string) Response {
	states := store.GetAllStates()

	response := Response{
		Status:      StatusPass,
		ServiceID:   serviceID,
		Description: serviceDescription,
		Checks:      make(map[string][]Check, len(states)),
	}

	allHealthy := true
	for name, state := range states {
		if !state.IsHealthy {
			allHealthy = false
		}
		response.Checks[name] = []Check{buildCheck(name, state, baseURL)}
	}

	response.Status, response.Output = overallStatus(store, allHealthy)

	if baseURL != "" {
		response.Links = map[string]string{"self": baseURL + healthPath}
	}

	return response
}

// BuildSummaryResponse renders only the gateway's overall status, without the
// per-service checks map. It is what an unauthenticated caller gets, so the
// document never names or reports the services behind the gateway.
func BuildSummaryResponse(store *ReadinessStore) Response {
	allHealthy := true
	for _, state := range store.GetAllStates() {
		if !state.IsHealthy {
			allHealthy = false
		}
	}
	status, output := overallStatus(store, allHealthy)

	return Response{
		Status:      status,
		ServiceID:   serviceID,
		Description: serviceDescription,
		Output:      output,
	}
}

// overallStatus maps the readiness snapshot onto the document status and its
// human-readable output, shared by the full and summary documents.
func overallStatus(store *ReadinessStore, allHealthy bool) (string, string) {
	switch {
	case !store.IsCoreServiceHealthy():
		return StatusFail, "one or more core services are unhealthy"
	case !allHealthy:
		return StatusWarn, "one or more non-core services are unhealthy"
	}
	return StatusPass, ""
}

// BuildServiceResponse renders a single service's slice of the snapshot, the
// document served by GET /health/{service}. The second return value reports
// whether the service is tracked at all; an untracked service is a failing
// document too, and the caller answers it with 404.
func BuildServiceResponse(store *ReadinessStore, service, baseURL string) (Response, bool) {
	response := Response{
		ServiceID:   serviceID,
		Description: serviceDescription,
	}
	if baseURL != "" {
		response.Links = map[string]string{"self": serviceURL(baseURL, service)}
	}

	state, tracked := store.GetServiceState(service)
	if !tracked {
		response.Status = StatusFail
		response.Output = fmt.Sprintf("service %q is not tracked", service)
		return response, false
	}

	response.Status = StatusPass
	if !state.IsHealthy {
		response.Status = StatusFail
		response.Output = fmt.Sprintf("service %q is unhealthy", service)
	}
	response.Checks = map[string][]Check{service: {buildCheck(service, state, baseURL)}}
	return response, true
}

func buildCheck(name string, state ServiceState, baseURL string) Check {
	check := Check{
		ComponentID:   name,
		ComponentType: "component",
		Status:        StatusPass,
		Time:          state.LastChecked.UTC().Format(time.RFC3339),
	}
	if !state.IsHealthy {
		check.Status = StatusFail
	}
	if baseURL != "" {
		check.Links = map[string]string{"self": serviceURL(baseURL, name)}
	}
	return check
}

func serviceURL(baseURL, service string) string {
	return baseURL + healthPath + "/" + url.PathEscape(service)
}
