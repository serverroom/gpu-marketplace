//go:build darwin

package mac

import (
	"path/filepath"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

// vmDir holds the agent's VM: its overlay disk, the base image, the seed, the
// UEFI store and the agent's key to it. Under the agent's data directory.
func vmDir() string { return filepath.Join(config.DataDir(), VMName) }

func (p vmPaths) diskPath() string     { return filepath.Join(p.dir, "disk.qcow2") }
func (p vmPaths) basePath() string     { return filepath.Join(p.dir, "base.img") }
func (p vmPaths) seedPath() string     { return filepath.Join(p.dir, "seed.img") }
func (p vmPaths) firmwarePath() string { return filepath.Join(p.dir, "uefi.fd") }
func (p vmPaths) varsPath() string     { return filepath.Join(p.dir, "uefi-vars.fd") }
func (p vmPaths) serialPath() string   { return filepath.Join(p.dir, "serial.log") }
func (p vmPaths) pidPath() string      { return filepath.Join(p.dir, "qemu.pid") }
func (p vmPaths) keyPath() string      { return filepath.Join(p.dir, "id_ed25519") }
func (p vmPaths) knownHosts() string   { return filepath.Join(p.dir, "known_hosts") }

type vmPaths struct{ dir string }

func paths() vmPaths { return vmPaths{dir: vmDir()} }
