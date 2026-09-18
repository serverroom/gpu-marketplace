package vmrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PairTestResult is the outcome of one pair test boot (`gpu-agent check --boot
// --pair`), kept on disk next to the single-machine self-test. The pair
// preflight reports a machine ready for a linked pair only while its latest
// result passed, for this agent version, on ConnectX-7 ports still present.
type PairTestResult struct {
	Passed       bool      `json:"passed"`
	AgentVersion string    `json:"agent_version"`
	Node         string    `json:"node,omitempty"`
	PeerListing  string    `json:"peer_listing,omitempty"`
	LocalMACs    []string  `json:"local_macs"`
	PeerMACs     []string  `json:"peer_macs"`
	RDMAGbps     []float64 `json:"rdma_gbps"`
	MinRDMAGbps  float64   `json:"min_rdma_gbps,omitempty"`
	Problems     []string  `json:"problems,omitempty"`
	At           int64     `json:"at"`
}

// PairTestPath is where the latest pair test result lives.
func PairTestPath(dataDir string) string { return filepath.Join(dataDir, "pairtest.json") }

// SavePairTest records a result.
func SavePairTest(h Host, dataDir string, res PairTestResult) error {
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return h.WriteFile(PairTestPath(dataDir), data, 0600)
}

// LoadPairTest returns the latest result, or nil when there is none.
func LoadPairTest(h Host, dataDir string) (*PairTestResult, error) {
	data, err := h.ReadFile(PairTestPath(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var res PairTestResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// PairTestCommand is what a host runs, on both machines, to pass the pair test.
const PairTestCommand = "run 'sudo gpu-agent check --boot --pair' on both machines within 10 minutes"

// PairTestProblem is the reason a machine is not ready for a pair on account of
// its pair test, or "" when a passing test for this version is on disk and every
// ConnectX-7 port it ran over is still on the machine (portMACs).
func PairTestProblem(res *PairTestResult, version string, portMACs []string) string {
	switch {
	case res == nil:
		return "this machine has not passed a pair test boot; " + PairTestCommand
	case !res.Passed:
		return fmt.Sprintf("its last pair test boot failed (%s); fix that and %s", strings.Join(res.Problems, "; "), PairTestCommand)
	case res.AgentVersion != version:
		return fmt.Sprintf("its passing pair test boot was with agent %s, not %s; %s", res.AgentVersion, version, PairTestCommand)
	case len(res.LocalMACs) == 0:
		return "its pair test boot records no ConnectX-7 port; " + PairTestCommand
	}
	present := map[string]bool{}
	for _, m := range portMACs {
		present[strings.ToLower(m)] = true
	}
	for _, m := range res.LocalMACs {
		if !present[strings.ToLower(m)] {
			return fmt.Sprintf("the ConnectX-7 port %s its pair test ran over is no longer on this machine; %s", m, PairTestCommand)
		}
	}
	return ""
}
