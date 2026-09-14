package vmrt

import (
	"crypto/rand"
	"fmt"
	"path"
	"path/filepath"
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
func CreateDisk(h Host, dir, id string, sizeGB int, golden string) (DiskState, error) {
	ds := DiskState{File: filepath.Join(dir, "disk.img")}
	if err := h.Run("truncate", "-s", mib(sizeGB), ds.File); err != nil {
		return ds, fmt.Errorf("allocate disk: %w", err)
	}
	loop, err := h.Output("losetup", "--find", "--show", ds.File)
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
	err = h.RunInput(key, "cryptsetup", "open", "--type", "plain",
		"--cipher", "aes-xts-plain64", "--key-size", "512",
		"--key-file", "-", "--keyfile-size", "64", ds.Loop, name)
	for i := range key {
		key[i] = 0
	}
	if err != nil {
		return ds, fmt.Errorf("encrypt disk: %w", err)
	}
	ds.Mapper = "/dev/mapper/" + name

	if err := h.Run("qemu-img", "convert", "-n", "-O", "raw", golden, ds.Mapper); err != nil {
		return ds, fmt.Errorf("write base image: %w", err)
	}
	return ds, nil
}

// DestroyDisk closes the mapping (the key is gone), detaches the loop device
// and deletes the file, then checks that none of the three is still there.
func DestroyDisk(h Host, ds DiskState) (wiped bool, detail []string) {
	if ds.Mapper != "" {
		if err := h.Run("cryptsetup", "close", path.Base(ds.Mapper)); err != nil && h.Exists(ds.Mapper) {
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
	if ds.Mapper != "" && h.Exists(ds.Mapper) {
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
