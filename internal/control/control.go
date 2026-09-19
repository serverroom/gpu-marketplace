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
	// GPUCount is how many GPUs a rental on this machine gets: 0 on a Linux
	// machine without a GPU, which hosts CPU-only rentals. Absent where the
	// agent cannot host at all (macOS, Windows) and from agents up to v0.1.9;
	// the control plane treats only an explicit 0 as a machine without a GPU.
	GPUCount *int `json:"gpu_count,omitempty"`
	// Guest is what a renter's VM gets on this machine (Linux only): the
	// same figures the VM is sized with.
	Guest *Guest `json:"guest,omitempty"`
	// Identity is what the machine says it is (DMI) and whether the agent
	// confirmed it as an NVIDIA DGX Spark; Interconnect is whether it can be
	// half of a linked pair. Absent from agents up to v0.1.9, and Identity
	// from hosts that are not Linux.
	Identity     *Identity     `json:"identity,omitempty"`
	Interconnect *Interconnect `json:"interconnect,omitempty"`
	// Errors are the agent's recent problems, newest first (at most
	// MaxAgentErrors): whatever stopped, or stops, this machine being
	// listed. Set when the capability is reported; absent before v0.2.0.
	Errors []AgentError `json:"errors,omitempty"`
	// AutoUpdate is the agent's automatic updating; absent before v0.2.0.
	AutoUpdate *AutoUpdate `json:"auto_update,omitempty"`
	// Setup is where the automatic setup stands; absent before v0.2.0 and on
	// a machine it never ran on.
	Setup *SetupProgress `json:"setup,omitempty"`
	// GPUs are what the last passing test boot saw inside the VM -- what a
	// renter gets, of any make. Absent until the machine has passed one, and
	// before v0.2.2.
	GPUs []GPU `json:"gpus,omitempty"`
	// Excluded names the GPUs on this machine that rentals leave out, and why:
	// the host is using one, it is already given to a VM of the host's own, it
	// is the processor's integrated GPU or the host's console, or it cannot be
	// passed through on its own. Absent before v0.2.2.
	Excluded []string `json:"excluded,omitempty"`
	// HostBusy: the host itself is using what a rental would take -- its own
	// programs hold the GPU, or the memory the rental's VM needs is in use.
	// The machine stays ready and offered: a rental that arrives waits for
	// the host to free it (up to its start_by). Absent while the host uses
	// nothing a rental needs, and before v0.2.3.
	HostBusy *HostBusy `json:"host_busy,omitempty"`
	// RetestPending: the machine sells on a test boot from before -- another
	// agent version's, or one run without the GPU while the host was using it
	// -- and runs the full test when the GPU is free, and always before a
	// rental starts. Absent (false) otherwise, and before v0.2.3.
	RetestPending bool `json:"retest_pending,omitempty"`
	// SelfTest is the last test boot in brief; absent before one ran, and
	// before v0.2.3.
	SelfTest *SelfTestSummary `json:"selftest,omitempty"`
}

// HostBusy is what the host is using of what a rental would take, and since
// when (the agent's own clock; it starts again when the agent restarts).
type HostBusy struct {
	Since int64 `json:"since"`
	// Holders are the host's programs holding the GPU, as
	// "llama-server (pid 11435)"; [] when only memory is short.
	Holders []string `json:"holders"`
	// MemoryShortGB is how much more memory must be free for the rental's VM
	// (0: none).
	MemoryShortGB int `json:"memory_short_gb"`
}

// SelfTestSummary is the last test boot: whether it passed, whether it
// took the GPU (a test run while the host was using the GPU passes without
// it; on a machine without a GPU every passing test counts), when, and with
// which agent version.
type SelfTestSummary struct {
	Passed       bool   `json:"passed"`
	GPUVerified  bool   `json:"gpu_verified"`
	At           int64  `json:"at"`
	AgentVersion string `json:"agent_version"`
}

// Pending rental states, in PendingRental.State.
const (
	// PendingWaiting: the host's own use holds the machine; the rental starts
	// as soon as it is free, or is given up at start_by.
	PendingWaiting = "waiting"
	// PendingStarting: the machine is free and the rental is starting (its
	// full test boot first, when the running agent has not passed one).
	PendingStarting = "starting"
	// PendingFailed: the rental did not start; Reason says why. The record
	// stays until the rental is torn down (POST /teardown) or replaced.
	PendingFailed = "failed"
)

// Pending rental failure reasons, in PendingRental.Reason.
const (
	// PendingReasonGPU: the full test boot right before the rental failed.
	PendingReasonGPU = "its GPU could not be handed to the rental"
	// PendingReasonStart: the rental itself did not start.
	PendingReasonStart = "the rental could not start on this machine"
)

// PendingRental is a rental accepted while the host was using the machine
// (POST /provision answered 202), for /status's "pending".
type PendingRental struct {
	RentalID string `json:"rental_id"`
	Since    int64  `json:"since"`
	StartBy  int64  `json:"start_by"`
	// Holders are what the host was using when last looked at, as in
	// HostBusy.
	Holders       []string `json:"holders"`
	MemoryShortGB int      `json:"memory_short_gb,omitempty"`
	State         string   `json:"state"`
	Reason        string   `json:"reason,omitempty"`
	Detail        string   `json:"detail,omitempty"`
	// WaitingForPeer: a pair half whose machine is free, waiting for the
	// other half's machine (Holders is then empty).
	WaitingForPeer bool `json:"waiting_for_peer,omitempty"`
}

// SetupProgress is the automatic setup's last attempt in brief: the step
// ("deps", "image", "test-boot"), its state ("running", "passed", "failed",
// "interrupted"), the line a host sees, and when that step started or the
// attempt ended.
type SetupProgress struct {
	Step    string `json:"step"`
	State   string `json:"state"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

// MaxAgentErrors is how many problems the agent keeps and reports.
const MaxAgentErrors = 20

// Agent error areas.
const (
	AreaSetup     = "setup"
	AreaPreflight = "preflight"
	AreaTestBoot  = "testboot"
	AreaUpdate    = "update"
	AreaTunnel    = "tunnel"
	AreaReport    = "report"
	AreaRental    = "rental"
	AreaRegister  = "register"
	AreaAgent     = "agent"
)

// AgentError is one problem the agent had: when, in which part of it, what
// in one plain line (at most 300 characters), optionally more detail (at most
// 2000: the last lines of a failing command, say), and whether it still
// stops the machine now.
type AgentError struct {
	At      int64  `json:"at"`
	Area    string `json:"area"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Active  bool   `json:"active"`
}

// AutoUpdate is the agent's automatic updating: whether the host left it on,
// when the agent last heard from the marketplace which release is current,
// and the last update (update.json), if any.
type AutoUpdate struct {
	Enabled   bool        `json:"enabled"`
	LastCheck int64       `json:"last_check"`
	Last      interface{} `json:"last"`
}

// AgentUpdate is the marketplace telling the agent which release to run
// (the answer to a capability report, or a status request): push is a staff
// or host request, applied even when automatic updates are off.
type AgentUpdate struct {
	Version string `json:"version"`
	Push    bool   `json:"push"`
}

// Guest is a rental VM's size on this machine.
type Guest struct {
	VCPUs    int    `json:"vcpus"`
	MemoryGB int    `json:"memory_gb"`
	DiskGB   int    `json:"disk_gb"`
	CPU      string `json:"cpu"`
}

// GPU is one GPU a rental gets, as the rental's own VM saw it.
type GPU struct {
	Model string `json:"model"`
	PCIID string `json:"pci_id"`
	// MemoryMB is absent when nothing in the VM could say (a driver without a
	// memory figure, or no driver at all).
	MemoryMB int  `json:"memory_mb,omitempty"`
	Unified  bool `json:"unified_memory,omitempty"`
	// Driver is absent when no driver took the GPU inside the VM.
	Driver string `json:"driver,omitempty"`
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
	if pp, ok := prov.(PairProvisioner); ok {
		mux.HandleFunc("/pair/provision", s.auth(s.handlePairProvision(pp)))
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
	// StartBy (unix seconds) is how long the rental may wait for the host to
	// free the machine; absent: it may not wait.
	StartBy *int64 `json:"start_by,omitempty"`
}

// WaitingProvisioner is a Provisioner that lets the host keep using the
// machine until it is rented (v0.2.3): a rental that arrives while the host
// uses it waits, up to startBy, and the answer is the pending rental (nil
// when it is starting at once).
type WaitingProvisioner interface {
	ProvisionBy(rentalID, renterPubkey string, startBy int64) (*PendingRental, error)
}

// PendingStatuser reports the pending rental for /status, or nil.
type PendingStatuser interface {
	PendingRental() *PendingRental
}

// PendingCanceller cancels a pending rental (POST /teardown for it): there
// is nothing to wipe. cancelled is false when rentalID is not pending here.
type PendingCanceller interface {
	CancelPending(rentalID string) (cancelled bool, err error)
}

// writeWaiting answers 202 for a rental that waits for the host.
func writeWaiting(w http.ResponseWriter, p *PendingRental) {
	body := map[string]interface{}{"status": StatusWaitingForHost, "holders": p.Holders, "start_by": p.StartBy}
	if p.MemoryShortGB > 0 {
		body["memory_short_gb"] = p.MemoryShortGB
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(body)
}

// StatusWaitingForHost is /status's status (and the 202 answer's) while a
// rental waits for the host to free the machine.
const StatusWaitingForHost = "waiting_for_host"

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
	if req.StartBy != nil && *req.StartBy <= 0 {
		http.Error(w, "start_by must be a unix time", http.StatusBadRequest)
		return
	}
	if wp, ok := s.prov.(WaitingProvisioner); ok {
		var startBy int64
		if req.StartBy != nil {
			startBy = *req.StartBy
		}
		pending, err := wp.ProvisionBy(req.RentalID, req.RenterPubkey, startBy)
		switch {
		case err != nil:
			writeError(w, err)
		case pending != nil:
			writeWaiting(w, pending)
		default:
			writeJSON(w, map[string]string{"status": "provisioning"})
		}
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
	// A rental still waiting for the host has nothing to wipe: it is
	// cancelled, and the host keeps the machine.
	if pc, ok := s.prov.(PendingCanceller); ok {
		cancelled, err := pc.CancelPending(req.RentalID)
		if err != nil {
			writeError(w, err)
			return
		}
		if cancelled {
			writeJSON(w, map[string]string{"status": "cancelled"})
			return
		}
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
		// Linked pairs (CONTRACT.md s3.2): false from an agent that cannot say.
		"interconnect_ready": c.Interconnect != nil && c.Interconnect.Ready,
		"identity_confirmed": c.Identity != nil && c.Identity.ConfirmedDGXSpark,
	}
	// The host's use, as the capability says it (null while it uses nothing
	// a rental needs), and whether the full test boot is still owed.
	body["host_busy"] = c.HostBusy
	body["retest_pending"] = c.RetestPending
	if ps, ok := s.prov.(PendingStatuser); ok {
		if pending := ps.PendingRental(); pending != nil {
			body["pending"] = pending
		}
	}
	if s.host != nil {
		// The marketplace may say which release this machine should run on
		// the status request itself: acted on first, so "update" below shows
		// an update it started.
		au, auto := s.host.(AutoUpdater)
		if auto {
			if u := requestedUpdate(r); u != nil {
				au.Offer(u)
			}
		}
		body["agent_version"] = s.host.AgentVersion()
		body["update"] = s.host.UpdateRecord()
		if auto {
			body["auto_update"] = au.AutoUpdateStatus()
		}
	}
	if ps, ok := s.prov.(PairStatuser); ok {
		if pair := ps.PairStatus(); pair != nil {
			body["pair"] = pair
		}
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

// AutoUpdater is a Host that updates itself when the marketplace names a
// newer release (CONTRACT-ops A2).
type AutoUpdater interface {
	// Offer acts on the marketplace's agent_update: started says an update
	// began, else why says what it waits for.
	Offer(u *AgentUpdate) (started bool, why string)
	AutoUpdateStatus() *AutoUpdate
}

// UpdateOfferHeader carries the marketplace's update offer on its heartbeat
// (its own GET /status, every 30 seconds): {"version":"vX.Y.Z","push":bool}.
const UpdateOfferHeader = "X-Marketplace-Agent-Update"

var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// ParseUpdateOffer reads an update offer strictly: a JSON object with a
// release version (vX.Y.Z) and a boolean push, nothing else; anything else is
// no offer.
func ParseUpdateOffer(raw string) *AgentUpdate {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return nil
	}
	var u AgentUpdate
	for k, v := range fields {
		switch k {
		case "version":
			if json.Unmarshal(v, &u.Version) != nil {
				return nil
			}
		case "push":
			if json.Unmarshal(v, &u.Push) != nil {
				return nil
			}
		default:
			return nil
		}
	}
	if !releaseVersion.MatchString(u.Version) {
		return nil
	}
	return &u
}

// requestedUpdate is the update offer a status request carries, if any.
func requestedUpdate(r *http.Request) *AgentUpdate {
	return ParseUpdateOffer(r.Header.Get(UpdateOfferHeader))
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
