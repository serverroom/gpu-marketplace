package heartbeat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"
)

// Heartbeat sends periodic alive signals to the hub.
type Heartbeat struct {
	hubEndpoint string
	peerID      string
	interval    time.Duration
	quit        chan struct{}
}

type heartbeatPayload struct {
	PeerID   string `json:"peer_id"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Time     string `json:"time"`
}

// New creates a new Heartbeat sender.
func New(hubHost string, hubPort int, peerID string, interval time.Duration) *Heartbeat {
	return &Heartbeat{
		hubEndpoint: fmt.Sprintf("http://%s:%d/heartbeat", hubHost, hubPort+1),
		peerID:      peerID,
		interval:    interval,
		quit:        make(chan struct{}),
	}
}

// Start begins sending heartbeats in the background.
func (h *Heartbeat) Start() {
	go h.run()
	log.Printf("Heartbeat started (every %s to %s)", h.interval, h.hubEndpoint)
}

// Stop stops the heartbeat sender.
func (h *Heartbeat) Stop() {
	close(h.quit)
}

func (h *Heartbeat) run() {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	// Send one immediately
	h.send()

	for {
		select {
		case <-ticker.C:
			h.send()
		case <-h.quit:
			return
		}
	}
}

func (h *Heartbeat) send() {
	hostname, _ := os.Hostname()
	payload := heartbeatPayload{
		PeerID:   h.peerID,
		Hostname: hostname,
		OS:       runtime.GOOS,
		Time:     time.Now().UTC().Format(time.RFC3339),
	}

	data, _ := json.Marshal(payload)
	resp, err := http.Post(h.hubEndpoint, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("Heartbeat returned %d", resp.StatusCode)
	}
}
