package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// The linked-pair half of the control API (CONTRACT.md s3.2): the raw-frame
// cable check and a pair rental's start. Both exist only on an agent whose
// provisioner implements them, so an agent without the pair runtime answers
// 404 -- the control plane's fail-closed signal -- rather than booting a lone
// VM from a request it half understood.

// LinkVerifyRequest is POST /link/verify.
type LinkVerifyRequest struct {
	Challenge string `json:"challenge"`
	Self      string `json:"self"`
	Peer      string `json:"peer"`
	// Seconds to listen: 1..15, 8 when absent.
	Seconds *int `json:"seconds,omitempty"`
}

// Link verify bounds.
const (
	LinkVerifyDefaultSeconds = 8
	LinkVerifyMinSeconds     = 1
	LinkVerifyMaxSeconds     = 15
)

// PeerFrames counts the valid authenticated frames one source sent.
type PeerFrames struct {
	Src     string `json:"src"`
	Listing string `json:"listing"`
	Count   int    `json:"count"`
}

// LLDPEntry is one LLDP sender heard on a port.
type LLDPEntry struct {
	Src     string `json:"src"`
	Chassis string `json:"chassis"`
}

// LinkPort is what one ConnectX-7 port heard during a cable check.
type LinkPort struct {
	Netdev     string       `json:"netdev"`
	BDF        string       `json:"bdf"`
	MAC        string       `json:"mac"`
	Carrier    bool         `json:"carrier"`
	SpeedMbps  int          `json:"speed_mbps"`
	PeerFrames []PeerFrames `json:"peer_frames"`
	LLDP       []LLDPEntry  `json:"lldp"`
	STP        bool         `json:"stp"`
	ForeignSrc []string     `json:"foreign_src"`
	BadFrames  int          `json:"bad_frames"`
}

// LinkVerifyResponse is the 200 answer to /link/verify.
type LinkVerifyResponse struct {
	Ports []LinkPort `json:"ports"`
}

// PairLink is one cable a pair rental's VM configures.
type PairLink struct {
	LocalMAC string `json:"local_mac"`
	CIDR     string `json:"cidr"`
	PeerIP   string `json:"peer_ip"`
}

// IntraKey is the per-rental key the two VMs of a pair log in to each other
// with. It is never stored by the control plane.
type IntraKey struct {
	PrivateOpenSSH string `json:"private_openssh"`
	Public         string `json:"public"`
}

// PairProvisionRequest is POST /pair/provision.
type PairProvisionRequest struct {
	RentalID     string     `json:"rental_id"`
	RenterPubkey string     `json:"renter_pubkey"`
	Node         string     `json:"node"`
	PeerHostname string     `json:"peer_hostname"`
	Links        []PairLink `json:"links"`
	MTU          int        `json:"mtu"`
	IntraKey     *IntraKey  `json:"intra_key,omitempty"`
}

// GuestLinkStatus is what a pair rental's VM reported about its cables.
type GuestLinkStatus struct {
	OK    int `json:"ok"`
	Fail  int `json:"fail"`
	Links int `json:"links"`
}

// PairStatus is /status's "pair" object during a pair rental.
type PairStatus struct {
	Node      string          `json:"node"`
	GuestLink GuestLinkStatus `json:"guest_link"`
}

// LinkVerifier runs the raw-frame cable check.
type LinkVerifier interface {
	LinkVerify(req LinkVerifyRequest) (*LinkVerifyResponse, error)
}

// PairProvisioner starts one machine's half of a pair rental.
type PairProvisioner interface {
	PairProvision(req PairProvisionRequest) error
}

// PairStatuser reports the pair rental on the machine, or nil when none is.
type PairStatuser interface {
	PairStatus() *PairStatus
}

// RequestError is a refusal with the HTTP status it is answered with.
type RequestError struct {
	Code int
	Msg  string
}

func (e *RequestError) Error() string { return e.Msg }

// Invalid is a 400 refusal: the request itself is wrong.
func Invalid(format string, a ...interface{}) error {
	return &RequestError{Code: http.StatusBadRequest, Msg: fmt.Sprintf(format, a...)}
}

// Conflict is a 409 refusal: the request is fine, the machine's state is not.
func Conflict(format string, a ...interface{}) error {
	return &RequestError{Code: http.StatusConflict, Msg: fmt.Sprintf(format, a...)}
}

// Unavailable is a 503 refusal: this machine cannot do it at all right now.
func Unavailable(format string, a ...interface{}) error {
	return &RequestError{Code: http.StatusServiceUnavailable, Msg: fmt.Sprintf(format, a...)}
}

func writeError(w http.ResponseWriter, err error) {
	var re *RequestError
	if errors.As(err, &re) {
		http.Error(w, re.Msg, re.Code)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// maxPairBody bounds a pair request: an intra key and two links fit many
// times over.
const maxPairBody = 64 << 10

// decodeStrict reads exactly one JSON object with no field this agent does not
// know. A pair request carrying a field the agent would ignore is refused, not
// half-honoured: that silent ignoring is what made /provision unfit for pairs.
func decodeStrict(r *http.Request, v interface{}) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxPairBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Invalid("bad request: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if dec.More() {
		return Invalid("bad request: more than one JSON value")
	}
	return nil
}

var (
	challengePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	listingIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// ValidListingID reports whether id looks like a marketplace listing id.
func ValidListingID(id string) bool { return listingIDPattern.MatchString(id) }

// ValidateLinkVerify checks a /link/verify body and returns the seconds to
// listen for.
func ValidateLinkVerify(req LinkVerifyRequest) (int, error) {
	if !challengePattern.MatchString(req.Challenge) {
		return 0, Invalid("challenge must be 32 lowercase hex characters")
	}
	if !ValidListingID(req.Self) || !ValidListingID(req.Peer) {
		return 0, Invalid("self and peer must be listing ids")
	}
	if req.Self == req.Peer {
		return 0, Invalid("self and peer are the same listing")
	}
	seconds := LinkVerifyDefaultSeconds
	if req.Seconds != nil {
		seconds = *req.Seconds
	}
	if seconds < LinkVerifyMinSeconds || seconds > LinkVerifyMaxSeconds {
		return 0, Invalid("seconds must be %d..%d", LinkVerifyMinSeconds, LinkVerifyMaxSeconds)
	}
	return seconds, nil
}

func (s *Server) handleLinkVerify(v LinkVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req LinkVerifyRequest
		if err := decodeStrict(r, &req); err != nil {
			writeError(w, err)
			return
		}
		if _, err := ValidateLinkVerify(req); err != nil {
			writeError(w, err)
			return
		}
		resp, err := v.LinkVerify(req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, resp)
	}
}

func (s *Server) handlePairProvision(pp PairProvisioner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Like /provision: a machine that cannot host a pair refuses before it
		// reads anything.
		c := s.prov.Capability()
		if !c.Ready {
			http.Error(w, "this machine cannot host a rental: "+strings.Join(c.Reasons, "; "), http.StatusServiceUnavailable)
			return
		}
		if c.Interconnect == nil || !c.Interconnect.Ready {
			why := "this machine cannot be half of a linked pair"
			if c.Interconnect != nil && len(c.Interconnect.Reasons) > 0 {
				why += ": " + strings.Join(c.Interconnect.Reasons, "; ")
			}
			http.Error(w, why, http.StatusServiceUnavailable)
			return
		}
		var req PairProvisionRequest
		if err := decodeStrict(r, &req); err != nil {
			writeError(w, err)
			return
		}
		if err := pp.PairProvision(req); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "provisioning"})
	}
}
