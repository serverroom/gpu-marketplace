package vmrt

import (
	"encoding/json"
	"path"
	"time"
)

// A rental's microVM boots with UEFI Secure Boot, so that the kernel of its
// Ubuntu image locks itself down: root in the VM can then no longer map a
// device's registers, read or write /dev/mem, use I/O ports, write MSRs or
// load a kernel module nobody signed -- which is how a renter with root would
// write to the firmware of the GPU the VM was handed. A device passed through
// whole cannot be filtered from outside the VM (the kernel's own words for
// vfio: "we have to trust the hardware isolation"), so the lock is put inside,
// on the one program that can reach the device.
//
// What it takes:
//
//   - a Secure Boot build of the VM's firmware with keys enrolled (the ovmf
//     package ships one beside the plain build), and SMM in the VM, which is
//     what keeps the Secure Boot variables out of the guest kernel's reach;
//   - a rental image whose kernel modules are all signed: the NVIDIA driver
//     as Canonical's signed modules, not built inside the image (seed.go);
//   - a test boot that says, from inside the VM, that it is locked down
//     (selftest.go): a machine is offered as locked down only on that proof.
//
// It is not a guarantee. A kernel bug in the guest gives root back what the
// lockdown took, and the firmware trusts every boot loader Microsoft signed,
// so a renter can still start another signed system in place of the image.
//
// A machine that cannot boot a VM this way -- an old KVM without SMM, a
// firmware package without the Secure Boot build -- keeps renting as before,
// not locked down, and says so: the proof that it cannot is a test boot or an
// image build that never came up with Secure Boot and did without it
// (SecureBootRecord). Arm machines are not covered: KVM there has no SMM.

// SecureBootRecord is what this machine found when it tried to boot a rental
// with Secure Boot: kept only when it could not.
type SecureBootRecord struct {
	Usable bool `json:"usable"`
	// Why is the failure that showed it, in the agent's own words.
	Why          string `json:"why,omitempty"`
	AgentVersion string `json:"agent_version"`
	At           int64  `json:"at"`
}

// SecureBootPath is where the record is kept.
func SecureBootPath(dataDir string) string { return path.Join(dataDir, "secureboot.json") }

// LoadSecureBoot returns the record, or nil when there is none.
func LoadSecureBoot(h Host, dataDir string) *SecureBootRecord {
	data, err := h.ReadFile(SecureBootPath(dataDir))
	if err != nil {
		return nil
	}
	var rec SecureBootRecord
	if json.Unmarshal(data, &rec) != nil {
		return nil
	}
	return &rec
}

// MarkSecureBootUnusable records that this machine could not boot a rental
// with Secure Boot, and why. It holds for this agent version: the next one
// tries again.
func MarkSecureBootUnusable(h Host, dataDir, version, why string) error {
	data, err := json.MarshalIndent(SecureBootRecord{Why: why, AgentVersion: version, At: time.Now().Unix()}, "", "  ")
	if err != nil {
		return err
	}
	if err := h.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	tmp := SecureBootPath(dataDir) + ".tmp"
	if err := h.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return h.Rename(tmp, SecureBootPath(dataDir))
}

// SecureBootRefusal is why this machine does not boot its rentals with Secure
// Boot although its firmware could, or "": this agent version tried and the VM
// never came up.
func SecureBootRefusal(h Host, dataDir, version string) string {
	rec := LoadSecureBoot(h, dataDir)
	if rec == nil || rec.Usable || rec.AgentVersion != version {
		return ""
	}
	if rec.Why == "" {
		return "a rental did not boot with it"
	}
	return rec.Why
}

// secureFirmwareCandidates are the Secure Boot builds of the VM's firmware,
// with the variable store that has the keys enrolled and Secure Boot on.
var secureFirmwareCandidates = map[string][]Firmware{
	"amd64": {
		{Code: "/usr/share/OVMF/OVMF_CODE_4M.secboot.fd", Vars: "/usr/share/OVMF/OVMF_VARS_4M.ms.fd", Secure: true},
		{Code: "/usr/share/OVMF/OVMF_CODE.secboot.fd", Vars: "/usr/share/OVMF/OVMF_VARS.ms.fd", Secure: true},
		{Code: "/usr/share/edk2/ovmf/OVMF_CODE.secboot.fd", Vars: "/usr/share/edk2/ovmf/OVMF_VARS.secboot.fd", Secure: true},
	},
}

// FindSecureFirmware returns the first Secure Boot firmware pair installed for arch.
func FindSecureFirmware(h Host, arch string) (Firmware, bool) {
	for _, fw := range secureFirmwareCandidates[arch] {
		if h.Exists(fw.Code) && h.Exists(fw.Vars) {
			return fw, true
		}
	}
	return Firmware{}, false
}

// ChooseFirmware is the firmware this machine's rentals boot with: the Secure
// Boot build when it is installed and this agent version has not found it
// unusable here, the plain build otherwise.
func ChooseFirmware(h Host, arch, dataDir, version string) (Firmware, bool) {
	if fw, ok := FindSecureFirmware(h, arch); ok && SecureBootRefusal(h, dataDir, version) == "" {
		return fw, true
	}
	return FindFirmware(h, arch)
}

// NotLockedDown says, for the host, why this machine's rentals do not boot
// locked down, or "" when they do.
func NotLockedDown(h Host, spec Spec, version string) string {
	switch {
	case spec.Firmware.Secure:
		return ""
	case spec.Arch != "amd64":
		// Not offered on this kind of machine at all: nothing to explain.
		return ""
	}
	if _, ok := FindSecureFirmware(h, spec.Arch); !ok {
		return "this machine's UEFI firmware package has no Secure Boot build"
	}
	if why := SecureBootRefusal(h, spec.DataDir, version); why != "" {
		return "a rental did not boot with Secure Boot on this machine (" + why + ")"
	}
	return "its firmware was chosen before the Secure Boot build was installed"
}
