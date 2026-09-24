package vmrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/serverroom/gpu-marketplace/internal/pcidev"
)

// The base image every rental starts from: Ubuntu 26.04 LTS's official cloud
// image, verified against Canonical's published SHA256SUMS, with what this
// machine's GPUs need baked in once (the NVIDIA driver, or the firmware AMD and
// Intel GPUs load) so a rental boots straight to a working GPU.
var (
	UbuntuRelease = "resolute"
	BakeTimeout   = 45 * time.Minute
	// DefaultDriver is an open-kernel-module server branch: required for
	// Blackwell (GB10), and supported on Turing and later.
	DefaultDriver = "580-server-open"
)

// NoDriver is the driver of a base image baked without the NVIDIA driver: the
// image of a machine that has no NVIDIA GPU.
const NoDriver = "none"

func cloudImageName(arch string) string {
	return UbuntuRelease + "-server-cloudimg-" + arch + ".img"
}

func cloudImageBase() string {
	return "https://cloud-images.ubuntu.com/" + UbuntuRelease + "/current/"
}

// GoldenInfo describes the baked image, next to it on disk.
type GoldenInfo struct {
	Base         string `json:"base"`
	BaseSHA256   string `json:"base_sha256"`
	Driver       string `json:"driver"`
	AgentVersion string `json:"agent_version"`
	CreatedAt    int64  `json:"created_at"`
	// DriverSource is how Driver was chosen: DriverFromHost, DriverDefault or
	// DriverFromFlag. Absent from images built by agents up to v0.1.9.
	DriverSource string `json:"driver_source,omitempty"`
	// HostDriver is the host's own NVIDIA driver when the image was built.
	HostDriver string `json:"host_driver,omitempty"`
	// Extras are optional additions baked in besides the driver: "rdma" (the
	// RDMA userspace tools and the mlx5_ib module a linked pair needs).
	Extras []string `json:"extras,omitempty"`
	// Vendors are the GPU makes (PCI vendor IDs) the image was baked for.
	// Absent from images built by agents up to v0.2.0.
	Vendors []string `json:"vendors,omitempty"`
}

// vendors are the makes an image serves. Agents up to v0.2.0 baked the NVIDIA
// driver or nothing, and recorded no makes; such an image serves NVIDIA when it
// has the driver, so an upgraded NVIDIA machine does not rebuild for nothing.
func (g GoldenInfo) vendors() []string {
	if len(g.Vendors) == 0 && g.Driver != "" && g.Driver != NoDriver {
		return []string{pcidev.NVIDIA}
	}
	return g.Vendors
}

// machineVendors are the makes of the machine's rentable GPUs, sorted --
// including GPUs the host is using now, so the image is already right on the
// day they are free to rent. A processor's integrated GPU is never rented, so
// it asks nothing of the image.
func machineVendors(h Host) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range pcidev.Display(h) {
		if !pcidev.Integrated(h, d) && !seen[d.Vendor] {
			seen[d.Vendor] = true
			out = append(out, d.Vendor)
		}
	}
	sort.Strings(out)
	return out
}

// LoadGoldenInfo reads the record next to the baked image.
func LoadGoldenInfo(h Host, spec Spec) (GoldenInfo, error) {
	var info GoldenInfo
	data, err := h.ReadFile(spec.GoldenImage + ".json")
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(data, &info)
	return info, err
}

// ExtraRDMA marks a base image with the RDMA tools baked in.
const ExtraRDMA = "rdma"

// GoldenHasExtra reports whether the baked image on disk records extra. Only a
// linked pair needs any; GoldenProblem, which every rental checks, does not
// look at extras, so a single machine never has to rebuild for them.
func GoldenHasExtra(h Host, spec Spec, extra string) bool {
	info, err := LoadGoldenInfo(h, spec)
	if err != nil {
		return false
	}
	for _, e := range info.Extras {
		if e == extra {
			return true
		}
	}
	return false
}

// GoldenDriver is the NVIDIA driver the baked image on disk records: a branch,
// NoDriver, or "" when nothing is recorded.
func GoldenDriver(h Host, spec Spec) string {
	data, err := h.ReadFile(spec.GoldenImage + ".json")
	var info GoldenInfo
	if err != nil || json.Unmarshal(data, &info) != nil {
		return ""
	}
	return info.Driver
}

// GoldenProblem says why the baked image on disk is not the one this agent
// builds, or "" when it is. A golden image outlives an agent upgrade, so without
// this a machine upgraded to a new release would keep renting out the old one --
// and one that gained a GPU of another make would rent it out with nothing for
// it in the image.
func GoldenProblem(h Host, spec Spec) string {
	const rebuild = "; rebuild it with 'sudo gpu-agent runtime prepare'"
	want := cloudImageName(spec.Arch)
	info, err := LoadGoldenInfo(h, spec)
	if err != nil || info.Base == "" {
		return "the rental base image does not say which Ubuntu image it was built from" + rebuild
	}
	if info.Base != want {
		return "the rental base image was built from " + info.Base + ", and this agent rents out " + want + rebuild
	}
	has := map[string]bool{}
	for _, v := range info.vendors() {
		has[v] = true
	}
	var missing []string
	for _, gpu := range spec.GPUs {
		v := pcidev.Read(h, gpu).Vendor
		// NVIDIA's driver is judged by GoldenDriver and GoldenDriverProblem;
		// here only what the other makes need (their firmware).
		if len(GPUPackages(NoDriver, []string{v})) > 0 && !has[v] {
			has[v] = true
			missing = append(missing, pcidev.VendorName(v))
		}
	}
	if len(missing) > 0 {
		return "the rental base image was not built for this machine's " + strings.Join(missing, " and ") + " GPU" + rebuild
	}
	return ""
}

// GoldenDriverProblem says why the baked image's NVIDIA driver is not the one
// this host's driver calls for, or "" when it is -- or when a person chose the
// image's driver (--driver), or the host's driver cannot be read.
func GoldenDriverProblem(h Host, spec Spec, want DriverChoice) string {
	info, err := LoadGoldenInfo(h, spec)
	if err != nil || info.DriverSource == DriverFromFlag || want.Source != DriverFromHost || info.Driver == want.Driver {
		return ""
	}
	have := info.Driver
	if have == "" {
		have = "an unrecorded driver"
	}
	return fmt.Sprintf("the rental base image has NVIDIA driver %s, and this machine runs %s (%s); rebuild it with 'sudo gpu-agent runtime prepare'",
		have, want.Driver, want.Describe())
}

// PrepareOptions controls `gpu-agent runtime prepare`.
type PrepareOptions struct {
	// Driver is the NVIDIA driver branch to bake in; "" matches the host's
	// (ChooseDriver).
	Driver string
	// DriverSource records how Driver was chosen; DriverFromFlag when it is
	// set and this is empty.
	DriverSource string
	InstallDeps  bool
	Log          func(format string, args ...interface{})
	// Ctx, when set, stops the build early: the bake VM is torn down and
	// Prepare returns the context's error.
	Ctx context.Context
}

// InstallPackages installs the runtime's packages (Packages) with apt-get.
func InstallPackages(h Host, arch string, log func(format string, args ...interface{})) error {
	if log == nil {
		log = func(string, ...interface{}) {}
	}
	log("Installing %s ...", strings.Join(Packages(arch), " "))
	if err := h.Run("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "update"); err != nil {
		return fmt.Errorf("apt-get update: %w", err)
	}
	args := append([]string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y"}, Packages(arch)...)
	if err := h.Run("env", args...); err != nil {
		return fmt.Errorf("install packages: %w", err)
	}
	return nil
}

// sumFor finds a file's hash in a SHA256SUMS listing ("<hash> *<name>").
func sumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// Prepare installs what the runtime needs and bakes the golden image: download
// and verify the cloud image, boot it once without any GPU behind the same
// fence and NAT a rental gets, let cloud-init install the driver and power the
// VM off, and flatten the result into the golden image.
func Prepare(h Host, spec Spec, fence Fence, version string, o PrepareOptions) error {
	log := o.Log
	if log == nil {
		log = func(string, ...interface{}) {}
	}
	ctx := o.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	host := ChooseImageDriver(h)
	switch {
	case o.Driver == "":
		o.Driver, o.DriverSource = host.Driver, host.Source
		switch host.Source {
		case DriverFromHost:
			log("This machine runs NVIDIA driver %s; the rental image gets %s to match.", host.Describe(), o.Driver)
		case DriverNoGPU:
			log("This machine has no NVIDIA GPU; the rental image gets no NVIDIA driver.")
		default:
			log("This machine's NVIDIA driver could not be read; the rental image gets %s.", o.Driver)
		}
	case o.DriverSource == "":
		o.DriverSource = DriverFromFlag
	}
	vendors := machineVendors(h)
	userData, err := BakeUserData(o.Driver, vendors)
	if err != nil {
		return err
	}

	if o.InstallDeps {
		if err := InstallPackages(h, spec.Arch, log); err != nil {
			return err
		}
	}
	if missing := MissingTools(h, spec.Arch); len(missing) > 0 {
		return fmt.Errorf("missing %s; install them with: %s (or rerun with --install-deps)",
			strings.Join(missing, ", "), InstallHint(spec.Arch))
	}
	fw, ok := FindFirmware(h, spec.Arch)
	if !ok {
		return fmt.Errorf("no UEFI firmware for %s; install it with: %s", spec.Arch, InstallHint(spec.Arch))
	}
	spec.Firmware = fw
	if st, err := LoadState(h, spec.DataDir); err != nil || st != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w (%s)", ErrRentalPresent, st.RentalID)
	}

	dir := path.Join(spec.Storage(), "images")
	if err := h.MkdirAll(dir, 0700); err != nil {
		return err
	}
	name := cloudImageName(spec.Arch)
	base := path.Join(dir, name)
	sumsPath := path.Join(dir, "SHA256SUMS")
	log("Fetching %sSHA256SUMS ...", cloudImageBase())
	if err := h.Download(cloudImageBase()+"SHA256SUMS", sumsPath); err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	sums, err := h.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	want := sumFor(string(sums), name)
	if want == "" {
		return fmt.Errorf("SHA256SUMS lists no %s", name)
	}
	if got, err := h.SHA256(base); err != nil || got != want {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("the base image build was stopped: %w", err)
		}
		log("Downloading %s%s ...", cloudImageBase(), name)
		if err := h.Download(cloudImageBase()+name, base); err != nil {
			return fmt.Errorf("download base image: %w", err)
		}
	}
	got, err := h.SHA256(base)
	if err != nil || got != want {
		_ = h.Remove(base)
		return fmt.Errorf("the base image does not match Canonical's published checksum; refusing it")
	}
	log("Base image verified (sha256 %s).", want)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("the base image build was stopped: %w", err)
	}

	bake := path.Join(dir, "bake.qcow2")
	_ = h.Remove(bake)
	if err := h.Run("qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, bake, "20G"); err != nil {
		return fmt.Errorf("create bake disk: %w", err)
	}
	defer h.Remove(bake)

	r := NewRental(spec.Storage(), BakeID)
	r.Disk = bake
	r.DiskFormat = "qcow2"
	r.MemoryMB = 4096
	if m := spec.GuestMemoryMB(); m > 0 && m < r.MemoryMB {
		r.MemoryMB = m
	}
	rt := New(h, spec, fence, nil)
	st := &State{RentalID: BakeID, Rental: r, StartedAt: time.Now().Unix()}
	save := func() error { return SaveState(h, spec.DataDir, st) }
	if err := save(); err != nil {
		return err
	}
	stopped := false
	defer func() {
		if !stopped {
			rt.Stop()
		}
	}()

	if err := h.MkdirAll(r.Dir, 0700); err != nil {
		return err
	}
	if err := fence.Apply(); err != nil {
		return fmt.Errorf("isolate network: %w", err)
	}
	st.Fenced = true
	_ = save()
	st.Net, err = SetupNetwork(h)
	_ = save()
	if err != nil {
		return fmt.Errorf("bake network: %w", err)
	}
	if err := rt.writeSeed(r, userData, MetaData(BakeID), NetworkConfig()); err != nil {
		return err
	}
	if err := rt.copyVars(r); err != nil {
		return err
	}

	if pkgs := GPUPackages(o.Driver, vendors); len(pkgs) > 0 {
		log("Booting the base image to install %s and the RDMA tools (this takes a while) ...", strings.Join(pkgs, " "))
	} else {
		log("Booting the base image to install the RDMA tools; this machine's GPUs need nothing beyond its kernel (this takes a while) ...")
	}
	if err := h.Run("systemd-run", LaunchArgs(spec, r)...); err != nil {
		return fmt.Errorf("boot bake VM: %w", err)
	}
	finished := false
	for waited := time.Duration(0); waited < BakeTimeout && ctx.Err() == nil; waited += pollInterval {
		if !rt.alive(r) {
			finished = true
			break
		}
		h.Sleep(pollInterval)
	}
	res := rt.Stop()
	stopped = true
	if !finished && ctx.Err() != nil {
		return fmt.Errorf("the base image build was stopped before it finished: %w", ctx.Err())
	}
	if !finished {
		return fmt.Errorf("the bake VM did not finish within %v", BakeTimeout)
	}
	if !res.Clean() {
		return fmt.Errorf("the bake VM did not tear down cleanly: %s", strings.Join(res.Detail, "; "))
	}
	serial, _ := h.ReadFile(lastSerialLog(spec.DataDir))
	if !strings.Contains(string(serial), markBake+" DONE") {
		return errors.New("the GPU package install inside the base image did not succeed; see " + lastSerialLog(spec.DataDir))
	}

	log("Flattening into %s ...", spec.GoldenImage)
	tmp := spec.GoldenImage + ".tmp"
	if err := h.Run("qemu-img", "convert", "-O", "qcow2", bake, tmp); err != nil {
		return fmt.Errorf("write golden image: %w", err)
	}
	if err := h.Rename(tmp, spec.GoldenImage); err != nil {
		return err
	}
	// Every bake installs the RDMA tools (cheap), so a machine never has to
	// rebuild to become half of a pair -- recorded only when that step
	// succeeded, since a single rental does not need them.
	var extras []string
	if strings.Contains(string(serial), markBake+" EXTRA "+ExtraRDMA) {
		extras = append(extras, ExtraRDMA)
	} else {
		log("The RDMA tools could not be installed in the base image; single rentals are unaffected, but this machine cannot be half of a linked pair until a rebuild installs them.")
	}
	info, _ := json.MarshalIndent(GoldenInfo{Base: name, BaseSHA256: want, Driver: o.Driver,
		AgentVersion: version, CreatedAt: time.Now().Unix(), DriverSource: o.DriverSource,
		HostDriver: host.Describe(), Extras: extras, Vendors: vendors}, "", "  ")
	_ = h.WriteFile(spec.GoldenImage+".json", info, 0600)
	log("Golden image ready. Next: sudo gpu-agent check --boot")
	return nil
}
