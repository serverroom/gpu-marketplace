package stats

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestArmCPUIdentity(t *testing.T) {
	for _, c := range []struct {
		name, cpuinfo, compatible string
		gpus                      []string
		model, detail             string
	}{
		{"Radxa ROCK 5B (RK3588)", "cpuinfo-rk3588", "dt-compatible-rock5b", nil, "Rockchip RK3588", "4× Cortex-A76 + 4× Cortex-A55"},
		{"an RK3588 board whose device tree names no known SoC", "cpuinfo-rk3588", "", nil, "ARM Cortex-A76 + Cortex-A55", "4× Cortex-A76 + 4× Cortex-A55"},
		{"DGX Spark (GB10)", "cpuinfo-gb10", "", []string{"NVIDIA GB10"}, "NVIDIA GB10", "10× Cortex-X925 + 10× Cortex-A725"},
		{"GB10 cores, GPU not seen", "cpuinfo-gb10", "", nil, "ARM Cortex-X925 + Cortex-A725", "10× Cortex-X925 + 10× Cortex-A725"},
		{"Ampere Altra (Neoverse-N1)", "cpuinfo-altra", "", nil, "ARM Neoverse-N1", "80× Neoverse-N1"},
		{"AmpereOne", "cpuinfo-ampereone", "", nil, "Ampere AmpereOne", "96× AmpereOne"},
		{"x86 has no ARM cores", "cpuinfo-x86", "", nil, "", ""},
	} {
		var compatible []byte
		if c.compatible != "" {
			compatible = fixture(t, c.compatible)
		}
		cores := ParseArmCores(string(fixture(t, c.cpuinfo)))
		if got := ArmCPUModel(compatible, cores, c.gpus); got != c.model {
			t.Errorf("%s: model %q, want %q", c.name, got, c.model)
		}
		if got := CoresDetail(cores); got != c.detail {
			t.Errorf("%s: cores %q, want %q", c.name, got, c.detail)
		}
	}
}

func TestArmCoresGroupBigFirstAndNameUnknownParts(t *testing.T) {
	groups := GroupCores(ParseArmCores(string(fixture(t, "cpuinfo-rk3588"))))
	if len(groups) != 2 || groups[0].Name() != "Cortex-A76" || strings.Join(itoa(groups[0].CPUs), ",") != "4,5,6,7" {
		t.Errorf("groups = %+v", groups)
	}
	odd := []ArmCore{{CPU: 0, Implementer: "0x41", Part: "0xfff"}}
	if got := ArmCPUModel(nil, odd, nil); got != "ARM core 0x41/0xfff" {
		t.Errorf("unknown part = %q", got)
	}
}

func itoa(ns []int) []string {
	var out []string
	for _, n := range ns {
		out = append(out, strings.TrimSpace(string(rune('0'+n))))
	}
	return out
}

func TestBoardName(t *testing.T) {
	if got := BoardName(fixture(t, "dt-model-rock5b")); got != "Radxa ROCK 5B" {
		t.Errorf("board = %q", got)
	}
	if got := BoardName([]byte("A\x01B" + strings.Repeat("x", 200))); len(got) != 80 || !strings.HasPrefix(got, "AB") {
		t.Errorf("board not cleaned and capped: %q", got)
	}
}
