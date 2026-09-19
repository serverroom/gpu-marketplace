package vmrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/stats"
)

// ContainerTest is the outcome of one container test boot, kept on disk. A
// machine reports ready in container mode only while its latest test passed for
// this agent version, these GPUs and this rental image (ContainerFingerprint).
type ContainerTest struct {
	Fingerprint  string   `json:"fingerprint"`
	Passed       bool     `json:"passed"`
	GPUVerified  bool     `json:"gpu_verified"`
	AgentVersion string   `json:"agent_version"`
	Image        string   `json:"image"`
	GPUs         []string `json:"gpus"`
	Internet     bool     `json:"internet"`
	Blocked      []string `json:"blocked"`
	Problems     []string `json:"problems,omitempty"`
	At           int64    `json:"at"`
	// Stopped: the test was cut short and its container torn down clean, so
	// nothing was recorded. Never written to disk.
	Stopped bool `json:"-"`
}

// ContainerSelfTestPath is where the latest container test result lives.
func ContainerSelfTestPath(dataDir string) string {
	return filepath.Join(dataDir, "container-selftest.json")
}

// ContainerFingerprint is the machine a container test's verdict holds for: the
// agent version, the GPUs a rental gets, and the rental image.
func ContainerFingerprint(spec Spec, version string) string {
	gpus := append([]string(nil), spec.GPUs...)
	sort.Strings(gpus)
	sum := sha256.Sum256([]byte(version + "|" + ContainerImageRef + "|" + strings.Join(gpus, ",")))
	return hex.EncodeToString(sum[:])
}

// SaveContainerTest records a result.
func SaveContainerTest(h Host, dataDir string, t ContainerTest) error {
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return h.WriteFile(ContainerSelfTestPath(dataDir), data, 0600)
}

// LoadContainerTest returns the latest result, or nil when there is none.
func LoadContainerTest(h Host, dataDir string) (*ContainerTest, error) {
	data, err := h.ReadFile(ContainerSelfTestPath(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var t ContainerTest
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ContainerSelfTestProblem is the reason a container test does not clear this
// machine to host, or "": no test yet, one for a different version/GPUs/image,
// or one that did not pass.
func ContainerSelfTestProblem(t *ContainerTest, fingerprint string) string {
	if t == nil {
		return "no container test boot has run yet; run 'sudo gpu-agent check --boot'"
	}
	if t.Fingerprint != fingerprint {
		return "the container test boot was for a different agent version, GPU set or image; run 'sudo gpu-agent check --boot'"
	}
	if !t.Passed {
		if len(t.Problems) > 0 {
			return "the last container test boot failed: " + strings.Join(t.Problems, "; ")
		}
		return "the last container test boot did not pass; run 'sudo gpu-agent check --boot'"
	}
	return ""
}

// EvaluateContainer turns what the probe printed and the teardown verified into
// a container-test verdict. Unlike a microVM, a container has no PCI
// passthrough to check: the GPU is present when nvidia-smi lists it over CDI.
func EvaluateContainer(rep SerialReport, expectGPU bool, gpuCount int, probes []string, sshOpened bool, stop StopResult) (passed, gpuVerified bool, problems []string) {
	add := func(format string, a ...interface{}) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if !rep.End {
		add("the container's self-test never finished (see 'podman logs')")
	}
	if !sshOpened {
		add("the container's SSH port never opened, so a renter could not have logged in")
	}
	if expectGPU {
		if rep.NVSMIFail != "" {
			add("nvidia-smi failed in the container: %s", rep.NVSMIFail)
		} else if len(rep.NVSMI) < gpuCount {
			add("the container saw %d of %d GPUs over CDI", len(rep.NVSMI), gpuCount)
		}
	}
	if rep.End && rep.Internet != "ok" {
		add("the container could not reach the internet")
	}
	for _, t := range rep.Reached {
		add("the container reached %s, which the fence must block", t)
	}
	seen := map[string]bool{}
	for _, t := range append(append([]string(nil), rep.Blocked...), rep.Reached...) {
		seen[t] = true
	}
	for _, p := range probes {
		if rep.End && !seen[p] {
			add("the container reported nothing for %s", p)
		}
	}
	if !stop.Clean() {
		add("the teardown did not verify: %s", strings.Join(stop.Detail, "; "))
	}
	passed = len(problems) == 0
	gpuVerified = passed && expectGPU
	return passed, gpuVerified, problems
}

// SelfTest runs a container test boot: it starts a probe container (sshd up,
// the probe printing markers to its logs), reads the verdict, tears it down and
// records the result. It mirrors *Runtime.SelfTest for the container path.
func (rt *ContainerRuntime) SelfTest(version string) ContainerTest {
	return rt.SelfTestContext(context.Background(), version)
}

// SelfTestContext is SelfTest that ctx can stop early. It starts a real rental
// container (which waits for its sshd to open -- proving a renter could log in),
// runs the probe inside it over `podman exec` (GPU over CDI, internet, and the
// fenced targets), then tears it down and records the verdict.
func (rt *ContainerRuntime) SelfTestContext(ctx context.Context, version string) ContainerTest {
	now := time.Now().Unix()
	fp := ContainerFingerprint(rt.spec, version)
	expectGPU := len(rt.spec.GPUs) > 0
	stamp := func(t ContainerTest) ContainerTest {
		t.Fingerprint, t.AgentVersion, t.At = fp, version, now
		t.Image, t.GPUs = ContainerImageRef, rt.spec.GPUs
		return t
	}
	fail := func(problem string) ContainerTest {
		t := stamp(ContainerTest{Problems: []string{problem}})
		_ = SaveContainerTest(rt.h, rt.spec.DataDir, t)
		return t
	}
	if ctx.Err() != nil {
		t := stamp(ContainerTest{Problems: []string{"the test boot was stopped before it started"}})
		t.Stopped = true
		return t
	}
	resume := stats.PauseGPUQueries()
	defer resume()
	pub, err := ThrowawayPubkey()
	if err != nil {
		return fail("could not make a test key: " + err.Error())
	}
	probes := DefaultProbes(rt.h)
	id := fmt.Sprintf("%s%d", SelfTestPrefix, now)
	name := containerName(id)
	// Start waits for the container's sshd to open (BootTimeout); its success
	// is the proof that a renter could have logged in.
	if err := rt.Start(StartOptions{ID: id, Pubkey: pub}); err != nil {
		problem := "the test container did not start: " + err.Error()
		if st, _ := LoadState(rt.h, rt.spec.DataDir); st != nil && st.Dirty {
			problem += "; and its cleanup did not verify, so this machine refuses rentals until that is fixed"
		}
		return fail(problem)
	}
	sshOpened := true

	// Run the probe inside the live container and read its markers (its output
	// is captured whether or not exec reports an error). Bounded by `timeout` so
	// a wedged probe can never hang the agent.
	out, _ := rt.h.Output("timeout", "120", "podman", "exec", name, "bash", "-c", probeScript(probes))
	rep := ParseSerial(out)

	stopped := ctx.Err() != nil
	stop := rt.Stop()
	passed, gpuVerified, problems := EvaluateContainer(rep, expectGPU, len(rt.spec.GPUs), probes, sshOpened, stop)
	t := stamp(ContainerTest{Passed: passed, GPUVerified: gpuVerified, Internet: rep.Internet == "ok", Blocked: rep.Blocked, Problems: problems})
	if stopped {
		t.Passed, t.GPUVerified = false, false
		t.Problems = append([]string{"the test boot was stopped before it finished"}, t.Problems...)
		if stop.Clean() {
			t.Stopped = true
			return t
		}
	}
	_ = SaveContainerTest(rt.h, rt.spec.DataDir, t)
	return t
}
