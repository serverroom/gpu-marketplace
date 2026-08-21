package provisioner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeRunner struct {
	failCmd map[string]bool   // command name -> should error
	output  map[string]string // command name -> Output result
	calls   []string          // every command name invoked, in order
}

func (f *fakeRunner) Run(name string, args ...string) error {
	f.calls = append(f.calls, name)
	if f.failCmd[name] {
		return fmt.Errorf("simulated failure: %s", name)
	}
	return nil
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	f.calls = append(f.calls, name)
	if f.failCmd[name] {
		return "", fmt.Errorf("simulated failure: %s", name)
	}
	return f.output[name], nil
}

func newProv(r Runner) *Provisioner {
	return New(r, "/img/ubuntu.img", "/disks", []string{"0000:01:00.0"}, VendorNVIDIA)
}

func TestProvisionSetsRented(t *testing.T) {
	p := newProv(&fakeRunner{})
	if err := p.Provision("R1", "ssh-ed25519 K"); err != nil {
		t.Fatal(err)
	}
	if p.Status() != StatusRented {
		t.Errorf("status = %q, want rented", p.Status())
	}
}

func TestTeardownFreeWhenWipeAndResetVerify(t *testing.T) {
	r := &fakeRunner{output: map[string]string{"nvidia-smi": "12\n8"}} // VRAM clear
	p := newProv(r)
	if err := p.Teardown("R1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Status() != StatusFree {
		t.Errorf("status = %q, want free", p.Status())
	}
}

func TestTeardownDirtyWhenGPUResetFails(t *testing.T) {
	r := &fakeRunner{failCmd: map[string]bool{"nvidia-smi": true}}
	p := newProv(r)
	if err := p.Teardown("R1"); err == nil {
		t.Fatal("expected an error when GPU reset fails")
	}
	if p.Status() != StatusDirty {
		t.Errorf("status = %q, want dirty (fail closed)", p.Status())
	}
}

func TestTeardownDirtyWhenVRAMNotClear(t *testing.T) {
	r := &fakeRunner{output: map[string]string{"nvidia-smi": "9000\n8"}} // a GPU still dirty
	p := newProv(r)
	if err := p.Teardown("R1"); err == nil {
		t.Fatal("expected an error when VRAM is not clear")
	}
	if p.Status() != StatusDirty {
		t.Errorf("status = %q, want dirty", p.Status())
	}
}

func TestTeardownDirtyWhenDiskWipeFails(t *testing.T) {
	r := &fakeRunner{
		failCmd: map[string]bool{"rm": true}, // overlay delete fails
		output:  map[string]string{"nvidia-smi": "5\n5"},
	}
	p := newProv(r)
	if err := p.Teardown("R1"); err == nil {
		t.Fatal("expected an error when the disk wipe fails")
	}
	if p.Status() != StatusDirty {
		t.Errorf("status = %q, want dirty", p.Status())
	}
}

func TestVerifyVRAMClear(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"5\n10", true},
		{"5", true},
		{"600\n5", false}, // one GPU over threshold
		{"", false},       // no output -> not clear
		{"abc", false},    // unparseable -> not clear
	}
	for _, c := range cases {
		if got := VerifyVRAMClear(c.out); got != c.want {
			t.Errorf("VerifyVRAMClear(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}

// An AMD host used to shell out to nvidia-smi, which is not installed there, so
// the reset failed, the turnover could not be verified, and the box quarantined
// itself dirty on its FIRST teardown - permanently unrentable.
func TestAMDHostResetsWithRocmSMI(t *testing.T) {
	r := &fakeRunner{}
	r.output = map[string]string{
		"rocm-smi": "device,VRAM Total Memory (B),VRAM Total Used Memory (B)\ncard0,17163091968,4194304\n",
	}
	p := New(r, "/img/ubuntu.img", "/disks", []string{"0"}, VendorAMD)
	if err := p.Provision("r1", "ssh-ed25519 KEY"); err != nil {
		t.Fatalf("Provision on an AMD host: %v", err)
	}
	if err := p.Teardown("r1"); err != nil {
		t.Fatalf("Teardown on an AMD host: %v", err)
	}
	if p.Status() != StatusFree {
		t.Errorf("status = %q, want %q - an AMD host must return to the pool", p.Status(), StatusFree)
	}
	if strings.Contains(strings.Join(r.calls, " "), "nvidia-smi") {
		t.Errorf("an AMD host shelled out to nvidia-smi: %v", r.calls)
	}
}

// Apple Silicon has no passthrough path at all. Refuse the rental up front
// rather than accepting it and bricking the host at teardown.
func TestAppleHostRefusesToProvision(t *testing.T) {
	p := New(&fakeRunner{}, "/img/ubuntu.img", "/disks", nil, VendorApple)
	err := p.Provision("r1", "ssh-ed25519 KEY")
	if !errors.Is(err, ErrVendorCannotIsolate) {
		t.Fatalf("Provision on Apple = %v, want ErrVendorCannotIsolate", err)
	}
	if p.Status() != StatusFree {
		t.Errorf("a refused rental left status %q", p.Status())
	}
}

func TestVerifyAMDVRAMClear(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"clear", "device,VRAM Total Used Memory (B)\ncard0,4194304\n", true},
		{"in use", "device,VRAM Total Used Memory (B)\ncard0,8589934592\n", false},
		{"one dirty of two", "device,VRAM Total Used Memory (B)\ncard0,4194304\ncard1,8589934592\n", false},
		{"no rows", "device,VRAM Total Used Memory (B)\n", false},
		{"unparseable", "device,VRAM Total Used Memory (B)\ncard0,n/a\n", false},
		{"no used column", "device,VRAM Total Memory (B)\ncard0,17163091968\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		if got := VerifyAMDVRAMClear(tt.in); got != tt.want {
			t.Errorf("%s: VerifyAMDVRAMClear = %v, want %v", tt.name, got, tt.want)
		}
	}
}
