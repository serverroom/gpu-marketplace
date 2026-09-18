package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Capability is whether this machine can actually host a rental and, when it
// cannot, every reason why. It is reported to the control plane at register
// and on every start, and it gates /provision here as well: a machine that
// cannot isolate a tenant refuses the rental instead of accepting it.
//
// This exists because v0.1.5 and earlier wired a stub provisioner that logged,
// set a flag and answered success. Nothing in the control API could tell that
// stub from a machine that had really booted a microVM, so a rental could be
// "delivered" onto nothing. There is no stub any more; a machine is either
// ready, or it says why not.
type Capability struct {
	Ready        bool     `json:"ready"`
	Kind         string   `json:"kind"`
	Reasons      []string `json:"reasons,omitempty"`
	AgentVersion string   `json:"agent_version,omitempty"`
	// UnifiedMemory: the GPU has no memory of its own and shares the machine's
	// pool (a GB10). A rental gets that pool as system and GPU memory at once.
	UnifiedMemory bool `json:"unified_memory,omitempty"`
	// VMUser: the login the rental's VM accepts the renter's key for. Absent from
	// agents up to v0.1.7, whose VMs log in as renter.
	VMUser string `json:"vm_user,omitempty"`
	// Identity is what the machine says it is (DMI) and whether the agent
	// confirmed it as an NVIDIA DGX Spark; Interconnect is whether it can be
	// half of a linked pair. Absent from agents up to v0.1.9, and Identity
	// from hosts that are not Linux.
	Identity     *Identity     `json:"identity,omitempty"`
	Interconnect *Interconnect `json:"interconnect,omitempty"`
}

// Provisioner is the agent action layer the control channel drives. The real
// QEMU/VFIO implementation is the provisioner package on top of internal/vmrt;
// tests use a fake.
type Provisioner interface {
	Provision(rentalID, renterPubkey string) error
	Teardown(rentalID string) error
	Status() string
	Capability() Capability
}

// Host is what the control channel asks of the agent beyond rentals: its
// version, its last update, updating it, and stopping hosting when the host
// removed the machine in the control panel. Without one, /update and
// /withdrawn answer 501.
type Host interface {
	AgentVersion() string
	// UpdateRecord is update.json, or nil.
	UpdateRecord() interface{}
	// Update checks the request and starts the update in the background.
	Update(version string) (from, to string, err error)
	// Withdraw stops hosting for good (until registered again).
	Withdraw() error
}

// StatusError is a refusal with the HTTP status to answer it with.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

// errorCode is the status for a Host error: its own when it carries one (a
// StatusError, or anything with an HTTPStatus method), else 500.
func errorCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	var coded interface{ HTTPStatus() int }
	if errors.As(err, &coded) {
		return coded.HTTPStatus()
	}
	return http.StatusInternalServerError
}

// rentalIDPattern is what a rental id may look like. The id becomes part of a
// disk path and a command argument on the host, so anything else is refused at
// the door rather than trusted to every place it ends up.
var rentalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// ValidRentalID reports whether id is safe to use in host paths and commands.
func ValidRentalID(id string) bool { return rentalIDPattern.MatchString(id) }

// Server is the agent control-channel HTTP server. It binds 127.0.0.1 only; the
// control plane reaches it through the relay's reverse tunnel. Every command is
// bearer-authenticated with the token shared at register time (end-to-end — the
// relay carries it as opaque bytes).
type Server struct {
	token      string
	prov       Provisioner
	host       Host
	httpServer *http.Server
	listenAddr string
}

// SetHost gives the channel the agent's own controls (update, withdrawn).
func (s *Server) SetHost(h Host) { s.host = h }

// Handler is the channel's HTTP handler, authentication included.
func (s *Server) Handler() http.Handler { return s.httpServer.Handler }

// New builds the control server. listenAddr should be a loopback address.
func New(listenAddr, token string, prov Provisioner) *Server {
	mux := http.NewServeMux()
	s := &Server{
		token:      token,
		prov:       prov,
		listenAddr: listenAddr,
		httpServer: &http.Server{
			Addr:         listenAddr,
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 180 * time.Second,
		},
	}
	mux.HandleFunc("/provision", s.auth(s.handleProvision))
	mux.HandleFunc("/teardown", s.auth(s.handleTeardown))
	mux.HandleFunc("/status", s.auth(s.handleStatus))
	mux.HandleFunc("/update", s.auth(s.handleUpdate))
	mux.HandleFunc("/withdrawn", s.auth(s.handleWithdrawn))
	mux.HandleFunc("/health", s.handleHealth)
	// The pair endpoints exist only where the provisioner has the pair
	// runtime; anywhere else they are 404, which the control plane reads as a
	// refusal.
	if v, ok := prov.(LinkVerifier); ok {
		mux.HandleFunc("/link/verify", s.auth(s.handleLinkVerify(v)))
	}
	return s
}

// Start begins serving in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.listenAddr, err)
	}
	log.Printf("Control channel listening on %s", s.listenAddr)
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("control server error: %v", err)
		}
	}()
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop() error { return s.httpServer.Close() }

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" || r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

type provisionReq struct {
	RentalID     string `json:"rental_id"`
	RenterPubkey string `json:"renter_pubkey"`
}

func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Refuse before reading anything: a machine that cannot host must never be
	// the one deciding a rental went well.
	if c := s.prov.Capability(); !c.Ready {
		http.Error(w, "this machine cannot host a rental: "+strings.Join(c.Reasons, "; "),
			http.StatusServiceUnavailable)
		return
	}
	var req provisionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !ValidRentalID(req.RentalID) || strings.TrimSpace(req.RenterPubkey) == "" {
		http.Error(w, "rental_id and renter_pubkey are required", http.StatusBadRequest)
		return
	}
	if err := s.prov.Provision(req.RentalID, req.RenterPubkey); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "provisioning"})
}

type teardownReq struct {
	RentalID string `json:"rental_id"`
}

func (s *Server) handleTeardown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req teardownReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !ValidRentalID(req.RentalID) {
		http.Error(w, "rental_id is required", http.StatusBadRequest)
		return
	}
	if err := s.prov.Teardown(req.RentalID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "wiping"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	c := s.prov.Capability()
	body := map[string]interface{}{
		"status":        s.prov.Status(),
		"ready":         c.Ready,
		"kind":          c.Kind,
		"reasons":       c.Reasons,
		"agent_version": c.AgentVersion,
		"update":        nil,
	}
	if s.host != nil {
		body["agent_version"] = s.host.AgentVersion()
		body["update"] = s.host.UpdateRecord()
	}
	// Provisioning runs in the background, so a rental that did not come up is
	// reported here rather than on the /provision call that started it.
	if le, ok := s.prov.(interface{ LastError() string }); ok {
		if msg := le.LastError(); msg != "" {
			body["error"] = msg
		}
	}
	writeJSON(w, body)
}

type updateReq struct {
	Version string `json:"version"`
}

// handleUpdate starts an update to the requested release and answers at once
// (202); the download, checks, swap and restart happen in the background, and
// /status shows them as "update". The URL is the agent's own: only the version
// comes from the request.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.host == nil {
		http.Error(w, "this agent cannot update itself", http.StatusNotImplemented)
		return
	}
	var req updateReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	from, to, err := s.host.Update(strings.TrimSpace(req.Version))
	if err != nil {
		http.Error(w, err.Error(), errorCode(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "updating", "from": from, "to": to})
}

// handleWithdrawn: the host removed this machine in the control panel.
func (s *Server) handleWithdrawn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.host == nil {
		http.Error(w, "this agent cannot stop hosting on request", http.StatusNotImplemented)
		return
	}
	if err := s.host.Withdraw(); err != nil {
		http.Error(w, err.Error(), errorCode(err))
		return
	}
	writeJSON(w, map[string]string{"status": "withdrawn"})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
