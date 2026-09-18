package provisioner

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// PairTestWait is how long a pair test waits for the other machine: the host
// runs the test on both machines within ten minutes of each other.
var PairTestWait = 10 * time.Minute

// PairTestRun is one pair test boot, as `gpu-agent check --boot --pair` asks
// for it.
type PairTestRun struct {
	MinRDMAGbps float64
	// Found is told when the other machine has been heard and the plan made.
	Found func(plan interconnect.PairPlan)
}

// ErrPairTestBlocked: something other than the pair test itself stands
// between this machine and a pair, so a test boot would prove nothing.
var ErrPairTestBlocked = errors.New("this machine cannot run a pair test boot yet")

// PairTestBlockers are what must be fixed before a pair test boot can run:
// every reason this machine cannot host a rental except its own test boot,
// and every pair problem except the pair test and the identity (a test on a
// staging machine that is not a Spark still proves the runtime).
func (p *Provisioner) PairTestBlockers() []string {
	var out []string
	for _, f := range p.Findings() {
		if f.Kind != ReasonTestBoot {
			out = append(out, f.Text)
		}
	}
	if p.Withdrawn() {
		out = append(out, "this machine was removed from the marketplace")
	}
	if p.host == nil {
		return append(out, "this agent has no pair runtime on this machine")
	}
	for _, problem := range p.freshPair().BlockingPairTest() {
		out = append(out, problem.Text)
	}
	return out
}

// PairSelfTest runs the pair test boot: it finds the other machine on the
// cable (waiting up to PairTestWait), agrees the plan with it, boots a test VM
// with the GPU and the ConnectX card, measures every link, tears it down, and
// records the verdict in pairtest.json -- a failure too. The error is for a
// test that could not run at all, which records nothing.
func (p *Provisioner) PairSelfTest(o PairTestRun) (vmrt.PairTestResult, error) {
	if !p.hasPairRuntime() || p.runtime == nil {
		return vmrt.PairTestResult{}, errors.New("this agent has no pair runtime on this machine")
	}
	if p.machine.Present() {
		return vmrt.PairTestResult{}, errors.New("a rental (or the leftover of one) is on this machine; a pair test boot cannot run now")
	}
	if blockers := p.PairTestBlockers(); len(blockers) > 0 {
		return vmrt.PairTestResult{}, fmt.Errorf("%w: %s", ErrPairTestBlocked, strings.Join(blockers, "; "))
	}
	unlock, ok, err := p.lockFrames()
	if err != nil {
		return vmrt.PairTestResult{}, fmt.Errorf("take the cable-check lock: %w", err)
	}
	if !ok {
		return vmrt.PairTestResult{}, errors.New("a cable check is running on this machine; try again in a minute")
	}
	defer unlock()

	min := o.MinRDMAGbps
	if min <= 0 {
		min = vmrt.DefaultMinRDMAGbps
	}
	self := p.selfListing()
	if !control.ValidListingID(self) {
		// Unregistered (a staging machine): any name the other side can tell
		// apart from its own will do.
		var b [4]byte
		_, _ = rand.Read(b[:])
		self = "unlisted-" + hex.EncodeToString(b[:])
	}
	rep := p.freshPair()
	failed := func(problem string) vmrt.PairTestResult {
		res := vmrt.PairTestResult{AgentVersion: p.version, MinRDMAGbps: min, At: p.clock().Unix(), Problems: []string{problem}}
		_ = vmrt.SavePairTest(p.host, p.dataDir, res)
		p.RefreshPair()
		return res
	}
	found, err := interconnect.FindPeer(p.host, p.openPacket, rep.FramePorts(), self, PairTestWait, rep.OwnMACs(),
		interconnect.JournalPath(p.dataDir))
	if err != nil {
		return failed(err.Error()), nil
	}
	if _, err := interconnect.SavePeers(p.host, p.dataDir, found.Peers(p.clock()), p.clock().Unix()); err != nil {
		return failed("record the other machine: " + err.Error()), nil
	}
	plan := interconnect.PlanPairTest(found)
	if o.Found != nil {
		o.Found(plan)
	}
	opts := &vmrt.PairOptions{Node: plan.Node, PeerHostname: plan.PeerHostname, Links: plan.Links, MTU: vmrt.PairMTU,
		Functions: rep.Functions()}
	used := map[string]bool{}
	for _, m := range plan.LocalMACs {
		used[m] = true
	}
	for _, m := range rep.PortMACs() {
		if m = strings.ToLower(m); !used[m] {
			opts.OtherMACs = append(opts.OtherMACs, m)
		}
	}
	run := p.pairTest
	if run == nil {
		run = p.runtime.PairSelfTest
	}
	res := run(p.version, vmrt.PairSelfTestOptions{ID: plan.ID, Pair: opts, MinRDMAGbps: min,
		PeerMACs: plan.PeerMACs, PeerListing: plan.PeerListing})
	p.RefreshPair()
	return res, nil
}
