package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/control"
)

func TestCheckPairSaysWhatIsMissing(t *testing.T) {
	c := control.Capability{
		Identity: &control.Identity{SysVendor: "NVIDIA", ProductName: "DGX Spark", ConfirmedDGXSpark: true},
		Interconnect: &control.Interconnect{Supported: true, Reasons: []string{"its last pair test boot failed (link 1 measured 61.2 Gb/s)"},
			Ports:    []control.NICPort{{Netdev: "enp1s0f0np0", MAC: "58:a2:e1:00:00:01", BDF: "0000:01:00.0", Carrier: true, SpeedMbps: 200000, MTU: 1500}},
			Peers:    []control.Peer{{ListingID: "B", LocalMAC: "58:a2:e1:00:00:01", PeerMAC: "58:a2:e1:00:01:01", SeenAt: 1789000000}},
			PairTest: &control.PairTest{AgentVersion: "v0.2.0-dev", LocalMACs: []string{"58:a2:e1:00:00:01"}, RDMAGbps: []float64{61.2}, At: 1789000000}},
	}
	var b bytes.Buffer
	printPairCapability(&b, c, time.Unix(1789003600, 0))
	for _, want := range []string{"confirmed NVIDIA DGX Spark", "enp1s0f0np0", "link up at 200 GbE", "listing B", "1h0m0s ago",
		"Pair test:    FAILED", "not ready:", "61.2 Gb/s"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output missing %q:\n%s", want, b.String())
		}
	}
}
