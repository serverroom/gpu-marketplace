package control

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// Provisioner is the agent action layer the control channel drives. The real
// Kata/VFIO implementation lives in the provisioner package; tests use a fake.
type Provisioner interface {
	Provision(rentalID, renterPubkey string) error
	Teardown(rentalID string) error
	Status() string
}

// Server is the agent control-channel HTTP server. It binds 127.0.0.1 only; the
// control plane reaches it through the relay's reverse tunnel. Every command is
// bearer-authenticated with the token shared at register time (end-to-end — the
// relay carries it as opaque bytes).
type Server struct {
	token      string
	prov       Provisioner
	httpServer *http.Server
	listenAddr string
}

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
			WriteTimeout: 30 * time.Second,
		},
	}
	mux.HandleFunc("/provision", s.auth(s.handleProvision))
	mux.HandleFunc("/teardown", s.auth(s.handleTeardown))
	mux.HandleFunc("/status", s.auth(s.handleStatus))
	mux.HandleFunc("/health", s.handleHealth)
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
	var req provisionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
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
	var req teardownReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.prov.Teardown(req.RentalID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "wiping"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": s.prov.Status()})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
