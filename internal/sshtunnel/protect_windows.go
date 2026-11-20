package sshtunnel

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// keySDDL: owned by Administrators; full control for SYSTEM and Administrators
// only; protected, so nothing is inherited from the folder.
const keySDDL = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"

// ProtectKey gives the tunnel's private key the permissions Windows' OpenSSH
// insists on, or ssh refuses it ("UNPROTECTED PRIVATE KEY FILE ... bad
// permissions") and the tunnel never comes up.
//
// ssh-keygen makes the key owned by, and writable for, whoever ran it: the
// administrator who typed 'gpu-agent register'. The service runs ssh as
// SYSTEM, for which that administrator is somebody else. The whole DACL is
// replaced (icacls /grant:r would leave that administrator's own entry).
func ProtectKey(path string) error {
	sd, err := windows.SecurityDescriptorFromString(keySDDL)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("set the permissions of %s: %w", path, err)
	}
	return nil
}
