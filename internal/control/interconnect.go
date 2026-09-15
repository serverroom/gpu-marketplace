package control

// Linked pairs: two NVIDIA DGX Sparks cabled to each other over their
// ConnectX-7 ports and rented as one. These are the shapes the agent reports
// in its capability and answers on the control channel; the control plane
// stores only a bounded copy of them.

// Bounds on what one capability report carries.
const (
	MaxInterconnectPorts   = 8
	MaxInterconnectReasons = 10
	MaxInterconnectPeers   = 4
)

// Identity is what the machine says it is (DMI), and whether the agent
// confirmed it as an NVIDIA DGX Spark. The raw strings travel with the verdict
// because the exact values a Spark carries are not yet verified on hardware.
type Identity struct {
	SysVendor         string `json:"sys_vendor"`
	ProductName       string `json:"product_name"`
	BoardName         string `json:"board_name"`
	ProductFamily     string `json:"product_family"`
	ConfirmedDGXSpark bool   `json:"confirmed_dgx_spark"`
	// Reason is why the machine was not confirmed; "" when it was.
	Reason string `json:"reason"`
}

// NICPort is one ConnectX-7 network interface on the host.
type NICPort struct {
	BDF       string `json:"bdf"`
	Netdev    string `json:"netdev"`
	MAC       string `json:"mac"`
	Carrier   bool   `json:"carrier"`
	SpeedMbps int    `json:"speed_mbps"`
	MTU       int    `json:"mtu"`
	Driver    string `json:"driver"`
	Firmware  string `json:"firmware"`
	Serial    string `json:"serial"`
}

// Peer is another agent heard on a ConnectX-7 port. Advisory only: the
// authenticated link check is the proof that two machines are cabled.
type Peer struct {
	ListingID string `json:"listing_id"`
	LocalMAC  string `json:"local_mac"`
	PeerMAC   string `json:"peer_mac"`
	SeenAt    int64  `json:"seen_at"`
}

// PairTest is the latest pair test boot (`gpu-agent check --boot --pair`).
type PairTest struct {
	Passed       bool      `json:"passed"`
	AgentVersion string    `json:"agent_version"`
	LocalMACs    []string  `json:"local_macs"`
	PeerMACs     []string  `json:"peer_macs"`
	RDMAGbps     []float64 `json:"rdma_gbps"`
	At           int64     `json:"at"`
}

// Interconnect is whether this machine can be one half of a linked pair.
type Interconnect struct {
	// Supported: this binary has the pair runtime.
	Supported bool      `json:"supported"`
	Ready     bool      `json:"ready"`
	Reasons   []string  `json:"reasons"`
	Ports     []NICPort `json:"ports"`
	Peers     []Peer    `json:"peers"`
	PairTest  *PairTest `json:"pair_test"`
}

// Bound caps the lists at what a capability report may carry and turns nil
// lists into empty ones, so the report always has the same shape.
func (ic *Interconnect) Bound() {
	if ic == nil {
		return
	}
	if len(ic.Reasons) > MaxInterconnectReasons {
		ic.Reasons = ic.Reasons[:MaxInterconnectReasons]
	}
	if len(ic.Ports) > MaxInterconnectPorts {
		ic.Ports = ic.Ports[:MaxInterconnectPorts]
	}
	if len(ic.Peers) > MaxInterconnectPeers {
		ic.Peers = ic.Peers[:MaxInterconnectPeers]
	}
	if ic.Reasons == nil {
		ic.Reasons = []string{}
	}
	if ic.Ports == nil {
		ic.Ports = []NICPort{}
	}
	if ic.Peers == nil {
		ic.Peers = []Peer{}
	}
}
