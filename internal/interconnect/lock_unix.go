//go:build unix

package interconnect

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// TryLock takes the lock every raw-frame run holds, so the daemon's cable
// check and peer announcements and a pair test boot from the command line
// never bring the same ports up and down under each other. It is an flock on a
// file in the data directory, held per open file, so it excludes runs in the
// same process as well as in another. ok is false while another run holds it.
func TryLock(dataDir string) (unlock func(), ok bool, err error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "frames.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, true, nil
}
