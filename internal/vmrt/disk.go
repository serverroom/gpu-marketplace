package vmrt

import (
	"crypto/rand"
	"fmt"
	"path"
	"strings"
)

// DiskState is a rental disk's pieces, recorded as each one comes into being so
// a teardown after a crash removes exactly what exists.
type DiskState struct {
	File   string `json:"file"`
	Loop   string `json:"loop"`
	Mapper string `json:"mapper"`
}

// CreateDisk makes the rental's disk: a sparse file, a loop device over it, and
// a dm-crypt mapping over that with a random key that exists only in this
// process's memory for the length of one cryptsetup call and in the kernel for
// the life of the mapping. It is never written anywhere. Closing the mapping at
// teardown therefore destroys the only copy, and what is left on the physical
// disk is ciphertext nobody can read -- which is what makes the wipe a wipe.
// The base image is then written through the mapping.
func CreateDisk(h Host, spec Spec, dir, id string) (DiskState, error) {
	ds, err := openEncrypted(h, spec, dir, id)
	if err != nil {
		return ds, err
	}
	if err := h.Run("qemu-img", "convert", "-n", "-O", "raw", spec.GoldenImage, ds.Mapper); err != nil {
		return ds, fmt.Errorf("write base image: %w", err)
	}
	return ds, nil
}

// openEncrypted makes the encrypted block device a rental writes to: a sparse
// file, a loop device over it, and a dm-crypt mapping with a random key that
// exists only in memory for one cryptsetup call and in the kernel for the life
// of the mapping. Closing the mapping at teardown destroys the only copy, which
// is what makes the wipe a wipe. Shared by CreateDisk (which then writes the
// golden image through it) and createEncryptedVolume (which formats it).
//
// The mapping is made within a time limit, and without waiting for udev on a
// machine whose udev does not answer (dmudev.go).
func openEncrypted(h Host, spec Spec, dir, id string) (DiskState, error) {
	ds := DiskState{File: path.Join(dir, "disk.img")}
	if err := h.Run("truncate", "-s", mib(spec.DiskGB), ds.File); err != nil {
		return ds, fmt.Errorf("allocate disk: %w", err)
	}
	// Direct I/O: without it every block the rental reads or writes is cached
	// twice, once for the loop device and once for the file under it -- on a
	// GB10, whose GPU shares that memory, twice the renter's I/O comes out of
	// what the GPU can use. A filesystem that cannot do direct I/O gets the
	// loop device as before.
	loop, err := h.Output("losetup", "--direct-io=on", "--find", "--show", ds.File)
	if loop = strings.TrimSpace(loop); err != nil || !strings.HasPrefix(loop, "/dev/loop") {
		// losetup may have attached the file and failed only to switch direct
		// I/O on: use that device rather than attach a second one.
		loop, err = "", nil
		if held, jerr := h.Output("losetup", "-j", ds.File); jerr == nil {
			if dev, _, ok := strings.Cut(strings.TrimSpace(held), ":"); ok && strings.HasPrefix(dev, "/dev/loop") {
				loop, err = dev, nil
			}
		}
		if loop == "" {
			loop, err = h.Output("losetup", "--find", "--show", ds.File)
		}
	}
	if err != nil {
		return ds, fmt.Errorf("attach disk: %w", err)
	}
	if loop = strings.TrimSpace(loop); !strings.HasPrefix(loop, "/dev/loop") {
		return ds, fmt.Errorf("attach disk: losetup answered %q", loop)
	}
	ds.Loop = loop

	key := make([]byte, 64)
	if _, err := rand.Read(key); err != nil {
		return ds, fmt.Errorf("disk key: %w", err)
	}
	name := MapperName(id)
	// Recorded before the attempt: a command ended at its limit may have made
	// the mapping first, and the teardown has to look for it either way.
	ds.Mapper = "/dev/mapper/" + name
	err = mappingsOn(h, spec).open(key, ds.Loop, name)
	for i := range key {
		key[i] = 0
	}
	if err != nil {
		return ds, fmt.Errorf("encrypt disk: %w", err)
	}
	return ds, nil
}

// createEncryptedVolume makes a rental's writable space for container mode: the
// same dm-crypt device, formatted ext4 and mounted under the rental directory.
// Returns the disk pieces (for DestroyDisk) and the mount point. The caller
// unmounts it before DestroyDisk at teardown.
func createEncryptedVolume(h Host, spec Spec, dir, id string) (DiskState, string, error) {
	ds, err := openEncrypted(h, spec, dir, id)
	if err != nil {
		return ds, "", err
	}
	if err := h.Run("mkfs.ext4", "-q", "-m", "0", ds.Mapper); err != nil {
		return ds, "", fmt.Errorf("format volume: %w", err)
	}
	mount := volumeDir(dir)
	if err := h.MkdirAll(mount, 0700); err != nil {
		return ds, "", fmt.Errorf("volume mount point: %w", err)
	}
	if err := h.Run("mount", ds.Mapper, mount); err != nil {
		return ds, "", fmt.Errorf("mount volume: %w", err)
	}
	return ds, mount, nil
}

// volumeDir is where a container rental's volume is mounted.
func volumeDir(dir string) string { return path.Join(dir, "vol") }

// knownDisk is a rental's disk as its teardown looks for it: the pieces the
// state recorded and, for each it did not, the place that piece would be. The
// pieces are recorded when the disk has been made. An agent stopped while it
// was being made (or, before these commands had a time limit, stopped by a
// person because cryptsetup never returned) left a mapping and a loop device
// behind that no record names, and a teardown that goes by the record alone
// would call that disk wiped.
func knownDisk(h Host, ds DiskState, dir, id string) DiskState {
	if dir == "" || id == "" {
		return ds
	}
	if ds.Mapper == "" {
		ds.Mapper = "/dev/mapper/" + MapperName(id)
	}
	if ds.File == "" {
		ds.File = path.Join(dir, "disk.img")
	}
	if ds.Loop == "" {
		if held, err := h.Output("losetup", "-j", ds.File); err == nil {
			if dev, _, ok := strings.Cut(strings.TrimSpace(held), ":"); ok && strings.HasPrefix(dev, "/dev/loop") {
				ds.Loop = dev
			}
		}
	}
	return ds
}

// DestroyDisk closes the mapping (the key is gone), detaches the loop device
// and deletes the file, then checks that none of the three is still there.
//
// The mapping is closed within a time limit, and without waiting for udev
// where udev does not answer (dmudev.go). Whether it is gone is asked of the
// kernel: on such a machine its node in /dev/mapper says nothing either way.
func DestroyDisk(h Host, spec Spec, ds DiskState) (wiped bool, detail []string) {
	name := path.Base(ds.Mapper)
	if ds.Mapper != "" {
		if err := mappingsOn(h, spec).close(name); err != nil && mapped(h, name) {
			detail = append(detail, fmt.Sprintf("close %s: %v", ds.Mapper, err))
		}
	}
	if ds.Loop != "" {
		_ = h.Run("losetup", "-d", ds.Loop)
	}
	if ds.File != "" {
		if err := h.Remove(ds.File); err != nil {
			detail = append(detail, fmt.Sprintf("delete %s: %v", ds.File, err))
		}
	}
	wiped = true
	if ds.Mapper != "" && mapped(h, name) {
		wiped = false
		detail = append(detail, ds.Mapper+" is still open")
	}
	if ds.File != "" && h.Exists(ds.File) {
		wiped = false
		detail = append(detail, ds.File+" still exists")
	}
	if ds.File != "" {
		if out, err := h.Output("losetup", "-j", ds.File); err == nil && strings.TrimSpace(out) != "" {
			wiped = false
			detail = append(detail, "a loop device is still attached to "+ds.File)
		}
	}
	return wiped, detail
}
