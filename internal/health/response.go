package health

import (
	"net/http"
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

// Check is one element of a "checks" entry: the health of a single
// sub-component or downstream dependency, per section 4 of the draft.
type Check struct {
	ComponentID   string `json:"componentId,omitempty"`
	ComponentType string `json:"componentType,omitempty"`
	Status        string `json:"status"`
	Time          string `json:"time,omitempty"`
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
// selfURL, when non-empty, is published as the "self" link so clients can
// re-check the endpoint by response code.
func BuildResponse(store *ReadinessStore, selfURL string) Response {
	states := store.GetAllStates()

	response := Response{
		Status:      StatusPass,
		ServiceID:   serviceID,
		Description: serviceDescription,
		Checks:      make(map[string][]Check, len(states)),
	}

	allHealthy := true
	for name, state := range states {
		checkStatus := StatusPass
		if !state.IsHealthy {
			checkStatus = StatusFail
			allHealthy = false
		}
		response.Checks[name] = []Check{{
			ComponentID:   name,
			ComponentType: "component",
			Status:        checkStatus,
			Time:          state.LastChecked.UTC().Format(time.RFC3339),
		}}
	}

	switch {
	case !store.IsCoreServiceHealthy():
		response.Status = StatusFail
		response.Output = "one or more core services are unhealthy"
	case !allHealthy:
		response.Status = StatusWarn
		response.Output = "one or more non-core services are unhealthy"
	}

	if selfURL != "" {
		response.Links = map[string]string{"self": selfURL}
	}

	return response
}
