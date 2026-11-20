package wsl

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// An administrator running the agent by hand (gpu-agent check, setup, status
// in an Administrator PowerShell) starts wsl.exe as the agent's account with
// CreateProcessWithTokenW. That new process inherits the caller's window
// station and desktop, and CreateProcessWithTokenW -- unlike
// CreateProcessWithLogonW -- does not give the account access to them: the
// caller must. Without it wsl.exe dies in DLL initialisation
// (0xC0000142, STATUS_DLL_INIT_FAILED), and on an interactive desktop Windows
// shows a modal "wsl.exe - Application Error" for every call, each one holding
// its wsl.exe until somebody clicks OK. Found on the first real Windows 11 host
// (SID 4434, 2026-11-20): every probe failed, so 'gpu-agent check' called a
// fully set-up machine not ready while the service, as LocalSystem on its own
// window station, saw it ready.
//
// grantDesktop gives the account the access it needs for as long as a wsl.exe
// of ours runs, and takes it away when the last one ends. The rights are what
// a console program needs to start, not the right to hook, record, switch or
// read the screen.

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procGetProcessWindowStation  = user32.NewProc("GetProcessWindowStation")
	procGetThreadDesktop         = user32.NewProc("GetThreadDesktop")
	procGetUserObjectInformation = user32.NewProc("GetUserObjectInformationW")
	procOpenWindowStation        = user32.NewProc("OpenWindowStationW")
	procOpenDesktop              = user32.NewProc("OpenDesktopW")
	procCloseWindowStation       = user32.NewProc("CloseWindowStation")
	procCloseDesktop             = user32.NewProc("CloseDesktop")
)

const (
	uoiName = 2

	winstaEnumDesktops      = 0x0001
	winstaReadAttributes    = 0x0002
	winstaAccessGlobalAtoms = 0x0020
	winstaExitWindows       = 0x0040
	winstaEnumerate         = 0x0100

	desktopReadObjects  = 0x0001
	desktopCreateWindow = 0x0002
	desktopCreateMenu   = 0x0004
	desktopEnumerate    = 0x0040
	desktopWriteObjects = 0x0080

	// The account's rights: enough for a console program to start, measured
	// on SID 4434 (Windows 11 build 26300): without WINSTA_EXITWINDOWS -- the
	// right a console program uses to register with csrss for shutdown
	// notices -- wsl.exe still fails with 0xC0000142; every other window
	// station right, and every desktop right beyond these, made no difference.
	winstaRights  = winstaEnumDesktops | winstaReadAttributes | winstaAccessGlobalAtoms | winstaExitWindows | winstaEnumerate | windows.READ_CONTROL
	desktopRights = desktopReadObjects | desktopCreateWindow | desktopCreateMenu | desktopEnumerate | desktopWriteObjects | windows.READ_CONTROL
)

var (
	grantMu    sync.Mutex
	grantCount int
	granted    []grantedObject
)

type grantedObject struct {
	handle windows.Handle
	close  *windows.LazyProc
}

// grantDesktop lets the account with SID sid use this process's window
// station and desktop until the returned release is called. Calls nest: the
// access goes when the last release runs.
func grantDesktop(sid string) (release func(), err error) {
	grantMu.Lock()
	defer grantMu.Unlock()
	if grantCount == 0 {
		account, err := windows.StringToSid(sid)
		if err != nil {
			return nil, err
		}
		objs, err := openOwnDesktop()
		if err != nil {
			return nil, err
		}
		for i, o := range objs {
			rights := windows.ACCESS_MASK(winstaRights)
			if i == 1 {
				rights = desktopRights
			}
			if err := setAccess(o.handle, account, rights, windows.GRANT_ACCESS); err != nil {
				for _, d := range objs[:i] {
					setAccess(d.handle, account, 0, windows.REVOKE_ACCESS)
				}
				closeAll(objs)
				return nil, fmt.Errorf("let %s use this desktop: %w", Account, err)
			}
		}
		granted = objs
	}
	grantCount++
	var once sync.Once
	return func() {
		once.Do(func() {
			grantMu.Lock()
			defer grantMu.Unlock()
			grantCount--
			if grantCount > 0 {
				return
			}
			if account, err := windows.StringToSid(sid); err == nil {
				for _, o := range granted {
					setAccess(o.handle, account, 0, windows.REVOKE_ACCESS)
				}
			}
			closeAll(granted)
			granted = nil
		})
	}, nil
}

// openOwnDesktop opens this process's window station and its thread's
// desktop by name, with the right to change who may use them (the handles
// GetProcessWindowStation/GetThreadDesktop return need not carry it).
func openOwnDesktop() ([]grantedObject, error) {
	ws, _, err := procGetProcessWindowStation.Call()
	if ws == 0 {
		return nil, fmt.Errorf("GetProcessWindowStation: %w", err)
	}
	dt, _, err := procGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	if dt == 0 {
		return nil, fmt.Errorf("GetThreadDesktop: %w", err)
	}
	wsName, err := objectName(windows.Handle(ws))
	if err != nil {
		return nil, err
	}
	dtName, err := objectName(windows.Handle(dt))
	if err != nil {
		return nil, err
	}
	const access = windows.READ_CONTROL | windows.WRITE_DAC
	name, _ := windows.UTF16PtrFromString(wsName)
	hws, _, err := procOpenWindowStation.Call(uintptr(unsafe.Pointer(name)), 0, access)
	if hws == 0 {
		return nil, fmt.Errorf("open window station %s: %w", wsName, err)
	}
	name, _ = windows.UTF16PtrFromString(dtName)
	hdt, _, err := procOpenDesktop.Call(uintptr(unsafe.Pointer(name)), 0, 0, access)
	if hdt == 0 {
		procCloseWindowStation.Call(hws)
		return nil, fmt.Errorf("open desktop %s: %w", dtName, err)
	}
	return []grantedObject{{windows.Handle(hws), procCloseWindowStation}, {windows.Handle(hdt), procCloseDesktop}}, nil
}

func objectName(h windows.Handle) (string, error) {
	buf := make([]uint16, 256)
	var need uint32
	r, _, err := procGetUserObjectInformation.Call(uintptr(h), uoiName, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&need)))
	if r == 0 {
		return "", fmt.Errorf("GetUserObjectInformation: %w", err)
	}
	return windows.UTF16ToString(buf), nil
}

// setAccess grants (GRANT_ACCESS) or removes (REVOKE_ACCESS) sid's entry in
// the object's DACL, leaving every other entry as it was.
func setAccess(h windows.Handle, sid *windows.SID, rights windows.ACCESS_MASK, mode windows.ACCESS_MODE) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_WINDOW_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: rights,
		AccessMode:        mode,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, dacl)
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(h, windows.SE_WINDOW_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func closeAll(objs []grantedObject) {
	for _, o := range objs {
		o.close.Call(uintptr(o.handle))
	}
}
