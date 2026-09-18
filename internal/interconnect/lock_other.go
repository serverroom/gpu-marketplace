//go:build !unix

package interconnect

import "sync"

var frameMu sync.Mutex

// TryLock excludes concurrent raw-frame runs in this process. There are none
// to exclude elsewhere: raw frames need Linux.
func TryLock(dataDir string) (unlock func(), ok bool, err error) {
	if !frameMu.TryLock() {
		return nil, false, nil
	}
	return frameMu.Unlock, true, nil
}
