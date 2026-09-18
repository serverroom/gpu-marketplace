package vmrt

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/serverroom/gpu-marketplace/internal/vmrt/fakehost"
)

const (
	pairID  = "1a2b3c4d-0000-4000-8000-000000000001"
	nic0    = "0001:01:00.0"
	nic1    = "0001:01:00.1"
	nicMAC0 = "58:a2:e1:00:00:01"
	nicMAC1 = "58:a2:e1:00:00:02"
)

// openSSHKey is an unencrypted ed25519 key in OpenSSH's own format, as
// ssh-keygen (and the control plane) write it, with its public line.
func openSSHKey(t *testing.T) (priv, pub string) { return openSSHKeyWith(t, "none", "none") }

func openSSHKeyWith(t *testing.T, cipher, kdf string) (priv, pub string) {
	t.Helper()
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob := append(sshString("ssh-ed25519"), sshString(string(pk))...)
	u32 := func(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
	var section []byte
	section = append(section, u32(0x11223344)...)
	section = append(section, u32(0x11223344)...)
	section = append(section, sshString("ssh-ed25519")...)
	section = append(section, sshString(string(pk))...)
	section = append(section, sshString(string(sk))...)
	section = append(section, sshString("pair-key")...)
	for i := 1; len(section)%8 != 0; i++ {
		section = append(section, byte(i))
	}
	raw := []byte(opensshMagic)
	raw = append(raw, sshString(cipher)...)
	raw = append(raw, sshString(kdf)...)
	raw = append(raw, sshString("")...)
	raw = append(raw, u32(1)...)
	raw = append(raw, sshString(string(blob))...)
	raw = append(raw, sshString(string(section))...)
	enc := base64.StdEncoding.EncodeToString(raw)
	var b strings.Builder
	b.WriteString(opensshBegin + "\n")
	for len(enc) > 70 {
		b.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	b.WriteString(enc + "\n" + opensshEnd + "\n")
	return b.String(), "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " pair@control"
}

func goodPair(t *testing.T) *PairOptions {
	priv, pub := openSSHKey(t)
	return &PairOptions{
		Node: "a", PeerHostname: "gpu-1a2b3c4d-b", MTU: 9000,
		Links: []GuestLink{
			{LocalMAC: "58:A2:E1:00:00:01", CIDR: "10.200.0.1/30", PeerIP: "10.200.0.2"},
			{LocalMAC: nicMAC1, CIDR: "10.200.1.1/30", PeerIP: "10.200.1.2"},
		},
		Functions: []string{nic0, nic1},
		OtherMACs: []string{"58:a2:e1:00:00:04", "58:a2:e1:00:00:03"},
		IntraKey:  &IntraKey{PrivateOpenSSH: priv, Public: pub},
	}
}

func TestValidatePairAcceptsAndNormalises(t *testing.T) {
	p := goodPair(t)
	if err := ValidatePair(pairID, p); err != nil {
		t.Fatal(err)
	}
	if p.Links[0].LocalMAC != nicMAC0 || p.Links[0].Name != "cx7p0" || p.Links[1].Name != "cx7p1" {
		t.Errorf("links = %+v", p.Links)
	}
	if strings.Join(p.OtherMACs, " ") != "58:a2:e1:00:00:03 58:a2:e1:00:00:04" {
		t.Errorf("other ports = %v", p.OtherMACs)
	}
	if strings.HasSuffix(p.IntraKey.Public, "pair@control") || !strings.HasPrefix(p.IntraKey.PrivateOpenSSH, opensshBegin+"\n") {
		t.Errorf("key not normalised: %q", p.IntraKey.Public)
	}
}

func TestValidatePairRefusesEveryBadField(t *testing.T) {
	otherPriv, _ := openSSHKey(t)
	cases := map[string]func(p *PairOptions){
		"node":            func(p *PairOptions) { p.Node = "c" },
		"peer name":       func(p *PairOptions) { p.PeerHostname = "gpu-1a2b3c4d-a" },
		"peer of another": func(p *PairOptions) { p.PeerHostname = "gpu-deadbeef-b" },
		"peer injection":  func(p *PairOptions) { p.PeerHostname = "gpu-1a2b3c4d-b\nruncmd: [reboot]" },
		"mtu low":         func(p *PairOptions) { p.MTU = 1400 },
		"mtu high":        func(p *PairOptions) { p.MTU = 9216 },
		"no links":        func(p *PairOptions) { p.Links = nil },
		"three links": func(p *PairOptions) {
			p.Links = append(p.Links, GuestLink{LocalMAC: "58:a2:e1:00:00:09", CIDR: "10.200.2.1/30", PeerIP: "10.200.2.2"})
		},
		"mac":                   func(p *PairOptions) { p.Links[0].LocalMAC = "58:a2:e1:00:00" },
		"mac twice":             func(p *PairOptions) { p.Links[1].LocalMAC = nicMAC0 },
		"not a /30":             func(p *PairOptions) { p.Links[0].CIDR = "10.200.0.1/24" },
		"outside 10.200/16":     func(p *PairOptions) { p.Links[0].CIDR, p.Links[0].PeerIP = "10.254.254.1/30", "10.254.254.2" },
		"network address":       func(p *PairOptions) { p.Links[0].CIDR = "10.200.0.0/30" },
		"broadcast peer":        func(p *PairOptions) { p.Links[0].PeerIP = "10.200.0.3" },
		"peer elsewhere":        func(p *PairOptions) { p.Links[0].PeerIP = "10.200.1.2" },
		"same address":          func(p *PairOptions) { p.Links[0].PeerIP = "10.200.0.1" },
		"ipv6":                  func(p *PairOptions) { p.Links[0].CIDR = "fd00::1/126" },
		"subnet twice":          func(p *PairOptions) { p.Links[1].CIDR, p.Links[1].PeerIP = "10.200.0.2/30", "10.200.0.1" },
		"no function":           func(p *PairOptions) { p.Functions = nil },
		"bad function":          func(p *PairOptions) { p.Functions = []string{"0001:01:00.0; reboot"} },
		"other port bad":        func(p *PairOptions) { p.OtherMACs = []string{"x"} },
		"other port is a link":  func(p *PairOptions) { p.OtherMACs = []string{nicMAC1} },
		"rsa intra key":         func(p *PairOptions) { p.IntraKey.Public = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ" },
		"private of another":    func(p *PairOptions) { p.IntraKey.PrivateOpenSSH = otherPriv },
		"private garbage":       func(p *PairOptions) { p.IntraKey.PrivateOpenSSH = opensshBegin + "\nnot base64!\n" + opensshEnd },
		"private with commands": func(p *PairOptions) { p.IntraKey.PrivateOpenSSH += "runcmd: [reboot]\n" },
		"private encrypted": func(p *PairOptions) {
			p.IntraKey.PrivateOpenSSH, p.IntraKey.Public = openSSHKeyWith(t, "aes256-ctr", "bcrypt")
		},
	}
	for name, spoil := range cases {
		p := goodPair(t)
		spoil(p)
		if err := ValidatePair(pairID, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidatePair("R1", goodPair(t)); err == nil {
		t.Errorf("a rental id that gives no gpu-<8 hex> name was accepted")
	}
}

// The seed parses as YAML, and says exactly what the pair needs.
func TestPairSeed(t *testing.T) {
	p := goodPair(t)
	if err := ValidatePair(pairID, p); err != nil {
		t.Fatal(err)
	}
	k, _ := ThrowawayPubkey()
	ud, err := BuildUserData(SeedOptions{ID: pairID, Pubkey: k, Pair: p})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hostname       string   `yaml:"hostname"`
		ManageEtcHosts *bool    `yaml:"manage_etc_hosts"`
		Keys           []string `yaml:"ssh_authorized_keys"`
		WriteFiles     []struct {
			Path, Permissions, Content, Encoding string
			Defer                                bool
		} `yaml:"write_files"`
		Runcmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Fatalf("user-data is not YAML: %v\n%s", err, ud)
	}
	if doc.Hostname != "gpu-1a2b3c4d-a" || doc.ManageEtcHosts == nil || *doc.ManageEtcHosts {
		t.Errorf("hostname %q manage_etc_hosts %v", doc.Hostname, doc.ManageEtcHosts)
	}
	if len(doc.Keys) != 2 || doc.Keys[0] != k || doc.Keys[1] != `from="10.200.0.0/16" `+p.IntraKey.Public {
		t.Errorf("keys = %q", doc.Keys)
	}
	files := map[string]string{}
	for _, f := range doc.WriteFiles {
		content := f.Content
		if f.Encoding == "b64" {
			raw, err := base64.StdEncoding.DecodeString(f.Content)
			if err != nil {
				t.Fatal(err)
			}
			content = string(raw)
			if !f.Defer {
				t.Errorf("%s is written before root's ssh directory exists", f.Path)
			}
		}
		files[f.Path] = content
		if f.Path == "/root/.ssh/id_ed25519" && f.Permissions != "0600" {
			t.Errorf("private key permissions %s", f.Permissions)
		}
	}
	for path, want := range map[string][]string{
		// The renter's page says `ping -c 3 gpu-1a2b3c4d-b` on machine a: the
		// peer's name is its first-link address, and this machine's own name
		// its own first-link address, never 127.0.1.1.
		"/etc/hosts": {"10.200.0.1 gpu-1a2b3c4d-a gpu-1a2b3c4d-a-l1\n", "10.200.0.2 gpu-1a2b3c4d-b peer gpu-1a2b3c4d-b-l1\n",
			"10.200.1.1 gpu-1a2b3c4d-a-l2\n", "10.200.1.2 gpu-1a2b3c4d-b-l2\n", "127.0.0.1 localhost\n"},
		"/etc/gpu-pair.json":                 {`"node": "a"`, `"peer": "gpu-1a2b3c4d-b"`, `"name": "cx7p1"`, `"address": "10.200.1.1/30"`, `"mtu": 9000`},
		"/etc/profile.d/gpu-pair.sh":         {`NCCL_SOCKET_IFNAME="${NCCL_SOCKET_IFNAME:-cx7}"`, `NCCL_IB_HCA="${NCCL_IB_HCA:-mlx5}"`, "NCCL_IB_GID_INDEX"},
		"/usr/local/sbin/gpuagent-linkcheck": {"ping -c3 -W1 -M do -s 8972", "check 0 cx7p0 10.200.0.2 &", "check 1 cx7p1 10.200.1.2 &", "GPUAGENT-LINK ok $i", "GPUAGENT-LINK fail $i"},
		"/root/.ssh/id_ed25519":              {opensshBegin, opensshEnd},
		"/root/.ssh/id_ed25519.pub":          {p.IntraKey.Public},
	} {
		for _, w := range want {
			if !strings.Contains(files[path], w) {
				t.Errorf("%s missing %q:\n%s", path, w, files[path])
			}
		}
	}
	if len(doc.Runcmd) != 1 || strings.Join(doc.Runcmd[0], " ") != "systemd-run --unit=gpuagent-linkcheck --no-block /usr/local/sbin/gpuagent-linkcheck" {
		t.Errorf("runcmd = %v", doc.Runcmd)
	}

	var nc struct {
		Ethernets map[string]struct {
			Match struct {
				MACAddress string `yaml:"macaddress"`
			}
			SetName   string   `yaml:"set-name"`
			MTU       int      `yaml:"mtu"`
			Addresses []string `yaml:"addresses"`
			LinkLocal []string `yaml:"link-local"`
			Optional  bool     `yaml:"optional"`
			Routes    []map[string]string
		} `yaml:"ethernets"`
	}
	if err := yaml.Unmarshal([]byte(NetworkConfigFor(p)), &nc); err != nil {
		t.Fatalf("network-config is not YAML: %v", err)
	}
	c0 := nc.Ethernets["cx7p0"]
	if c0.Match.MACAddress != nicMAC0 || c0.SetName != "cx7p0" || c0.MTU != 9000 || strings.Join(c0.Addresses, ",") != "10.200.0.1/30" ||
		c0.LinkLocal == nil || len(c0.LinkLocal) != 0 || !c0.Optional || len(c0.Routes) != 0 {
		t.Errorf("cx7p0 = %+v", c0)
	}
	if c2 := nc.Ethernets["cx7p2"]; c2.Match.MACAddress != "58:a2:e1:00:00:03" || c2.SetName != "cx7p2" || len(c2.Addresses) != 0 || c2.MTU != 0 || !c2.Optional {
		t.Errorf("an unverified port was configured: %+v", c2)
	}
	if r := nc.Ethernets["rental"]; len(r.Routes) != 1 {
		t.Errorf("the rental NIC lost its default route: %+v", r)
	}
}

// pairHost is newHost plus a ConnectX-7 with two ports, each function in an
// IOMMU group of its own, and a pair rental ready to start.
func pairHost() *fakehost.Host {
	h := newHost()
	h.NIC(nic0, "enP1p1s0f0np0", nicMAC0, "MT2412X00001", nic0)
	h.NIC(nic1, "enP1p1s0f1np1", nicMAC1, "MT2412X00001", nic1)
	return h
}

func startPair(t *testing.T, h *fakehost.Host, verify func() bool) (*Runtime, *fakeFence, error) {
	t.Helper()
	rt, fence := newRuntime(h, verify)
	p := goodPair(t)
	p.OtherMACs = nil
	return rt, fence, rt.Start(StartOptions{ID: pairID, Pubkey: key(t), Pair: p})
}

func TestPairStartTakesTheCardLast(t *testing.T) {
	h := pairHost()
	rt, _, err := startPair(t, h, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	// The GPU leaves the host first, then the card, then the VM boots.
	before(t, h, "write /sys/bus/pci/devices/"+testGPU+"/driver_override", "write /sys/bus/pci/devices/"+nic0+"/driver_override")
	before(t, h, "run mstconfig -d "+nic0+" q", "write /sys/bus/pci/devices/"+nic0+"/driver_override")
	before(t, h, "write /sys/bus/pci/devices/"+nic1+"/driver_override", "run systemd-run")
	if h.Driver(nic0) != "vfio-pci" || h.Driver(nic1) != "vfio-pci" || h.Driver(testGPU) != "vfio-pci" {
		t.Errorf("drivers: gpu=%s nic0=%s nic1=%s", h.Driver(testGPU), h.Driver(nic0), h.Driver(nic1))
	}
	launch := h.Call("run systemd-run")
	for _, want := range []string{"vfio-pci,host=" + testGPU, "vfio-pci,host=" + nic0, "vfio-pci,host=" + nic1} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch missing %q", want)
		}
	}
	st, _ := LoadState(h, dataDir)
	if st == nil || len(st.NICDevices) != 2 || st.NICBaseline == nil || st.NICBaseline.MACs[nicMAC0] != "enP1p1s0f0np0" ||
		st.NICBaseline.Firmware[nic0] != "28.40.1000 (NVD0000000033)" || st.NICBaseline.NVConfig[nic1] == "" || st.Pair == nil || st.Pair.Node != "a" {
		t.Fatalf("state = %+v", st)
	}
	data := string(h.Files[StatePath(dataDir)])
	if strings.Contains(data, "OPENSSH") || strings.Contains(data, "intra") {
		t.Errorf("the intra-pair key reached the state file:\n%s", data)
	}
	seed := string(h.Files[NewRental(dataDir, pairID).Dir+"/network-config"])
	if !strings.Contains(seed, "set-name: cx7p1") {
		t.Errorf("network-config = %s", seed)
	}

	res := rt.Stop()
	if !res.Clean() || res.NICDirty {
		t.Fatalf("a clean pair teardown did not verify: %+v", res)
	}
	if h.Driver(nic0) != "mlx5_core" || h.Driver(nic1) != "mlx5_core" {
		t.Errorf("card not given back: %s %s", h.Driver(nic0), h.Driver(nic1))
	}
}

// A boot that fails after the card was taken gives back the card and the GPU.
func TestFailedPairBootGivesBothBack(t *testing.T) {
	h := pairHost()
	h.Fail["systemd-run"] = errors.New("qemu: vfio 0001:01:00.0: failed to setup container")
	_, fence, err := startPair(t, h, func() bool { return true })
	if err == nil {
		t.Fatal("a failed boot started")
	}
	if h.Driver(testGPU) != "nvidia" || h.Driver(nic0) != "mlx5_core" || h.Driver(nic1) != "mlx5_core" {
		t.Errorf("not given back: gpu=%s nic0=%s nic1=%s", h.Driver(testGPU), h.Driver(nic0), h.Driver(nic1))
	}
	if st, _ := LoadState(h, dataDir); st != nil || fence.removed != 1 {
		t.Errorf("state %+v fence removed %d", st, fence.removed)
	}
}

func TestPairThatDoesNotValidateTouchesNothing(t *testing.T) {
	h := pairHost()
	rt, fence := newRuntime(h, nil)
	p := goodPair(t)
	p.Links[0].CIDR = "192.168.1.10/30"
	if err := rt.Start(StartOptions{ID: pairID, Pubkey: key(t), Pair: p}); err == nil {
		t.Fatal("a pair outside 10.200.0.0/16 started")
	}
	if fence.applied != 0 || len(h.Calls) != 0 {
		t.Errorf("work done for a refused pair: %v", h.Calls)
	}
}

func TestRDMAUsersKeepTheCard(t *testing.T) {
	h := pairHost()
	h.Links["/proc/777/fd/4"] = "/dev/infiniband/uverbs0"
	h.Files["/proc/777/comm"] = []byte("ib_write_bw\n")
	_, _, err := startPair(t, h, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), "ib_write_bw (pid 777)") {
		t.Fatalf("Start = %v", err)
	}
	if h.Driver(nic0) != "mlx5_core" || h.Ran("run systemd-run") {
		t.Errorf("a card in use was taken")
	}
}

// Whatever the guest did to the card shows at teardown, and the machine stays
// dirty: a port that never came back, changed firmware, a changed persistent
// configuration, or the host putting an address on a returning port.
func TestPairTeardownCatchesAChangedCard(t *testing.T) {
	NICReturnTimeout = 0
	t.Cleanup(func() { NICReturnTimeout = 60e9 })
	for name, spoil := range map[string]func(h *fakehost.Host){
		"port gone":   func(h *fakehost.Host) { h.LoseNetdev(nic1) },
		"MAC changed": func(h *fakehost.Host) { h.SetNICMAC(nic0, "58:a2:e1:00:00:ff") },
		"firmware":    func(h *fakehost.Host) { h.Outputs["ethtool -i enP1p1s0f0np0"] = "firmware-version: 28.99.9999\n" },
		"NV config":   func(h *fakehost.Host) { h.Outputs["mstconfig -d "+nic1+" q"] = "Configurations:\n SRIOV_EN True(1)\n" },
		"host address": func(h *fakehost.Host) {
			h.Outputs["ip -j addr show dev enP1p1s0f1np1"] = `[{"addr_info":[{"family":"inet","local":"169.254.3.1","scope":"link"}]}]`
		},
	} {
		h := pairHost()
		rt, _, err := startPair(t, h, func() bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		spoil(h)
		res := rt.Stop()
		if res.Clean() || !res.NICDirty || res.Wiped != true || res.GPUClean != true {
			t.Errorf("%s: %+v", name, res)
			continue
		}
		if st, _ := LoadState(h, dataDir); st == nil || !st.Dirty {
			t.Errorf("%s: not quarantined", name)
		}
	}
}

func TestGuestLinkMarkers(t *testing.T) {
	log := "noise GPUAGENT-LINK ok 0\r\nGPUAGENT-LINK fail 1\n[ 9.1] GPUAGENT-LINK ok 1\nGPUAGENT-LINK ok 2\nGPUAGENT-LINK ok 01\nGPUAGENT-LINK1|x|y\n"
	if ok, fail := ParseGuestLinks(log, 2); ok != 2 || fail != 0 {
		t.Errorf("ok=%d fail=%d", ok, fail)
	}
	if ok, fail := ParseGuestLinks("GPUAGENT-LINK fail 0\n", 2); ok != 0 || fail != 1 {
		t.Errorf("ok=%d fail=%d", ok, fail)
	}

	h := pairHost()
	rt, _, err := startPair(t, h, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	h.SetFile(NewRental(dataDir, pairID).SerialLog, []byte(strings.Repeat("x", 100<<10)+"\nGPUAGENT-LINK ok 0\nGPUAGENT-LINK ok 1\n"))
	pair, ok, fail := rt.PairRental()
	if pair == nil || pair.Node != "a" || ok != 2 || fail != 0 {
		t.Errorf("pair=%+v ok=%d fail=%d", pair, ok, fail)
	}
	rt.Stop()
	if pair, _, _ := rt.PairRental(); pair != nil {
		t.Errorf("a pair after teardown: %+v", pair)
	}
}

func TestBakeInstallsTheRDMATools(t *testing.T) {
	ud, err := BakeUserData(DefaultDriver)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rdma-core ibverbs-utils perftest infiniband-diags rdmacm-utils ethtool",
		"modinfo mlx5_ib", "linux-modules-extra-$(uname -r)", "GPUAGENT-BAKE DONE", "GPUAGENT-BAKE FAIL"} {
		if !strings.Contains(ud, want) {
			t.Errorf("bake user-data missing %q:\n%s", want, ud)
		}
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
		t.Errorf("bake user-data is not YAML: %v", err)
	}
}

// bakeHost is a machine on which `runtime prepare` goes all the way: the cloud
// image downloads and matches its checksum, the bake VM runs and reports DONE,
// and the result is flattened into the golden image.
func bakeHost(t *testing.T) *fakehost.Host {
	t.Helper()
	h := newHost()
	h.Files["/usr/share/OVMF/OVMF_CODE_4M.fd"] = []byte("code")
	image := []byte("the resolute cloud image")
	sum := sha256.Sum256(image)
	h.Downloads[cloudImageBase()+"SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + " *" + cloudImageName("amd64") + "\n")
	h.Downloads[cloudImageBase()+cloudImageName("amd64")] = image
	delete(h.OnRun, "systemd-run --unit=") // the bake VM powers itself off
	h.Files[lastSerialLog(dataDir)] = []byte("GPUAGENT-BAKE DONE\n")
	h.OnRun["qemu-img convert"] = func(h *fakehost.Host, cmd string) { h.SetFile(fakehost.LastField(cmd), []byte("golden")) }
	return h
}

func TestPrepareRecordsTheRDMAExtra(t *testing.T) {
	h := bakeHost(t)
	if err := Prepare(h, testSpec(), &fakeFence{h: h}, "v0.2.0-dev", PrepareOptions{}); err != nil {
		t.Fatal(err)
	}
	var info GoldenInfo
	if err := json.Unmarshal(h.Files[dataDir+"/golden.img.json"], &info); err != nil {
		t.Fatal(err)
	}
	if info.Driver != DefaultDriver || strings.Join(info.Extras, ",") != ExtraRDMA || info.Base != cloudImageName("amd64") {
		t.Errorf("golden info = %+v", info)
	}
	if !GoldenHasExtra(h, testSpec(), ExtraRDMA) || GoldenProblem(h, testSpec()) != "" {
		t.Errorf("the new image is not accepted")
	}
}

// The names the renter uses resolve over the cable on both machines, from the
// first boot's own /etc/hosts: each machine's name is its first-link address,
// the other machine's name the other end of that link, and per-link names are
// counted from 1.
func TestPairHostsResolveOverTheCable(t *testing.T) {
	a := goodPair(t)
	if err := ValidatePair(pairID, a); err != nil {
		t.Fatal(err)
	}
	b := &PairOptions{Node: "b", PeerHostname: "gpu-1a2b3c4d-a", MTU: 9000, Functions: []string{nic0},
		Links: []GuestLink{{LocalMAC: "58:a2:e1:00:01:01", CIDR: "10.200.0.2/30", PeerIP: "10.200.0.1"}}}
	if err := ValidatePair(pairID, b); err != nil {
		t.Fatal(err)
	}
	k, _ := ThrowawayPubkey()
	for _, c := range []struct {
		pair      *PairOptions
		want, not []string
	}{
		{a, []string{"\n10.200.0.1 gpu-1a2b3c4d-a gpu-1a2b3c4d-a-l1\n10.200.0.2 gpu-1a2b3c4d-b peer gpu-1a2b3c4d-b-l1\n" +
			"10.200.1.1 gpu-1a2b3c4d-a-l2\n10.200.1.2 gpu-1a2b3c4d-b-l2\n"}, []string{"127.0.1.1", "-l0"}},
		{b, []string{"\n10.200.0.2 gpu-1a2b3c4d-b gpu-1a2b3c4d-b-l1\n10.200.0.1 gpu-1a2b3c4d-a peer gpu-1a2b3c4d-a-l1\n"},
			[]string{"127.0.1.1", "-l2"}},
	} {
		ud, err := BuildUserData(SeedOptions{ID: pairID, Pubkey: k, Pair: c.pair})
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			WriteFiles []struct{ Path, Content string } `yaml:"write_files"`
		}
		if err := yaml.Unmarshal([]byte(ud), &doc); err != nil {
			t.Fatal(err)
		}
		hosts := ""
		for _, f := range doc.WriteFiles {
			if f.Path == "/etc/hosts" {
				hosts = f.Content
			}
		}
		for _, w := range c.want {
			if !strings.Contains(hosts, w) {
				t.Errorf("node %s /etc/hosts missing %q:\n%s", c.pair.Node, w, hosts)
			}
		}
		for _, n := range c.not {
			if strings.Contains(hosts, n) {
				t.Errorf("node %s /etc/hosts has %q:\n%s", c.pair.Node, n, hosts)
			}
		}
		if !strings.Contains(ud, "manage_etc_hosts: false\n") {
			t.Errorf("cloud-init would rewrite /etc/hosts")
		}
	}
}
