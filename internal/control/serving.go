package control

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"
)

// Serving: a machine answering the marketplace's inference API while it is
// listed and waiting to be rented, and opted in by its host (package serving).
// A rental always comes first: when a client rents the machine, /provision takes
// it out of the inference pool (no new requests, running ones finish) before it
// starts the rental. /status keeps saying "free" while the machine serves, so
// the order page never stops offering it.
//
//	POST /serve/start {serving_id, model, credential}
//	                  -> 202 {"status": "loading", ...ServingStatus}
//	                  the engine starts in the background; /status shows it.
//	POST /serve/stop  {serving_id, drain_seconds}
//	                  -> 200 {"status": "draining"|"stopped", ...}
//	                  no new requests; running ones finish within drain_seconds
//	                  (at most MaxDrainSeconds, the default), then the engine stops
//	                  and the GPU is checked clear.
//
// Both exist only where the provisioner can serve (ServingController); anywhere
// else they are 404, which the control plane reads as a refusal, as with the
// pair endpoints. The credential is what the machine's worker presents to the
// gateway for this serving session: it is never logged, stored on disk or
// reported back.

// Serving states, in ServingStatus.State.
const (
	ServingLoading  = "loading"
	ServingReady    = "ready"
	ServingDraining = "draining"
)

// MaxDrainSeconds is the longest running requests are given to finish when
// serving stops, and the default.
const MaxDrainSeconds = 60

// ServingStatus is what the machine is serving, for /status's "serving".
type ServingStatus struct {
	ServingID string `json:"serving_id"`
	Model     string `json:"model"`
	State     string `json:"state"`
	// Since (unix) is when the current state began.
	Since    int64 `json:"since"`
	InFlight int   `json:"in_flight"`
}

// ServingWeights is one model's weights on this machine, for the capability.
type ServingWeights struct {
	Model string `json:"model"`
	// State is "absent", "partial", "downloading" or "verified".
	State string `json:"state"`
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
	Error string `json:"error,omitempty"`
}

// ServeStartRequest is POST /serve/start's body.
type ServeStartRequest struct {
	ServingID  string `json:"serving_id"`
	Model      string `json:"model"`
	Credential string `json:"credential"`
}

type serveStopReq struct {
	ServingID    string `json:"serving_id"`
	DrainSeconds *int   `json:"drain_seconds,omitempty"`
}

// ServingController is a provisioner that can serve the inference API.
type ServingController interface {
	// ServeStart starts serving in the background; the answer is the session
	// as it starts. Refused (a RequestError) when the host has not opted in or
	// has switched serving off, the machine is rented or in use, the model is
	// not one this release serves or its weights are not verified.
	ServeStart(req ServeStartRequest) (*ServingStatus, error)
	// ServeStop stops the session servingID: no new requests, then the engine
	// stops after at most drain. nil: nothing is serving any more.
	ServeStop(servingID string, drain time.Duration) (*ServingStatus, error)
	// Serving is the current session, or nil.
	Serving() *ServingStatus
}

var (
	servingModelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	// A credential is opaque to the agent: printable, no whitespace, long
	// enough to be a secret and short enough to be one.
	credentialPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{32,512}$`)
)

func (s *Server) handleServeStart(sc ServingController) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The same refusal as /provision: serving runs in the rental runtime,
		// so a machine that cannot host a rental cannot serve either.
		if c := s.prov.Capability(); !c.Ready {
			http.Error(w, "this machine cannot serve: it cannot host a rental", http.StatusServiceUnavailable)
			return
		}
		var req ServeStartRequest
		if err := decodeStrict(r, &req); err != nil {
			writeError(w, err)
			return
		}
		switch {
		case !ValidRentalID(req.ServingID):
			writeError(w, Invalid("serving_id is required"))
			return
		case !servingModelPattern.MatchString(req.Model):
			writeError(w, Invalid("model is required"))
			return
		case !credentialPattern.MatchString(req.Credential):
			writeError(w, Invalid("credential is required"))
			return
		}
		st, err := sc.ServeStart(req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeServing(w, http.StatusAccepted, ServingLoading, st)
	}
}

func (s *Server) handleServeStop(sc ServingController) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req serveStopReq
		if err := decodeStrict(r, &req); err != nil {
			writeError(w, err)
			return
		}
		if !ValidRentalID(req.ServingID) {
			writeError(w, Invalid("serving_id is required"))
			return
		}
		drain := MaxDrainSeconds
		if req.DrainSeconds != nil {
			drain = *req.DrainSeconds
		}
		if drain < 0 || drain > MaxDrainSeconds {
			writeError(w, Invalid("drain_seconds must be 0 to %d", MaxDrainSeconds))
			return
		}
		st, err := sc.ServeStop(req.ServingID, time.Duration(drain)*time.Second)
		if err != nil {
			writeError(w, err)
			return
		}
		if st == nil {
			writeJSON(w, map[string]string{"status": "stopped"})
			return
		}
		writeServing(w, http.StatusOK, st.State, st)
	}
}

// writeServing answers with a session's status, the session's fields beside it.
func writeServing(w http.ResponseWriter, code int, status string, st *ServingStatus) {
	body := map[string]interface{}{"status": status}
	if st != nil {
		body["serving_id"] = st.ServingID
		body["model"] = st.Model
		body["state"] = st.State
		body["since"] = st.Since
		body["in_flight"] = st.InFlight
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}
