package update

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// winService is the agent's Windows service, through the service manager.
type winService struct{}

func defaultService() ServiceControl { return winService{} }

func (winService) open() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(ServiceUnit)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	return m, s, nil
}

func (w winService) State() (bool, int, error) {
	m, s, err := w.open()
	if err != nil {
		return false, 0, err
	}
	defer m.Disconnect()
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return false, 0, err
	}
	return st.State == svc.Running, int(st.ProcessId), nil
}

func (w winService) Stop() error {
	m, s, err := w.open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
	}
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if st, err = s.Query(); err == nil && st.State == svc.Stopped {
			return nil
		}
	}
	return errors.New("the agent's service did not stop within a minute")
}

func (w winService) Start() error {
	m, s, err := w.open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return s.Start()
}

func (winService) Image(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// Spawn starts a process with no console, in a process group of its own and,
// where the service's job allows it, outside that job: it must survive the
// service stopping.
func (winService) Spawn(name string, args ...string) error {
	flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	for _, extra := range []uint32{windows.CREATE_BREAKAWAY_FROM_JOB, 0} {
		cmd := exec.Command(name, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags | extra}
		err := cmd.Start()
		if err == nil {
			return cmd.Process.Release()
		}
		if extra == 0 {
			return fmt.Errorf("start %s: %w", name, err)
		}
	}
	return nil
}
