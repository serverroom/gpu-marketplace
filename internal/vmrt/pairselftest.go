package vmrt

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// PairTestTimeout bounds a pair test VM: its boot, up to ten minutes waiting
// for the other machine's VM, and an RDMA measurement per link.
var PairTestTimeout = 35 * time.Minute

// DefaultMinRDMAGbps is the RDMA write bandwidth each link must reach (owner
// decision Q9: provisional, calibrated in the pilot).
const DefaultMinRDMAGbps = 100.0

// PairSerialReport is what a pair test VM printed besides the single report.
type PairSerialReport struct {
	Begin, End bool
	Ping       map[int]string  // link -> ok|fail
	RDMA       map[int]float64 // link -> Gb/s
	RDMAFail   map[int]string  // link -> why
	NCCL       string
}

// ParsePairSerial reads the pair test's marker lines out of a serial log.
func ParsePairSerial(log string) PairSerialReport {
	rep := PairSerialReport{Ping: map[int]string{}, RDMA: map[int]float64{}, RDMAFail: map[int]string{}}
	for _, raw := range strings.Split(log, "\n") {
		i := strings.Index(raw, markSelfTest+" ")
		if i < 0 {
			continue
		}
		f := strings.Fields(strings.TrimRight(raw[i+len(markSelfTest)+1:], "\r"))
		if len(f) == 0 {
			continue
		}
		link := func(s string) (int, bool) {
			n, err := strconv.Atoi(s)
			return n, err == nil && n >= 0 && n < 8 && strconv.Itoa(n) == s
		}
		switch {
		case len(f) >= 2 && f[0] == "PAIR" && f[1] == "BEGIN":
			rep.Begin = true
		case len(f) >= 2 && f[0] == "PAIR" && f[1] == "END":
			rep.End = true
		case len(f) == 4 && f[0] == "PAIR" && f[1] == "PING" && (f[2] == "ok" || f[2] == "fail"):
			if n, ok := link(f[3]); ok {
				rep.Ping[n] = f[2]
			}
		case len(f) == 3 && f[0] == "RDMA":
			n, ok := link(f[1])
			v, err := strconv.ParseFloat(f[2], 64)
			if ok && err == nil && v >= 0 && v < 100000 {
				rep.RDMA[n] = v
			}
		case len(f) >= 2 && f[0] == "RDMAFAIL":
			if n, ok := link(f[1]); ok {
				why := strings.Join(f[2:], " ")
				if len(why) > 200 {
					why = why[:200]
				}
				rep.RDMAFail[n] = why
			}
		case len(f) >= 2 && f[0] == "NCCL":
			rep.NCCL = f[1]
		}
	}
	return rep
}

// EvaluatePair turns a pair test into a verdict: everything the single test
// boot checks (the GPU, the internet, the fence, SSH, a clean teardown --
// which for a pair includes the ConnectX card), plus, on every link, 9000-byte
// frames both ways (the pair test's own ping and the rental's link check) and
// an RDMA write bandwidth of at least minGbps. Every problem is listed.
func EvaluatePair(single SerialReport, pair PairSerialReport, guestOK map[int]bool, hostGPUs, probes []string, sshOpened bool,
	stop StopResult, plan *PairOptions, peerMACs []string, minGbps float64, version string, at int64) PairTestResult {
	base := Evaluate(single, hostGPUs, probes, sshOpened, stop, version, at)
	res := PairTestResult{AgentVersion: version, Node: plan.Node, PeerMACs: append([]string(nil), peerMACs...),
		MinRDMAGbps: minGbps, Problems: base.Problems, At: at}
	add := func(format string, a ...interface{}) { res.Problems = append(res.Problems, fmt.Sprintf(format, a...)) }
	if !pair.End {
		add("the test VM never finished its pair report (see last-serial.log)")
	}
	for i, l := range plan.Links {
		res.LocalMACs = append(res.LocalMACs, l.LocalMAC)
		name := fmt.Sprintf("link %d (%s, %s)", i, l.Name, l.LocalMAC)
		if pair.Ping[i] != "ok" {
			add("%s did not carry %d-byte frames to the other machine", name, plan.MTU)
		}
		if !guestOK[i] {
			add("the rental's own link check did not report %s ok", name)
		}
		v, measured := pair.RDMA[i]
		switch {
		case measured && v >= minGbps:
		case measured:
			add("%s measured %.1f Gb/s over RDMA, below the %g Gb/s a pair needs", name, v, minGbps)
		case pair.RDMAFail[i] != "":
			add("%s: RDMA did not run: %s", name, pair.RDMAFail[i])
		case pair.Ping[i] == "ok":
			add("%s: the VM reported no RDMA measurement", name)
		}
		res.RDMAGbps = append(res.RDMAGbps, v)
	}
	res.Passed = len(res.Problems) == 0
	return res
}

// PairSelfTestOptions is one machine's side of a pair test.
type PairSelfTestOptions struct {
	ID          string       // the same on both machines
	Pair        *PairOptions // node, links, peer name, the card's functions
	Plan        PairTestPlan
	MinRDMAGbps float64
	PeerMACs    []string
	PeerListing string
}

// PairSelfTest boots a pair test VM -- a self-test VM with the ConnectX card
// passed through, a key nobody holds, and a throwaway intra-pair key -- lets it
// report, tears it down (verifying the card), and records the verdict in
// pairtest.json. The other machine runs the same at the same time; its VM is
// the far end of every measurement.
func (rt *Runtime) PairSelfTest(version string, o PairSelfTestOptions) PairTestResult {
	now := time.Now().Unix()
	minGbps := o.MinRDMAGbps
	if minGbps <= 0 {
		minGbps = DefaultMinRDMAGbps
	}
	fail := func(problem string) PairTestResult {
		res := PairTestResult{AgentVersion: version, PeerListing: o.PeerListing, PeerMACs: o.PeerMACs, MinRDMAGbps: minGbps,
			At: now, Problems: []string{problem}}
		if o.Pair != nil {
			res.Node = o.Pair.Node
			for _, l := range o.Pair.Links {
				res.LocalMACs = append(res.LocalMACs, l.LocalMAC)
			}
		}
		_ = SavePairTest(rt.h, rt.spec.DataDir, res)
		return res
	}
	if o.Pair == nil {
		return fail("no pair to test")
	}
	if !IsSetupID(o.ID) {
		return fail("a pair test boot's id must end in " + PairTestSuffix)
	}
	// No GPU query of the agent's own runs while the GPU is being tested.
	resume := stats.PauseGPUQueries()
	defer resume()
	pub, err := ThrowawayPubkey()
	if err != nil {
		return fail("could not make a test key: " + err.Error())
	}
	if o.Pair.IntraKey == nil {
		if o.Pair.IntraKey, err = ThrowawayIntraKey(); err != nil {
			return fail("could not make a test key: " + err.Error())
		}
	}
	plan := o.Plan
	if plan.Seconds == 0 {
		plan = DefaultPairTestPlan
	}
	probes := DefaultProbes(rt.h)
	if err := rt.Start(StartOptions{ID: o.ID, Pubkey: pub, Probes: probes, NoWait: true, Pair: o.Pair, PairTest: &plan}); err != nil {
		problem := "the pair test VM did not start: " + err.Error()
		if st, _ := LoadState(rt.h, rt.spec.DataDir); st != nil && st.Dirty {
			problem += "; and its cleanup did not verify, so this machine refuses rentals until that is fixed"
		}
		return fail(problem)
	}

	r := NewRental(rt.spec.Storage(), o.ID)
	var single SerialReport
	var pair PairSerialReport
	guestOK := map[int]bool{}
	sshOpened := false
	for waited := time.Duration(0); waited < PairTestTimeout; waited += pollInterval {
		if !sshOpened && rt.h.DialTCP(net.JoinHostPort(GuestIP, "22"), 3*time.Second) == nil {
			sshOpened = true
		}
		if data, err := rt.h.ReadTail(r.SerialLog, 4<<20); err == nil {
			log := string(data)
			single, pair = ParseSerial(log), ParsePairSerial(log)
			guestOK = map[int]bool{}
			for i, state := range GuestLinkStates(log, len(o.Pair.Links)) {
				guestOK[i] = state == "ok"
			}
		}
		if (single.End && pair.End && sshOpened && len(guestOK) == len(o.Pair.Links)) || !rt.alive(r) {
			break
		}
		rt.h.Sleep(pollInterval)
	}

	stop := rt.Stop()
	res := EvaluatePair(single, pair, guestOK, rt.spec.GPUs, probes, sshOpened, stop, o.Pair, o.PeerMACs, minGbps, version, now)
	res.PeerListing = o.PeerListing
	_ = SavePairTest(rt.h, rt.spec.DataDir, res)
	return res
}

// ThrowawayIntraKey makes an intra-pair key for a pair test VM, in the format
// the control plane sends one in.
func ThrowawayIntraKey() (*IntraKey, error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blob := append(sshString("ssh-ed25519"), sshString(string(pk))...)
	u32 := func(v uint32) []byte {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, v)
		return b
	}
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, err
	}
	var section []byte
	section = append(section, check[:]...)
	section = append(section, check[:]...)
	section = append(section, sshString("ssh-ed25519")...)
	section = append(section, sshString(string(pk))...)
	section = append(section, sshString(string(sk))...)
	section = append(section, sshString("gpu-agent pair test")...)
	for i := 1; len(section)%8 != 0; i++ {
		section = append(section, byte(i))
	}
	raw := []byte(opensshMagic)
	for _, s := range []string{"none", "none", ""} {
		raw = append(raw, sshString(s)...)
	}
	raw = append(raw, u32(1)...)
	raw = append(raw, sshString(string(blob))...)
	raw = append(raw, sshString(string(section))...)
	pub := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
	canonical, err := checkOpenSSHPrivateKey(opensshBegin+"\n"+base64.StdEncoding.EncodeToString(raw)+"\n"+opensshEnd+"\n", pub)
	if err != nil {
		return nil, err
	}
	return &IntraKey{PrivateOpenSSH: canonical, Public: pub}, nil
}
