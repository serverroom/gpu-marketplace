package control

import (
	"net/http"
	"strings"
	"testing"
)

// pairProv is a provisioner with the pair runtime.
type pairProv struct {
	fakeProv
	verified   *LinkVerifyRequest
	verifyErr  error
	provision  *PairProvisionRequest
	provErr    error
	pairStatus *PairStatus
}

func (f *pairProv) LinkVerify(req LinkVerifyRequest) (*LinkVerifyResponse, error) {
	f.verified = &req
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	return &LinkVerifyResponse{Ports: []LinkPort{{Netdev: "enp1s0f0np0", BDF: "0000:01:00.0", MAC: "58:a2:e1:00:00:01",
		Carrier: true, SpeedMbps: 200000, PeerFrames: []PeerFrames{{Src: "58:a2:e1:00:01:01", Listing: "B", Count: 31}},
		LLDP: []LLDPEntry{}, ForeignSrc: []string{}}}}, nil
}

func (f *pairProv) PairProvision(req PairProvisionRequest) error {
	f.provision = &req
	return f.provErr
}

func (f *pairProv) PairStatus() *PairStatus { return f.pairStatus }

func readyPairProv() *pairProv {
	return &pairProv{fakeProv: fakeProv{capability: Capability{Ready: true, Kind: "qemu-vfio",
		Identity:     &Identity{ConfirmedDGXSpark: true},
		Interconnect: &Interconnect{Supported: true, Ready: true}}}}
}

const verifyBody = `{"challenge":"0123456789abcdef0123456789abcdef","self":"A","peer":"B","seconds":8}`

func TestLinkVerifyAnswersWhatThePortsHeard(t *testing.T) {
	fp := readyPairProv()
	w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/link/verify", verifyBody, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	if fp.verified == nil || fp.verified.Self != "A" || *fp.verified.Seconds != 8 {
		t.Fatalf("request = %+v", fp.verified)
	}
	for _, want := range []string{`"peer_frames":[{"src":"58:a2:e1:00:01:01","listing":"B","count":31}]`,
		`"lldp":[]`, `"stp":false`, `"foreign_src":[]`, `"bad_frames":0`, `"speed_mbps":200000`, `"carrier":true`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("answer missing %s: %s", want, w.Body.String())
		}
	}
}

func TestLinkVerifyRefusesBadBodies(t *testing.T) {
	for _, body := range []string{
		`{"challenge":"0123456789ABCDEF0123456789ABCDEF","self":"A","peer":"B"}`,
		`{"challenge":"0123","self":"A","peer":"B"}`,
		`{"challenge":"0123456789abcdef0123456789abcdef","self":"A","peer":"A"}`,
		`{"challenge":"0123456789abcdef0123456789abcdef","self":"../A","peer":"B"}`,
		`{"challenge":"0123456789abcdef0123456789abcdef","self":"A","peer":"B","seconds":0}`,
		`{"challenge":"0123456789abcdef0123456789abcdef","self":"A","peer":"B","seconds":16}`,
		`{"challenge":"0123456789abcdef0123456789abcdef","self":"A","peer":"B","ports":["eth0"]}`,
		`{"challenge":"0123456789abcdef0123456789abcdef","self":"A","peer":"B"} {}`,
		`not json`,
	} {
		fp := readyPairProv()
		w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/link/verify", body, "secret")
		if w.Code != http.StatusBadRequest || fp.verified != nil {
			t.Errorf("%s: code = %d, called = %v", body, w.Code, fp.verified != nil)
		}
	}
	fp := readyPairProv()
	w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/link/verify",
		`{"challenge":"0123456789abcdef0123456789abcdef","self":"A","peer":"B"}`, "secret")
	if w.Code != http.StatusOK || fp.verified.Seconds != nil {
		t.Errorf("seconds is optional: code = %d", w.Code)
	}
}

func TestLinkVerifyConflictsAndAuth(t *testing.T) {
	fp := readyPairProv()
	fp.verifyErr = Conflict("this machine is rented; the cable is checked only while it is free")
	w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/link/verify", verifyBody, "secret")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "rented") {
		t.Errorf("code = %d body = %s", w.Code, w.Body.String())
	}
	if w := serve(New("127.0.0.1:0", "secret", readyPairProv()), "POST", "/link/verify", verifyBody, "wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("without the token: %d", w.Code)
	}
	if w := serve(New("127.0.0.1:0", "secret", readyPairProv()), "GET", "/link/verify", "", "secret"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", w.Code)
	}
}

func TestLinkVerifyErrorsAreStatusCodes(t *testing.T) {
	for err, code := range map[error]int{
		Invalid("x"):     http.StatusBadRequest,
		Conflict("x"):    http.StatusConflict,
		Unavailable("x"): http.StatusServiceUnavailable,
	} {
		fp := readyPairProv()
		fp.verifyErr = err
		w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/link/verify", verifyBody, "secret")
		if w.Code != code {
			t.Errorf("%T %v -> %d, want %d", err, err, w.Code, code)
		}
	}
}

const pairBody = `{"rental_id":"1a2b3c4d-0000-4000-8000-000000000000","renter_pubkey":"ssh-ed25519 AAAA","node":"a",` +
	`"peer_hostname":"gpu-1a2b3c4d-b","links":[{"local_mac":"58:a2:e1:00:00:01","cidr":"10.200.0.1/30","peer_ip":"10.200.0.2"}],` +
	`"mtu":9000,"intra_key":{"private_openssh":"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n","public":"ssh-ed25519 AAAA"}}`

func TestPairProvisionStartsTheRental(t *testing.T) {
	fp := readyPairProv()
	w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/pair/provision", pairBody, "secret")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"provisioning"`) {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	r := fp.provision
	if r == nil || r.Node != "a" || r.PeerHostname != "gpu-1a2b3c4d-b" || r.MTU != 9000 || len(r.Links) != 1 ||
		r.Links[0].CIDR != "10.200.0.1/30" || r.IntraKey == nil || r.IntraKey.Public != "ssh-ed25519 AAAA" {
		t.Errorf("request = %+v", r)
	}
}

// A machine that cannot be half of a pair refuses before reading the request.
func TestPairProvisionRefusesAMachineThatIsNotReady(t *testing.T) {
	for name, spoil := range map[string]func(c *Capability){
		"single not ready":       func(c *Capability) { c.Ready, c.Reasons = false, []string{"no test boot"} },
		"interconnect not ready": func(c *Capability) { c.Interconnect.Ready, c.Interconnect.Reasons = false, []string{"no pair test"} },
		"no pair half":           func(c *Capability) { c.Interconnect = nil },
	} {
		fp := readyPairProv()
		spoil(&fp.capability)
		w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/pair/provision", pairBody, "secret")
		if w.Code != http.StatusServiceUnavailable || fp.provision != nil {
			t.Errorf("%s: code = %d called = %v", name, w.Code, fp.provision != nil)
		}
	}
}

func TestPairProvisionRefusals(t *testing.T) {
	for body, code := range map[string]int{
		strings.Replace(pairBody, `"mtu":9000`, `"mtu":9000,"gateway":"10.200.0.254"`, 1): http.StatusBadRequest,
		`{"rental_id":`: http.StatusBadRequest,
	} {
		fp := readyPairProv()
		if w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/pair/provision", body, "secret"); w.Code != code || fp.provision != nil {
			t.Errorf("%s: code = %d, want %d", body, w.Code, code)
		}
	}
	for err, code := range map[error]int{
		Conflict("link 0: 58:a2:e1:00:00:09 is not a ConnectX-7 port of this machine"): http.StatusConflict,
		Invalid("link 0: cidr 10.200.0.1/24 must be a /30"):                            http.StatusBadRequest,
		Unavailable("this machine cannot be half of a linked pair: x"):                 http.StatusServiceUnavailable,
	} {
		fp := readyPairProv()
		fp.provErr = err
		if w := serve(New("127.0.0.1:0", "secret", fp), "POST", "/pair/provision", pairBody, "secret"); w.Code != code {
			t.Errorf("%v: code = %d, want %d", err, w.Code, code)
		}
	}
	if w := serve(New("127.0.0.1:0", "secret", readyPairProv()), "POST", "/pair/provision", pairBody, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("without the token: %d", w.Code)
	}
}

func TestStatusCarriesThePair(t *testing.T) {
	fp := readyPairProv()
	fp.pairStatus = &PairStatus{Node: "b", GuestLink: GuestLinkStatus{OK: 1, Fail: 1, Links: 2}}
	w := serve(New("127.0.0.1:0", "secret", fp), "GET", "/status", "", "secret")
	for _, want := range []string{`"interconnect_ready":true`, `"identity_confirmed":true`,
		`"pair":{"node":"b","guest_link":{"ok":1,"fail":1,"links":2}}`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("status missing %s: %s", want, w.Body.String())
		}
	}
	// A single machine (or an agent without the pair half) says false and no pair.
	w = serve(New("127.0.0.1:0", "secret", readyProv()), "GET", "/status", "", "secret")
	if !strings.Contains(w.Body.String(), `"interconnect_ready":false`) || !strings.Contains(w.Body.String(), `"identity_confirmed":false`) ||
		strings.Contains(w.Body.String(), `"pair"`) {
		t.Errorf("status = %s", w.Body.String())
	}
}
