package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// waitingProv is a provisioner (v0.2.3) whose machine its host is using.
type waitingProv struct {
	fakeProv
	startBy   int64
	pending   *PendingRental
	cancelled string
	pair      *PairProvisionRequest
}

func (w *waitingProv) ProvisionBy(rentalID, renterPubkey string, startBy int64) (*PendingRental, error) {
	w.provisioned, w.startBy = rentalID, startBy
	if startBy == 0 {
		return nil, Conflict("this machine is in use by its owner (llama-server); a rental can wait for it only with a start_by")
	}
	w.pending = &PendingRental{RentalID: rentalID, Since: 1789700000, StartBy: startBy,
		Holders: []string{"llama-server (pid 11435)"}, State: PendingWaiting}
	return w.pending, nil
}

func (w *waitingProv) PendingRental() *PendingRental { return w.pending }

func (w *waitingProv) CancelPending(rentalID string) (bool, error) {
	if w.pending == nil || w.pending.RentalID != rentalID {
		return false, nil
	}
	w.cancelled, w.pending = rentalID, nil
	return true, nil
}

func (w *waitingProv) Status() string {
	if w.pending != nil {
		return StatusWaitingForHost
	}
	return "free"
}

func (w *waitingProv) PairProvision(req PairProvisionRequest) error { return nil }

func (w *waitingProv) PairProvisionBy(req PairProvisionRequest) (*PendingRental, error) {
	w.pair = &req
	return w.ProvisionBy(req.RentalID, req.RenterPubkey, *req.StartBy)
}

func busyProv() *waitingProv {
	return &waitingProv{fakeProv: fakeProv{capability: Capability{Ready: true, Kind: "qemu-vfio",
		HostBusy: &HostBusy{Since: 1789690000, Holders: []string{"llama-server (pid 11435)"}}}}}
}

// A rental for a machine its host is using is answered 202 waiting_for_host,
// with the host's programs and the deadline; /status then carries the
// pending rental, and a teardown cancels it.
func TestAWaitingRentalIs202AndShowsOnStatus(t *testing.T) {
	wp := busyProv()
	s := New("127.0.0.1:0", "secret", wp)
	w := serve(s, "POST", "/provision", `{"rental_id":"R1","renter_pubkey":"k","start_by":1789819200}`, "secret")
	if w.Code != http.StatusAccepted || wp.startBy != 1789819200 {
		t.Fatalf("code %d body %s start_by %d", w.Code, w.Body, wp.startBy)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"holders":["llama-server (pid 11435)"],"start_by":1789819200,"status":"waiting_for_host"}` {
		t.Errorf("202 body = %s", got)
	}

	w = serve(s, "GET", "/status", "", "secret")
	var st map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if string(st["status"]) != `"waiting_for_host"` ||
		string(st["pending"]) != `{"rental_id":"R1","since":1789700000,"start_by":1789819200,"holders":["llama-server (pid 11435)"],"state":"waiting"}` ||
		string(st["host_busy"]) != `{"since":1789690000,"holders":["llama-server (pid 11435)"],"memory_short_gb":0}` {
		t.Errorf("status = %s", w.Body)
	}

	w = serve(s, "POST", "/teardown", `{"rental_id":"R1"}`, "secret")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"cancelled"`) || wp.cancelled != "R1" || wp.tornDown != "" {
		t.Errorf("teardown: %d %s (cancelled %q, torn down %q)", w.Code, w.Body, wp.cancelled, wp.tornDown)
	}
	w = serve(s, "POST", "/teardown", `{"rental_id":"R2"}`, "secret")
	if w.Code != http.StatusOK || wp.tornDown != "R2" {
		t.Errorf("a teardown of a rental that is not pending: %d, torn down %q", w.Code, wp.tornDown)
	}
}

// Without a start_by a rental cannot wait (409, with why); a start_by that is
// no unix time is a bad request. /status says host_busy null on a free machine.
func TestAWaitingRentalNeedsAStartBy(t *testing.T) {
	wp := busyProv()
	s := New("127.0.0.1:0", "secret", wp)
	if w := serve(s, "POST", "/provision", `{"rental_id":"R1","renter_pubkey":"k"}`, "secret"); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "llama-server") {
		t.Errorf("no start_by: %d %s", w.Code, w.Body)
	}
	for _, by := range []string{"0", "-5"} {
		if w := serve(s, "POST", "/provision", `{"rental_id":"R1","renter_pubkey":"k","start_by":`+by+`}`, "secret"); w.Code != http.StatusBadRequest {
			t.Errorf("start_by %s: %d", by, w.Code)
		}
	}
	wp.capability.HostBusy = nil
	if w := serve(s, "GET", "/status", "", "secret"); !strings.Contains(w.Body.String(), `"host_busy":null`) || strings.Contains(w.Body.String(), `"pending"`) {
		t.Errorf("free machine status = %s", w.Body)
	}
}

// /pair/provision takes start_by too (its strict decoding refuses fields it
// does not know, so an agent before v0.2.3 answers 400).
func TestAPairHalfCanWait(t *testing.T) {
	wp := busyProv()
	wp.capability.Interconnect = &Interconnect{Ready: true}
	s := New("127.0.0.1:0", "secret", wp)
	body := `{"rental_id":"R1","renter_pubkey":"k","node":"a","peer_hostname":"gpu-r1-b","links":[],"mtu":9000,"start_by":1789819200}`
	w := serve(s, "POST", "/pair/provision", body, "secret")
	if w.Code != http.StatusAccepted || wp.pair == nil || *wp.pair.StartBy != 1789819200 ||
		!strings.Contains(w.Body.String(), `"status":"waiting_for_host"`) {
		t.Errorf("pair half: %d %s", w.Code, w.Body)
	}
}
