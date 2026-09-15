package interconnect

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	dataDir = "/var/lib/gpu-agent"
	version = "v0.2.0-dev"
	gpuBDF  = "000f:01:00.0"
)

// The DGX Spark layout DESIGN.md assumes (A1): one ConnectX-7, two ports, each
// reachable over two PCIe x4 links in two PCI domains -- four functions, four
// netdevs, one serial.
var sparkPorts = []struct{ bdf, netdev, mac string }{
	{"0000:01:00.0", "enp1s0f0np0", "58:a2:e1:00:00:01"},
	{"0000:01:00.1", "enp1s0f1np1", "58:a2:e1:00:00:02"},
	{"0002:01:00.0", "enP2p1s0f0np0", "58:a2:e1:00:00:03"},
	{"0002:01:00.1", "enP2p1s0f1np1", "58:a2:e1:00:00:04"},
}

func spec() vmrt.Spec {
	return vmrt.Spec{Arch: "arm64", DataDir: dataDir, GoldenImage: dataDir + "/golden.img", GPUs: []string{gpuBDF}}
}

func opts() Options {
	return Options{GOOS: "linux", Arch: "arm64", Spec: spec(), Version: version, GPUModels: []string{"NVIDIA GB10"}}
}

// sparkHost is a DGX Spark that passes every pair check: identified, its
// ConnectX-7 untouched by the host, the tools and an RDMA base image installed,
// and a passing pair test boot for this version over its ports.
func sparkHost(t *testing.T) *fakehost.Host {
	t.Helper()
	h := fakehost.New()
	for name, v := range map[string]string{"sys_vendor": "NVIDIA", "product_name": "DGX Spark", "board_name": "P4242", "product_family": "DGX Spark"} {
		h.Files["/sys/class/dmi/id/"+name] = []byte(v + "\n")
	}
	for _, p := range sparkPorts {
		h.NIC(p.bdf, p.netdev, p.mac, "MT2412X00001", p.bdf)
	}
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 192.168.1.1 dev enP7s7 src 192.168.1.50 uid 0\n    cache\n"
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-arm64.img", Extras: []string{"rdma"}})
	h.Files[dataDir+"/golden.img.json"] = golden
	pass, _ := json.Marshal(vmrt.PairTestResult{Passed: true, AgentVersion: version,
		LocalMACs: []string{"58:a2:e1:00:00:01", "58:a2:e1:00:00:02"}, PeerMACs: []string{"58:a2:e1:00:01:01", "58:a2:e1:00:01:02"},
		RDMAGbps: []float64{112.4, 110.9}, At: 1789000000})
	h.Files[vmrt.PairTestPath(dataDir)] = pass
	return h
}

func texts(rep Report) string { return strings.Join(rep.Reasons(), " | ") }

func wantProblem(t *testing.T, rep Report, check, fragment string) {
	t.Helper()
	for _, p := range rep.Problems {
		if p.Check == check && strings.Contains(p.Text, fragment) {
			return
		}
	}
	t.Errorf("no %s problem containing %q; problems: %s", check, fragment, texts(rep))
}

func TestReadySparkPassesEveryCheck(t *testing.T) {
	rep := Preflight(sparkHost(t), opts())
	if !rep.Ready() {
		t.Fatalf("a clean Spark is not ready: %s", texts(rep))
	}
	if len(rep.NICs) != 1 || len(rep.NICs[0].Functions) != 4 || len(rep.Ports()) != 4 {
		t.Fatalf("discovery = %+v", rep.NICs)
	}
	if len(rep.EligiblePorts()) != 4 {
		t.Errorf("eligible ports = %v", rep.Eligible)
	}
	if got := strings.Join(rep.Functions(), " "); got != "0000:01:00.0 0000:01:00.1 0002:01:00.0 0002:01:00.1" {
		t.Errorf("functions = %s", got)
	}
	p := rep.Ports()[0]
	if p.MAC != "58:a2:e1:00:00:01" || !p.Carrier || p.SpeedMbps != 200000 || p.MTU != 1500 ||
		p.Driver != "mlx5_core" || p.Firmware != "28.40.1000 (NVD0000000033)" || p.Serial != "MT2412X00001" {
		t.Errorf("port = %+v", p)
	}
}

func TestDiscoverGroupsFunctionsBySerial(t *testing.T) {
	h := sparkHost(t)
	h.NIC("0003:01:00.0", "enP3p1s0f0np0", "58:a2:e1:00:02:01", "MT9999X00002", "0003:01:00.0")
	h.PCI("0004:01:00.0", "igb", "0x020000", "0004:01:00.0") // not a ConnectX
	h.Files["/sys/bus/pci/devices/0004:01:00.0/vendor"] = []byte("0x8086\n")
	nics := Discover(h)
	if len(nics) != 2 || nics[0].Serial != "MT2412X00001" || len(nics[0].Functions) != 4 ||
		nics[1].Serial != "MT9999X00002" || len(nics[1].Functions) != 1 {
		t.Fatalf("nics = %+v", nics)
	}
}

// devlink answers only for a function on mlx5_core; the PCI VPD reads the same
// whatever holds the function.
func TestSerialFallsBackToVPDAndGroupsBySlot(t *testing.T) {
	h := sparkHost(t)
	h.Fail["devlink -j dev info pci/0002:01:00.1"] = errors.New("devlink: no such device")
	h.Fail["devlink -j dev info pci/0002:01:00.0"] = errors.New("devlink: no such device")
	h.Files["/sys/bus/pci/devices/0002:01:00.0/vpd"] = vpd("ConnectX-7", map[string]string{"PN": "MCX75310", "SN": "MT2412X00001"})
	nics := Discover(h)
	if len(nics) != 1 || len(nics[0].Functions) != 4 || len(nics[0].Problems) != 0 {
		t.Fatalf("VPD serial or slot grouping failed: %+v", nics)
	}

	delete(h.Files, "/sys/bus/pci/devices/0002:01:00.0/vpd")
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP1, "serial number of the ConnectX card at 0002:01:00.0 cannot be read")
}

// vpd builds PCI VPD: an identifier string resource, a read-only resource of
// keywords, the end tag.
func vpd(id string, kw map[string]string) []byte {
	out := []byte{0x82, byte(len(id)), 0}
	out = append(out, id...)
	var ro []byte
	for _, k := range []string{"PN", "SN"} {
		if v, ok := kw[k]; ok {
			ro = append(ro, k[0], k[1], byte(len(v)))
			ro = append(ro, v...)
		}
	}
	out = append(out, 0x90, byte(len(ro)), 0)
	out = append(out, ro...)
	return append(out, 0x78)
}

func TestP1NoCardVFsAndVFIO(t *testing.T) {
	bare := fakehost.New()
	rep := Preflight(bare, opts())
	wantProblem(t, rep, CheckP1, "no NVIDIA ConnectX network card was found")

	h := sparkHost(t)
	h.PCI("0000:01:00.2", "mlx5_core", "0x020000", "0000:01:00.2")
	h.Files["/sys/bus/pci/devices/0000:01:00.2/vendor"] = []byte("0x15b3\n")
	h.Files["/sys/bus/pci/devices/0000:01:00.2/physfn"] = nil
	h.Outputs["devlink -j dev info pci/0000:01:00.2"] = `{"info":{"pci/0000:01:00.2":{"serial_number":"MT2412X00001"}}}`
	wantProblem(t, Preflight(h, opts()), CheckP1, "SR-IOV virtual function 0000:01:00.2")

	h = sparkHost(t)
	h.Links["/sys/bus/pci/devices/0002:01:00.1/driver"] = "../../../bus/pci/drivers/vfio-pci"
	delete(h.Files, "/sys/bus/pci/devices/0002:01:00.1/net/enP2p1s0f1np1")
	rep = Preflight(h, opts())
	wantProblem(t, rep, CheckP1, "0002:01:00.1 is on vfio-pci")
}

func TestP2AddressesAndRoutes(t *testing.T) {
	h := sparkHost(t)
	h.Outputs["ip -j addr show dev enp1s0f0np0"] = `[{"ifname":"enp1s0f0np0","addr_info":[` +
		`{"family":"inet6","local":"fe80::5aa2:e1ff:fe00:1","prefixlen":64,"scope":"link"},` +
		`{"family":"inet","local":"192.168.100.10","prefixlen":24,"scope":"global"}]}]`
	h.Outputs["ip -j addr show dev enp1s0f1np1"] = `[{"ifname":"enp1s0f1np1","addr_info":[{"family":"inet6","local":"fd00::1","prefixlen":64,"scope":"global"}]}]`
	h.Outputs["ip -6 -j route show dev enP2p1s0f0np0"] = `[{"dst":"fe80::/64"},{"dst":"fd00:1::/64"}]`
	h.Outputs["ip -j route show dev enP2p1s0f1np1"] = `[{"dst":"10.9.0.0/16"}]`
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP2, "enp1s0f0np0 has the address 192.168.100.10/24")
	wantProblem(t, rep, CheckP2, "enp1s0f1np1 has the address fd00::1/64")
	wantProblem(t, rep, CheckP2, "enP2p1s0f0np0 carries the route fd00:1::/64")
	wantProblem(t, rep, CheckP2, "enP2p1s0f1np1 carries the route 10.9.0.0/16")
	if strings.Contains(texts(rep), "fe80::") {
		t.Errorf("an IPv6 link-local address or route was refused: %s", texts(rep))
	}
	for _, nd := range []string{"enp1s0f0np0", "enp1s0f1np1", "enP2p1s0f0np0", "enP2p1s0f1np1"} {
		if rep.Eligible[nd] {
			t.Errorf("%s is configured on the host but eligible for raw frames", nd)
		}
	}
}

func TestP3BondsBridgesAndUppers(t *testing.T) {
	h := sparkHost(t)
	h.Links["/sys/class/net/enp1s0f0np0/master"] = "../../bond0"
	h.Files["/sys/class/net/enp1s0f1np1/upper_enp1s0f1np1.100"] = nil
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP3, "enp1s0f0np0 is a member of bond0")
	wantProblem(t, rep, CheckP3, "enp1s0f1np1 has enp1s0f1np1.100")
	if !rep.Eligible["enP2p1s0f0np0"] || rep.Eligible["enp1s0f0np0"] {
		t.Errorf("eligibility = %v", rep.Eligible)
	}
}

func TestP4ManagementAndRelayRoutesStayOnTheHost(t *testing.T) {
	h := sparkHost(t)
	h.Outputs["ip route get 1.1.1.1"] = "1.1.1.1 via 10.0.0.1 dev enp1s0f0np0 src 10.0.0.5\n"
	h.Outputs["ip route get 162.244.81.236"] = "162.244.81.236 via 10.9.0.1 dev enP2p1s0f1np1 src 10.9.0.5\n"
	o := opts()
	o.RelayAddrs = func() []string { return []string{"162.244.81.236"} }
	rep := Preflight(h, o)
	wantProblem(t, rep, CheckP4, "enp1s0f0np0 is this machine's route to the internet")
	wantProblem(t, rep, CheckP4, "enP2p1s0f1np1 is this machine's route to the relay (162.244.81.236)")
}

func TestP5NetworkManagerAndNetplan(t *testing.T) {
	h := sparkHost(t)
	h.Outputs["nmcli -t -f DEVICE,STATE device"] = "enP7s7:connected\nenp1s0f0np0:disconnected\nenp1s0f1np1:unmanaged\n"
	h.Files["/etc/netplan/50-cloud-init.yaml"] = []byte("network:\n  version: 2\n  ethernets:\n    cx:\n      match:\n        name: \"enP2p1*\"\n")
	h.Files["/etc/netplan/60-mac.yaml"] = []byte("network:\n  ethernets:\n    port:\n      match:\n        macaddress: \"58:A2:E1:00:00:02\"\n      set-name: fast0\n")
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP5, "NetworkManager manages enp1s0f0np0 (state disconnected)")
	wantProblem(t, rep, CheckP5, "enP2p1s0f0np0 is configured in /etc/netplan/50-cloud-init.yaml")
	wantProblem(t, rep, CheckP5, "enP2p1s0f1np1 is configured in /etc/netplan/50-cloud-init.yaml")
	wantProblem(t, rep, CheckP5, "enp1s0f1np1 is configured in /etc/netplan/60-mac.yaml")
	if strings.Contains(texts(rep), "NetworkManager manages enp1s0f1np1") {
		t.Errorf("an unmanaged interface was refused: %s", texts(rep))
	}

	// No NetworkManager at all is fine.
	h = sparkHost(t)
	h.Tools = map[string]bool{"ip": true, "ethtool": true, "devlink": true, "mstconfig": true}
	h.Outputs["nmcli -t -f DEVICE,STATE device"] = "enp1s0f0np0:connected\n"
	if rep := Preflight(h, opts()); !rep.Ready() {
		t.Errorf("a host without nmcli: %s", texts(rep))
	}
}

func TestP6IOMMUGroups(t *testing.T) {
	h := sparkHost(t)
	h.NIC("0000:01:00.0", "enp1s0f0np0", "58:a2:e1:00:00:01", "MT2412X00001", "0000:01:00.0", "0000:01:00.1", "0000:00:02.0", gpuBDF, "0000:05:00.0")
	h.PCI("0000:00:02.0", "pcieport", "0x060400")
	h.PCI(gpuBDF, "nvidia", "0x030200")
	h.PCI("0000:05:00.0", "nvme", "0x010802")
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP6, "PCI device 0000:05:00.0 shares an IOMMU group with the ConnectX card")
	for _, ok := range []string{"0000:01:00.1 shares", "0000:00:02.0", gpuBDF + " shares"} {
		if strings.Contains(texts(rep), ok) {
			t.Errorf("a function of the same card, a bridge or the GPU was refused: %s", texts(rep))
		}
	}

	delete(h.Files, "/sys/bus/pci/devices/0002:01:00.1/iommu_group/devices/0002:01:00.1")
	wantProblem(t, Preflight(h, opts()), CheckP6, "0002:01:00.1 is in no IOMMU group")
}

func TestP7RDMAUsers(t *testing.T) {
	h := sparkHost(t)
	h.Links["/proc/777/fd/4"] = "/dev/infiniband/uverbs0"
	h.Files["/proc/777/comm"] = []byte("ib_write_bw\n")
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP7, "ib_write_bw (pid 777)")
	if len(rep.EligiblePorts()) != 0 {
		t.Errorf("ports in use by RDMA are eligible for raw frames")
	}
}

func TestP8ToolsAndP9BaseImage(t *testing.T) {
	h := sparkHost(t)
	h.Tools = map[string]bool{"ip": true, "ethtool": true, "devlink": true}
	golden, _ := json.Marshal(vmrt.GoldenInfo{Base: "resolute-server-cloudimg-arm64.img"})
	h.Files[dataDir+"/golden.img.json"] = golden
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP8, "(mstconfig); install them with: apt-get install -y iproute2 ethtool mstflint")
	wantProblem(t, rep, CheckP9, "without the RDMA tools")
}

func TestP10PairTestOnlyAfterEverythingElse(t *testing.T) {
	h := sparkHost(t)
	delete(h.Files, vmrt.PairTestPath(dataDir))
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckP10, "check --boot --pair' on both machines within 10 minutes")
	if len(rep.BlockingPairTest()) != 0 {
		t.Errorf("a missing pair test blocks the pair test: %v", rep.BlockingPairTest())
	}

	o := opts()
	o.Version = "v0.2.1"
	wantProblem(t, Preflight(sparkHost(t), o), CheckP10, "with agent v0.2.0-dev, not v0.2.1")

	h = sparkHost(t)
	h.Files["/sys/class/net/enp1s0f1np1/address"] = []byte("58:a2:e1:00:0f:02\n")
	wantProblem(t, Preflight(h, opts()), CheckP10, "58:a2:e1:00:00:02 its pair test ran over is no longer on this machine")

	h = sparkHost(t)
	h.Tools = map[string]bool{"ip": true, "ethtool": true, "devlink": true}
	rep = Preflight(h, opts())
	for _, p := range rep.Problems {
		if p.Check == CheckP10 {
			t.Errorf("pair test judged before the tools exist: %s", p.Text)
		}
	}
	if rep.PairTest == nil || !rep.PairTest.Passed {
		t.Errorf("the pair test on disk is not reported: %+v", rep.PairTest)
	}
}

func TestIdentityIsAProblemAndReadsRawDMI(t *testing.T) {
	h := sparkHost(t)
	h.Files["/sys/class/dmi/id/sys_vendor"] = []byte("ASUSTeK COMPUTER INC.\n")
	h.Files["/sys/class/dmi/id/product_name"] = []byte("ASUS Ascent GX10\n")
	delete(h.Files, "/sys/class/dmi/id/product_family")
	rep := Preflight(h, opts())
	wantProblem(t, rep, CheckIdentity, "linked pairs need two NVIDIA DGX Sparks: system vendor 'ASUSTeK COMPUTER INC.' is not NVIDIA")
	if rep.Identity.ProductName != "ASUS Ascent GX10" || rep.Identity.ProductFamily != "" || rep.Identity.BoardName != "P4242" {
		t.Errorf("raw identity = %+v", rep.Identity)
	}
	if len(rep.BlockingPairTest()) != 0 {
		t.Errorf("identity blocks the pair test boot: %v", rep.BlockingPairTest())
	}
}

func TestMatchIdentity(t *testing.T) {
	spark := control.Identity{SysVendor: "NVIDIA", ProductName: "DGX Spark", BoardName: "P4242", ProductFamily: "DGX Spark"}
	gb10 := []string{"NVIDIA GB10"}
	cases := []struct {
		name   string
		id     control.Identity
		arch   string
		models []string
		ok     bool
		reason string
	}{
		{"spark", spark, "arm64", gb10, true, ""},
		{"product spelled with underscore", control.Identity{SysVendor: "Nvidia Corporation", ProductName: "NVIDIA DGX_Spark"}, "arm64", gb10, true, ""},
		{"only the family says so", control.Identity{SysVendor: "NVIDIA", ProductName: "P4242-A", ProductFamily: "DGX-Spark"}, "arm64", gb10, true, ""},
		{"x86", spark, "amd64", gb10, false, "this machine is amd64, and an NVIDIA DGX Spark is arm64"},
		{"another GB10 box", control.Identity{SysVendor: "NVIDIA", ProductName: "ASUS Ascent GX10"}, "arm64", gb10, false, "product name 'ASUS Ascent GX10' is not an NVIDIA DGX Spark"},
		{"HP", control.Identity{SysVendor: "HP", ProductName: "HP ZGX Nano G1n"}, "arm64", gb10, false, "system vendor 'HP' is not NVIDIA"},
		{"no vendor", control.Identity{ProductName: "DGX Spark"}, "arm64", gb10, false, "does not report its system vendor"},
		{"no product", control.Identity{SysVendor: "NVIDIA"}, "arm64", gb10, false, "does not report its product name"},
		{"no GB10", spark, "arm64", []string{"NVIDIA RTX PRO 6000"}, false, "no NVIDIA GB10 GPU was detected"},
		{"no GPU", spark, "arm64", nil, false, "no NVIDIA GB10 GPU was detected"},
	}
	for _, c := range cases {
		got := MatchIdentity(c.id, "linux", c.arch, c.models)
		if got.ConfirmedDGXSpark != c.ok || (c.ok && got.Reason != "") || (!c.ok && !strings.Contains(got.Reason, c.reason)) {
			t.Errorf("%s: confirmed=%v reason=%q, want %v %q", c.name, got.ConfirmedDGXSpark, got.Reason, c.ok, c.reason)
		}
	}
	long := MatchIdentity(control.Identity{SysVendor: "NVIDIA\x00\x01 " + strings.Repeat("x", 200)}, "linux", "arm64", gb10)
	if len(long.SysVendor) != 80 || !strings.HasPrefix(long.SysVendor, "NVIDIA??") {
		t.Errorf("DMI value not cleaned and capped: %q (%d)", long.SysVendor, len(long.SysVendor))
	}
}

func TestNonLinuxHasOnlyThePlatformAndIdentity(t *testing.T) {
	o := opts()
	o.GOOS, o.Arch = "darwin", "arm64"
	rep := Preflight(fakehost.New(), o)
	if len(rep.Problems) != 2 || rep.Problems[0].Check != CheckPlatform || rep.Problems[1].Check != CheckIdentity {
		t.Errorf("problems = %+v", rep.Problems)
	}
}

func TestCapabilityIsBoundedAndAlwaysShaped(t *testing.T) {
	rep := Report{}
	for i := 0; i < 14; i++ {
		rep.Problems = append(rep.Problems, Problem{Check: CheckP2, Text: fmt.Sprintf("reason %d", i)})
	}
	for i := 0; i < 11; i++ {
		rep.NICs = append(rep.NICs, NIC{Ports: []Port{{Netdev: fmt.Sprintf("p%d", i), MAC: fmt.Sprintf("58:a2:e1:00:00:%02x", i)}}})
	}
	var peers []control.Peer
	for i := 0; i < 7; i++ {
		peers = append(peers, control.Peer{ListingID: fmt.Sprintf("L%d", i)})
	}
	_, ic := rep.Capability(peers)
	if !ic.Supported || ic.Ready || len(ic.Reasons) != 10 || len(ic.Ports) != 8 || len(ic.Peers) != 4 {
		t.Errorf("bounds: supported=%v ready=%v reasons=%d ports=%d peers=%d", ic.Supported, ic.Ready, len(ic.Reasons), len(ic.Ports), len(ic.Peers))
	}

	id, empty := Report{}.Capability(nil)
	data, _ := json.Marshal(map[string]interface{}{"identity": id, "interconnect": empty})
	for _, want := range []string{`"reasons":[]`, `"ports":[]`, `"peers":[]`, `"pair_test":null`, `"confirmed_dgx_spark":false`, `"reason":""`, `"supported":true`, `"ready":true`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("capability JSON missing %s: %s", want, data)
		}
	}
}

func TestReadySparkCapability(t *testing.T) {
	rep := Preflight(sparkHost(t), opts())
	id, ic := rep.Capability(nil)
	if !id.ConfirmedDGXSpark || !ic.Ready || ic.PairTest == nil || !ic.PairTest.Passed || len(ic.PairTest.RDMAGbps) != 2 || len(ic.Ports) != 4 {
		t.Errorf("identity=%+v interconnect=%+v", id, ic)
	}
}

func TestPeersKeepADayAndOnlyPresentPorts(t *testing.T) {
	now := int64(1789000000)
	known := []control.Peer{
		{ListingID: "B", LocalMAC: "58:a2:e1:00:00:01", PeerMAC: "58:a2:e1:00:01:01", SeenAt: now - 3600},
		{ListingID: "old", LocalMAC: "58:a2:e1:00:00:01", PeerMAC: "58:a2:e1:00:09:01", SeenAt: now - PeerMaxAge - 1},
		{ListingID: "gone-port", LocalMAC: "58:a2:e1:00:0f:0f", PeerMAC: "58:a2:e1:00:01:03", SeenAt: now - 10},
	}
	heard := []control.Peer{{ListingID: "B", LocalMAC: "58:a2:e1:00:00:01", PeerMAC: "58:a2:e1:00:01:01", SeenAt: now}}
	merged := MergePeers(known, heard, now)
	if len(merged) != 2 || merged[0].SeenAt != now {
		t.Fatalf("merged = %+v", merged)
	}
	recent := RecentPeers(merged, now, []string{"58:a2:e1:00:00:01"})
	if len(recent) != 1 || recent[0].ListingID != "B" {
		t.Errorf("recent = %+v", recent)
	}

	h := fakehost.New()
	if _, err := SavePeers(h, dataDir, heard, now); err != nil {
		t.Fatal(err)
	}
	if got := LoadPeers(h, dataDir); len(got) != 1 || got[0].ListingID != "B" {
		t.Errorf("peers on disk = %+v", got)
	}
}
