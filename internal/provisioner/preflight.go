package provisioner

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/config"
	"github.com/serverroom/gpu-marketplace/internal/control"
	"github.com/serverroom/gpu-marketplace/internal/interconnect"
	"github.com/serverroom/gpu-marketplace/internal/netguard"
	"github.com/serverroom/gpu-marketplace/internal/pcidev"
	"github.com/serverroom/gpu-marketplace/internal/register"
	"github.com/serverroom/gpu-marketplace/internal/stats"
	"github.com/serverroom/gpu-marketplace/internal/vmrt"
)

// The ways this agent hosts a rental: a QEMU/KVM microVM behind the netguard
// fence, with the machine's GPUs handed to it over VFIO (KindQEMUVFIO), or on
// a machine without a GPU, with its CPUs, memory and disk only (KindQEMU).
const (
	KindQEMUVFIO = "qemu-vfio"
	KindQEMU     = "qemu"
)

// ReasonKind says who can fix a reason this machine is not ready: the agent's
// automatic setup, or a person.
type ReasonKind string

const (
	// ReasonHuman needs a person: hardware, firmware settings, a GPU that
	// shares its group, too little memory, a desktop on a machine that is not
	// a DGX Spark, a quarantined leftover.
	ReasonHuman ReasonKind = "human"
	// ReasonTools: the runtime's packages or UEFI firmware are missing.
	ReasonTools ReasonKind = "tools"
	// ReasonImage: the base image is missing, or built for another Ubuntu
	// release or another NVIDIA driver.
	ReasonImage ReasonKind = "image"
	// ReasonTestBoot: no passing test boot for this agent version and GPUs.
	ReasonTestBoot ReasonKind = "test-boot"
)

// Finding is one reason with its kind.
type Finding struct {
	Kind ReasonKind
	Text string
}

// HostReport is what preflight found: the GPUs a rental would get, and every
// reason this machine cannot host one. No reasons means ready.
type HostReport struct {
	Vendor   GPUVendor
	BDFs     []string
	Models   []string // GPU model names: nvidia-smi's where the host has it, else the PCI ID database's
	Unified  bool
	Firmware vmrt.Firmware
	Reasons  []string
	// Findings are Reasons with their kinds, in the same order.
	Findings []Finding
	// Identity is what the machine says it is (nil off Linux).
	Identity *control.Identity
	// DesktopOnDemand: a confirmed DGX Spark not made headless on purpose; its
	// desktop closes while rented or tested rather than refusing the rental.
	DesktopOnDemand bool
	// GPUCount is how many GPUs the machine has: the ones a rental would get,
	// or, when it can rent none of them now, its GPU cards.
	GPUCount int
	// Excluded are GPUs on this machine that rentals leave out, and why. They
	// are no reason to refuse the machine while another GPU can be rented.
	Excluded []string
	// GPUs are what the last passing test boot saw, once there is one for this
	// version and these GPUs.
	GPUs []vmrt.GuestGPU
}

// Preflight checks, without changing anything, whether this machine can host a
// rental: a Linux KVM host, with the IOMMU on and GPUs of any make that can be
// passed through on their own when it has GPUs (a machine without one, or with
// only its processor's own, hosts CPU-only), the runtime's tools, firmware and
// base image installed, enough memory and disk -- and, once all of that holds,
// a passing test boot on this very machine for this agent version. Every
// failing check is reported in words a provider can act on, not just the
// first.
func Preflight(h vmrt.Host, goos string, spec vmrt.Spec, version string) HostReport {
	var rep HostReport
	addKind := func(kind ReasonKind, format string, a ...interface{}) {
		text := fmt.Sprintf(format, a...)
		rep.Reasons = append(rep.Reasons, text)
		rep.Findings = append(rep.Findings, Finding{Kind: kind, Text: text})
	}
	add := func(format string, a ...interface{}) { addKind(ReasonHuman, format, a...) }

	switch goos {
	case "linux":
	case "darwin":
		rep.Vendor = VendorApple
		add("%v: Apple Silicon has no way to pass its GPU through to a microVM", ErrVendorCannotIsolate)
		return rep
	default:
		add("rentals run inside a Linux KVM microVM, and this machine runs %s", goos)
		return rep
	}

	if !h.Exists("/dev/kvm") {
		add("%s", kvmMissing(h, spec.Arch))
	}
	for _, missing := range missingKernelFeatures(h) {
		add("%s", missing)
	}

	// GPUs, of any make, from the PCI bus. The host needs no GPU driver or
	// vendor tool: the rental's VM is where a GPU has to work, and its test boot
	// proves the GPU gets there. nvidia-smi, where the host has it, adds names
	// and says which GPUs share the machine's memory (a GB10).
	nv := hostNVIDIA(h)
	var nvNames []string
	for _, bdf := range sortedKeys(nv) {
		nvNames = append(nvNames, nv[bdf].name)
	}
	id := ReadIdentity(h, goos, spec.Arch, nvNames)
	rep.Identity = &id
	if id.ConfirmedDGXSpark {
		// Made headless on purpose (runtime prepare --headless): left that way.
		_, err := vmrt.LoadHeadless(h, spec.DataDir)
		rep.DesktopOnDemand = errors.Is(err, os.ErrNotExist)
	}

	groups, _ := h.Glob("/sys/kernel/iommu_groups/*")
	ours := ownRentalDevices(h, spec.DataDir)
	var cards []pcidev.Device
	var excluded []string
	for _, d := range pcidev.Display(h) {
		if pcidev.Integrated(h, d) && !ours[d.BDF] {
			excluded = append(excluded, fmt.Sprintf("GPU %s (%s) is the processor's integrated GPU; it stays with the host", d.BDF, gpuName(h, d, nv)))
			continue
		}
		cards = append(cards, d)
	}
	// A DGX Spark's desktop closes for a rental and comes back after -- as long
	// as the agent can close it, through the display manager.
	desktopCloses := rep.DesktopOnDemand && vmrt.ActiveDisplayManager(h) != ""
	rentable, left := rentableGPUs(h, cards, nv, len(groups) > 0, ours, desktopCloses)
	switch {
	case len(cards) == 0:
		// No GPU a rental could take -- none at all, or only the processor's
		// own: the machine rents its CPUs, memory and disk.
		rep.Vendor = VendorNone
		rep.Excluded = excluded
	case len(rentable) == 0:
		// Cards the host cannot give up now are not a machine without a GPU:
		// renting it out as one would hide the GPU the host means to rent.
		rep.Vendor = VendorPCI
		rep.GPUCount = len(cards)
		for _, why := range left {
			add("%s", why)
		}
		rep.Excluded = excluded
	default:
		rep.Vendor = VendorPCI
		rep.BDFs = rentable
		rep.GPUCount = len(rentable)
		rep.Excluded = append(excluded, left...)
	}
	rentsNVIDIA := false
	for _, bdf := range rep.BDFs {
		d := pcidev.Read(h, bdf)
		rep.Models = append(rep.Models, gpuName(h, d, nv))
		rentsNVIDIA = rentsNVIDIA || d.Vendor == pcidev.NVIDIA
	}
	rep.Unified = hostUnified(nv, rep.BDFs)
	// The IOMMU is what hands a GPU to a microVM; without a GPU it is not needed.
	if rep.GPUCount > 0 && len(groups) == 0 {
		add("the IOMMU is off, so no GPU can be handed to a microVM: enable VT-d / AMD-Vi (or the SMMU on Arm) in the firmware and on the kernel command line")
	}
	// The base image is judged against the GPUs a rental would get.
	spec.GPUs = rep.BDFs

	apt := AptDistro(h)
	if missing := vmrt.MissingTools(h, spec.Arch); len(missing) > 0 {
		if apt {
			addKind(ReasonTools, "the rental runtime's tools are missing (%s); run 'sudo gpu-agent runtime prepare --install-deps'", strings.Join(missing, ", "))
		} else {
			addKind(ReasonTools, "the rental runtime's tools are missing (%s): the agent installs them with apt-get on Ubuntu and Debian (Armbian too); "+
				"on this system install QEMU, UEFI firmware for it, cloud-image-utils, cryptsetup and nftables with its own package manager", strings.Join(missing, ", "))
		}
	}
	if fw, ok := vmrt.FindFirmware(h, spec.Arch); ok {
		rep.Firmware = fw
	} else if apt {
		addKind(ReasonTools, "no UEFI firmware for microVMs is installed; run 'sudo gpu-agent runtime prepare --install-deps'")
	} else {
		addKind(ReasonTools, "no UEFI firmware for microVMs is installed (OVMF on x86, AAVMF on Arm): install it with this system's package manager")
	}
	if !h.Exists(spec.GoldenImage) {
		addKind(ReasonImage, "the rental base image has not been built; run 'sudo gpu-agent runtime prepare'")
	} else if problem := vmrt.GoldenProblem(h, spec); problem != "" {
		addKind(ReasonImage, "%s", problem)
	} else if rentsNVIDIA && vmrt.GoldenDriver(h, spec) == vmrt.NoDriver {
		addKind(ReasonImage, "the rental base image was built without the NVIDIA driver, when this machine had no NVIDIA GPU; rebuild it with 'sudo gpu-agent runtime prepare'")
	} else if rentsNVIDIA {
		if problem := vmrt.GoldenDriverProblem(h, spec, vmrt.ChooseDriver(h)); problem != "" {
			addKind(ReasonImage, "%s", problem)
		}
	}
	if spec.GuestMemoryMB() == 0 {
		add("this machine has %d MB of memory, and a rental needs at least 4 GB: 2 GB for the rental, the rest kept for the machine", spec.TotalMemMB)
	}
	if n := spec.GuestCPUs(); n < 2 {
		add("a rental on this machine would get %d CPU (what the machine keeps for itself taken off), and a rental needs at least 2", n)
	}
	for _, problem := range storageProblems(h, spec.Storage(), spec.DiskGB) {
		add("%s", problem)
	}

	// A test boot proves what the checks above cannot: that this GPU really
	// reaches a VM on this hardware. It only means anything once they pass.
	res, err := vmrt.LoadSelfTest(h, spec.DataDir)
	current := err == nil && vmrt.SelfTestProblem(res, version, rep.BDFs) == ""
	if len(rep.Reasons) == 0 {
		if err != nil {
			addKind(ReasonTestBoot, "its last test boot could not be read (%v); run 'sudo gpu-agent check --boot'", err)
		} else if problem := vmrt.SelfTestProblem(res, version, rep.BDFs); problem != "" {
			addKind(ReasonTestBoot, "%s", problem)
		}
	}
	if current && len(rep.BDFs) > 0 {
		rep.GPUs = res.GPUs
		for _, g := range res.GPUs {
			rep.Unified = rep.Unified || g.Unified
		}
	}
	return rep
}

// Detect runs preflight and returns a provisioner, backed by the real microVM
// runtime, whose Capability says what it found. It is the only constructor
// production code should use.
func Detect(h vmrt.Host, goos, arch, dataDir, version string) *Provisioner {
	storage := StorageDir(dataDir)
	spec := vmrt.Spec{
		Arch:        arch,
		DataDir:     dataDir,
		GoldenImage: filepath.Join(storage, "golden.img"),
		TotalMemMB:  hostMemoryMB(h),
		CPUs:        runtime.NumCPU(),
		DiskGB:      rentalDiskGB(h, storage),
	}
	if storage != dataDir {
		spec.StorageDir = storage
	}
	if goos == "linux" {
		// One core type on a machine with big and little cores.
		cpus := vmrt.ChooseGuestCPUs(h, arch)
		spec.GuestCores, spec.GuestCPUName = cpus.Cores, cpus.Name
	}
	rep := Preflight(h, goos, spec, version)
	spec.GPUs = rep.BDFs
	spec.Unified = rep.Unified
	spec.Firmware = rep.Firmware
	spec.DesktopOnDemand = rep.DesktopOnDemand

	// The pair checks never change whether this machine can host a single
	// rental; they only say whether it can also be half of a linked pair.
	pairOpts := interconnect.Options{GOOS: goos, Arch: arch, Spec: spec, Version: version, GPUModels: rep.Models, RelayAddrs: RelayAddrs}
	pair := interconnect.Preflight(h, pairOpts)
	spec.NICs = pair.Functions()

	fence := netguard.New(h, netguard.Bridge, vmrt.GuestSubnet, netguard.HostNetworks)
	rt := vmrt.New(h, spec, fence, gpuVerifier(h))
	p := New(rt, rep.Vendor, rep.BDFs, rep.Unified)
	p.runtime = rt
	p.findings = rep.Findings
	p.host = h
	p.dataDir = dataDir
	p.version = version
	p.pairOpts = pairOpts
	p.pair = pair
	p.openPacket = interconnect.OpenPacket
	p.lockFrames = func() (func(), bool, error) { return interconnect.TryLock(dataDir) }
	p.now = time.Now
	p.redetect = func() *Provisioner { return Detect(h, goos, arch, dataDir, version) }
	_, ic := pair.Capability(interconnect.RecentPeers(interconnect.LoadPeers(h, dataDir), time.Now().Unix(), pair.PortMACs()))
	kind := KindQEMUVFIO
	var gpuCount *int
	var guest *control.Guest
	// A rental (or its leftover) holds the GPU and the card: no driver sees
	// them, so what the checks say about GPUs is not the machine. The count
	// is left out, a machine whose rental took GPUs is never called GPU-less,
	// and it is all read again once the rental has gone (afterRental).
	midRental := goos == "linux" && rt.Present()
	if midRental && rep.Vendor == VendorNone && rentalTookGPUs(h, dataDir) {
		text := "a rental holds this machine's GPU, so the agent cannot check it now; it checks the machine again once the rental has ended"
		rep.Reasons = append(rep.Reasons, text)
		rep.Findings = append(rep.Findings, Finding{Kind: ReasonHuman, Text: text})
		p.findings = rep.Findings
	}
	if goos == "linux" {
		if !midRental {
			n := rep.GPUCount
			gpuCount = &n
		}
		if rep.Vendor == VendorNone && !(midRental && rentalTookGPUs(h, dataDir)) {
			kind = KindQEMU
		}
		// What the renter gets, from the sizing the VM itself uses.
		guest = &control.Guest{VCPUs: spec.GuestCPUs(), MemoryGB: spec.GuestMemoryMB() / 1024, DiskGB: spec.DiskGB, CPU: spec.GuestCPUName}
	}
	p.midRental = midRental
	p.capability = control.Capability{
		Ready:         len(rep.Reasons) == 0,
		Kind:          kind,
		GPUCount:      gpuCount,
		Guest:         guest,
		Reasons:       rep.Reasons,
		AgentVersion:  version,
		UnifiedMemory: rep.Unified,
		VMUser:        vmrt.VMUser,
		Excluded:      rep.Excluded,
		Identity:      rep.Identity,
		Interconnect:  ic,
	}
	for _, g := range rep.GPUs {
		p.capability.GPUs = append(p.capability.GPUs, control.GPU{
			Model: g.Model, PCIID: g.ID, MemoryMB: g.MemoryMB, Unified: g.Unified, Driver: g.Driver,
		})
	}
	return p
}

// HasNVIDIAGPU reports whether this machine has an NVIDIA GPU: one nvidia-smi
// sees, or one on the PCI bus without a working driver yet.
func HasNVIDIAGPU(h vmrt.Host) bool { return vmrt.HasNVIDIAGPU(h) }

// CheckBakeDriver accepts what --driver may name: nothing (match this
// machine: its own driver branch, or none on a machine without an NVIDIA GPU),
// a driver branch, or "none".
func CheckBakeDriver(flag string) error {
	if flag == "" || flag == vmrt.NoDriver || vmrt.ValidDriver(flag) {
		return nil
	}
	return fmt.Errorf("invalid driver branch %q (for example %s, or none)", flag, vmrt.DefaultDriver)
}

// StorageDir is where the base image and the rentals' disks live: the directory
// `gpu-agent setup --data-dir` chose, else dataDir. A variable so tests never
// read this machine's configuration.
var StorageDir = func(dataDir string) string {
	if dir := config.StorageDir(); dir != config.DataDir() {
		return dir
	}
	return dataDir
}

// RelayAddrs resolves the relay this agent tunnels to, so the pair preflight can
// refuse a ConnectX-7 port that is the host's own route to it. A variable so
// tests never read this machine's tunnel config or DNS.
var RelayAddrs = func() []string {
	cfg, err := register.LoadTunnelConfig()
	if err != nil || cfg == nil || cfg.RelayHost == "" {
		return nil
	}
	if ip := net.ParseIP(cfg.RelayHost); ip != nil {
		return []string{ip.String()}
	}
	addrs, err := net.LookupHost(cfg.RelayHost)
	if err != nil {
		return nil
	}
	return addrs
}

func hostMemoryMB(h vmrt.Host) int {
	data, err := h.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			return kb / 1024
		}
	}
	return 0
}

// rentalDiskGB is the disk a rental gets: what is free under dataDir (or the
// nearest existing parent), less 20 GB for the host, capped at 500 GB.
func rentalDiskGB(h vmrt.Host, dataDir string) int {
	dir := dataDir
	for dir != "" && !h.Exists(dir) {
		parent := filepath.Dir(dir)
		if parent == dir { // the root, on any OS
			break
		}
		dir = parent
	}
	out, err := h.Output("df", "--output=avail", "-B1G", dir)
	if err != nil {
		return 0
	}
	lines := strings.Fields(out)
	if len(lines) < 2 {
		return 0
	}
	avail, err := strconv.Atoi(strings.TrimSuffix(lines[len(lines)-1], "G"))
	if err != nil {
		return 0
	}
	gb := avail - 20
	if gb > 500 {
		gb = 500
	}
	if gb < 0 {
		gb = 0
	}
	return gb
}

// nvidiaRow is one GPU as nvidia-smi on the host describes it.
type nvidiaRow struct {
	name     string
	memoryMB float64 // 0 when nvidia-smi says [N/A]
}

// hostNVIDIA is what nvidia-smi on the host says about its GPUs, by PCI
// address; empty when the host has no NVIDIA driver -- which is no reason not
// to host: it only adds names, and which GPUs share the machine's memory.
func hostNVIDIA(r Runner) map[string]nvidiaRow {
	rows := map[string]nvidiaRow{}
	out, err := r.Output("nvidia-smi", "--query-gpu=pci.bus_id,name,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return rows
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// The name sits between the first and the last comma.
		first, last := strings.Index(line, ","), strings.LastIndex(line, ",")
		if first < 0 || last <= first {
			continue
		}
		bdf := NormalizeBDF(line[:first])
		if bdf == "" {
			continue
		}
		mem, _ := strconv.ParseFloat(strings.TrimSpace(line[last+1:]), 64)
		rows[bdf] = nvidiaRow{name: strings.TrimSpace(line[first+1 : last]), memoryMB: mem}
	}
	return rows
}

func sortedKeys(m map[string]nvidiaRow) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// gpuName is how a GPU is shown: nvidia-smi's name where the host has it,
// else the PCI ID database's.
func gpuName(fs pcidev.FS, d pcidev.Device, nv map[string]nvidiaRow) string {
	if row, ok := nv[d.BDF]; ok && row.name != "" {
		return row.name
	}
	return pcidev.Name(fs, d)
}

// hostUnified reports whether nvidia-smi describes one of these GPUs as a
// unified-memory part: memory.total is [N/A] for a GB10 because its memory is
// the machine's pool, which a rental then gets as both its system memory and
// its GPU memory. A GPU that reports no memory and is not such a part is
// simply a GPU whose memory the host cannot read; the test boot says from
// inside the VM.
func hostUnified(nv map[string]nvidiaRow, bdfs []string) bool {
	for _, b := range bdfs {
		if row, ok := nv[b]; ok && row.memoryMB <= 0 && stats.IsUnifiedMemoryModel(row.name) {
			return true
		}
	}
	return false
}

// ownRentalDevices are the PCI functions this agent's own rental (or test
// boot) holds, from its state: they are on vfio-pci and held by its QEMU, and
// they are this machine's to rent all the same.
func ownRentalDevices(h vmrt.Host, dataDir string) map[string]bool {
	ours := map[string]bool{}
	if st, err := vmrt.LoadState(h, dataDir); err == nil && st != nil {
		for _, d := range st.Devices {
			ours[d.BDF] = true
		}
	}
	return ours
}

// rentableGPUs splits the machine's GPU cards, of whatever make, into those a
// rental can take and those it has to leave, each with why:
//   - one already on vfio-pci belongs to a VM of the provider's own;
//   - one that draws the machine's desktop, unless that desktop closes for a
//     rental (a DGX Spark's) -- the fix is running headless;
//   - one whose IOMMU group holds a device that is not part of a graphics card
//     would take that device from the host too. Cards that share a group with
//     each other are judged together, so they rent or stay together:
//     group-mates have the same group, so they get the same verdict. Isolation
//     is only judged with the IOMMU on; without it nothing can be passed
//     through, which is its own reason;
//   - the boot display (the firmware's and the kernel's console) stays with
//     the host whenever another card can be rented; a machine whose only card
//     it is rents it.
//
// Anything else holding a card is judged when a rental starts, as it always
// was: a process that has it open for a moment when the agent starts is no
// reason to take a card off the market until someone restarts the agent.
// GPUs of this agent's own rental (ours) are this machine's to rent.
func rentableGPUs(h vmrt.Host, cards []pcidev.Device, nv map[string]nvidiaRow, iommu bool, ours map[string]bool, desktopCloses bool) (rentable, excluded []string) {
	names := map[string]string{}
	var free []string
	for _, d := range cards {
		names[d.BDF] = gpuName(h, d, nv)
		if ours[d.BDF] {
			free = append(free, d.BDF)
			continue
		}
		if d.Driver == "vfio-pci" {
			excluded = append(excluded, fmt.Sprintf("GPU %s (%s) is already bound to vfio-pci on this machine, for a VM of its own; unbind it to rent it",
				d.BDF, names[d.BDF]))
			continue
		}
		if desktop, _ := vmrt.ClassifyGPUHolders(h, []string{d.BDF}); len(desktop) > 0 && !desktopCloses {
			excluded = append(excluded, fmt.Sprintf("GPU %s (%s): %s", d.BDF, names[d.BDF], vmrt.DesktopOnGPUProblem(desktop)))
			continue
		}
		free = append(free, d.BDF)
	}

	// isolated keeps the cards whose groups hold nothing but cards going with
	// them, and says why of the rest.
	isolated := func(going []string) (ok []string, why []string) {
		for _, bdf := range going {
			if iommu {
				if problems := vmrt.GroupProblemsWith(h, bdf, going); len(problems) > 0 {
					why = append(why, fmt.Sprintf("GPU %s (%s) cannot be passed through on its own: %s",
						bdf, names[bdf], strings.Join(problems, "; ")))
					continue
				}
			}
			ok = append(ok, bdf)
		}
		return ok, why
	}
	rentable, why := isolated(free)

	// The boot display stays with the host when another card can go -- judged
	// on the cards that can, so it is never left out for a card that cannot.
	if len(rentable) > 1 {
		var keep, boot []string
		for _, bdf := range rentable {
			if data, err := h.ReadFile("/sys/bus/pci/devices/" + bdf + "/boot_vga"); err == nil && strings.TrimSpace(string(data)) == "1" && !ours[bdf] {
				boot = append(boot, bdf)
			} else {
				keep = append(keep, bdf)
			}
		}
		// Without the boot display, a card that shared its group would take it
		// along anyway: the rest are judged again without it.
		if kept, keptWhy := isolated(keep); len(boot) > 0 && len(kept) > 0 {
			for _, bdf := range boot {
				why = append(why, fmt.Sprintf("GPU %s (%s) is this machine's boot display, its console; it stays with the host while another GPU can be rented",
					bdf, names[bdf]))
			}
			rentable = kept
			why = append(why, keptWhy...)
		}
	}
	return rentable, append(excluded, why...)
}

// NormalizeBDF turns nvidia-smi's 8-digit PCI domain ("00000000:0F:01.0") into
// the 4-digit form sysfs and VFIO use ("0000:0f:01.0").
func NormalizeBDF(s string) string { return pcidev.NormalizeBDF(s) }

// rentalTookGPUs reports whether the rental state on disk handed GPUs to its
// VM (an unreadable state counts as yes: fail closed).
func rentalTookGPUs(h vmrt.Host, dataDir string) bool {
	st, err := vmrt.LoadState(h, dataDir)
	return err != nil || (st != nil && len(st.Devices) > 0)
}
