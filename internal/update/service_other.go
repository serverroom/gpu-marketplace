//go:build !windows

package update

import "errors"

// noService stands in for the Windows service manager elsewhere: nothing on
// Linux asks for it (systemd does that job).
type noService struct{}

func defaultService() ServiceControl { return noService{} }

var errNotWindows = errors.New("the Windows service manager is only on Windows")

func (noService) State() (bool, int, error)     { return false, 0, errNotWindows }
func (noService) Stop() error                   { return errNotWindows }
func (noService) Start() error                  { return errNotWindows }
func (noService) Image(int) (string, error)     { return "", errNotWindows }
func (noService) Spawn(string, ...string) error { return errNotWindows }
