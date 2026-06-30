package provisioner

import (
	"fmt"
	"testing"
)

type fakeRunner struct {
	failCmd map[string]bool   // command name -> should error
	output  map[string]string // command name -> Output result
}

func (f *fakeRunner) Run(name string, args ...string) error {
	if f.failCmd[name] {
		return fmt.Errorf("simulated failure: %s", name)
	}
	return nil
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	if f.failCmd[name] {
		return "", fmt.Errorf("simulated failure: %s", name)
	}
	return f.output[name], nil
}

func newProv(r Runner) *Provisioner {
	return New(r, "/img/ubuntu.img", "/disks", []string{"0000:01:00.0"})
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
