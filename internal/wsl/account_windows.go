//go:build windows

package wsl

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/serverroom/gpu-marketplace/internal/config"
)

var (
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	userenv  = windows.NewLazySystemDLL("userenv.dll")
	netapi32 = windows.NewLazySystemDLL("netapi32.dll")

	procLogonUserW          = advapi32.NewProc("LogonUserW")
	procLsaOpenPolicy       = advapi32.NewProc("LsaOpenPolicy")
	procLsaAddAccountRights = advapi32.NewProc("LsaAddAccountRights")
	procLsaClose            = advapi32.NewProc("LsaClose")
	procLoadUserProfileW    = userenv.NewProc("LoadUserProfileW")
	procNetUserAdd          = netapi32.NewProc("NetUserAdd")
	procNetUserSetInfo      = netapi32.NewProc("NetUserSetInfo")
	procNetUserDel          = netapi32.NewProc("NetUserDel")
)

const (
	logon32LogonBatch      = 4
	logon32ProviderDefault = 0
	nerrUserExists         = 2224
	nerrUserNotFound       = 2221
	userPrivUser           = 1
	ufScript               = 0x0001
	ufPasswdCantChange     = 0x0040
	ufDontExpirePasswd     = 0x10000
	piNoUI                 = 1
	cryptprotectLocalMach  = 0x4
	policyCreateAccount    = 0x00000010
	policyLookupNames      = 0x00000800
)

type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

type userInfo1003 struct{ Password *uint16 }

type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	Profile     windows.Handle
}

type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               uintptr
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

// secretPath holds the account's password, sealed with DPAPI for this machine.
func secretPath() string { return filepath.Join(config.ConfigDir(), "wsl-account.dat") }

func seal(data []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, cryptprotectLocalMach|windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func unseal(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty secret")
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// newPassword is 32 random bytes, with one of each character class so any
// password policy a machine enforces accepts it.
func newPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b) + "aA1!", nil
}

func password() (string, error) {
	data, err := os.ReadFile(secretPath())
	if err != nil {
		return "", err
	}
	p, err := unseal(data)
	if err != nil {
		return "", fmt.Errorf("unseal the WSL account's password: %w", err)
	}
	return string(p), nil
}

// EnsureAccount creates the agent's Windows account (or gives an existing one
// a new password when the sealed one is gone): a standard user, hidden from
// the sign-in screen, allowed only to run as a batch job -- never to sign in
// at the console, over Remote Desktop or over the network.
func EnsureAccount() error {
	if _, err := password(); err == nil && accountExists() {
		return grantRights()
	}
	pw, err := newPassword()
	if err != nil {
		return err
	}
	name, _ := windows.UTF16PtrFromString(Account)
	pass, _ := windows.UTF16PtrFromString(pw)
	comment, _ := windows.UTF16PtrFromString("GPU marketplace agent: runs the WSL 2 environment rentals use")
	ui := userInfo1{Name: name, Password: pass, Priv: userPrivUser, Comment: comment, Flags: ufScript | ufPasswdCantChange | ufDontExpirePasswd}
	r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&ui)), 0)
	switch r {
	case 0:
	case nerrUserExists:
		info := userInfo1003{Password: pass}
		if r, _, _ := procNetUserSetInfo.Call(0, uintptr(unsafe.Pointer(name)), 1003, uintptr(unsafe.Pointer(&info)), 0); r != 0 {
			return fmt.Errorf("reset the password of the Windows account %s: error %d", Account, r)
		}
	default:
		return fmt.Errorf("create the Windows account %s: error %d (a policy of this machine may forbid local accounts)", Account, r)
	}
	sealed, err := seal([]byte(pw))
	if err != nil {
		return fmt.Errorf("seal the WSL account's password: %w", err)
	}
	if err := os.MkdirAll(config.ConfigDir(), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(secretPath(), sealed, 0600); err != nil {
		return err
	}
	resetSession()
	// Off the sign-in screen.
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList`, registry.SET_VALUE)
	if err == nil {
		_ = k.SetDWordValue(Account, 0)
		k.Close()
	}
	return grantRights()
}

func accountExists() bool {
	sid, _, _, err := windows.LookupSID("", Account)
	return err == nil && sid != nil
}

// grantRights lets the account run as a batch job (how the agent starts WSL
// under it) and denies it every way of signing in.
func grantRights() error {
	sid, _, _, err := windows.LookupSID("", Account)
	if err != nil {
		return fmt.Errorf("look up the Windows account %s: %w", Account, err)
	}
	var oa lsaObjectAttributes
	oa.Length = uint32(unsafe.Sizeof(oa))
	var policy windows.Handle
	if st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&oa)), policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy))); st != 0 {
		return fmt.Errorf("open the local security policy: NTSTATUS 0x%x", st)
	}
	defer procLsaClose.Call(uintptr(policy))
	for _, right := range []string{"SeBatchLogonRight", "SeDenyInteractiveLogonRight", "SeDenyRemoteInteractiveLogonRight", "SeDenyNetworkLogonRight"} {
		u, err := windows.NewNTUnicodeString(right)
		if err != nil {
			return err
		}
		if st, _, _ := procLsaAddAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(u)), 1); st != 0 {
			return fmt.Errorf("grant %s to %s: NTSTATUS 0x%x", right, Account, st)
		}
	}
	return nil
}

// RemoveAccount deletes the account, its sealed password and its sign-in
// screen entry (gpu-agent remove). Its profile folder goes with it.
func RemoveAccount() error {
	name, _ := windows.UTF16PtrFromString(Account)
	r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(name)))
	if r != 0 && r != nerrUserNotFound {
		return fmt.Errorf("delete the Windows account %s: error %d", Account, r)
	}
	_ = os.Remove(secretPath())
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList`, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(Account)
		k.Close()
	}
	resetSession()
	return nil
}

// session is the account signed on: its token, environment and profile.
type session struct {
	token   windows.Token
	env     []string
	home    string
	sid     string
	profile windows.Handle
}

var (
	sessMu sync.Mutex
	sess   *session
)

func resetSession() {
	sessMu.Lock()
	defer sessMu.Unlock()
	if sess != nil {
		sess.token.Close()
		sess = nil
	}
}

// openSession signs the account on as a batch job and loads its profile (its
// registry hive, where WSL records the distribution). Kept for the life of the
// agent.
func openSession() (*session, error) {
	sessMu.Lock()
	defer sessMu.Unlock()
	if sess != nil {
		return sess, nil
	}
	pw, err := password()
	if err != nil {
		return nil, err
	}
	user, _ := windows.UTF16PtrFromString(Account)
	domain, _ := windows.UTF16PtrFromString(".")
	pass, _ := windows.UTF16PtrFromString(pw)
	var tok windows.Token
	if r, _, err := procLogonUserW.Call(uintptr(unsafe.Pointer(user)), uintptr(unsafe.Pointer(domain)), uintptr(unsafe.Pointer(pass)),
		logon32LogonBatch, logon32ProviderDefault, uintptr(unsafe.Pointer(&tok))); r == 0 {
		return nil, fmt.Errorf("sign on the Windows account %s: %w", Account, err)
	}
	pi := profileInfo{Flags: piNoUI, UserName: user}
	pi.Size = uint32(unsafe.Sizeof(pi))
	if r, _, err := procLoadUserProfileW.Call(uintptr(tok), uintptr(unsafe.Pointer(&pi))); r == 0 {
		tok.Close()
		return nil, fmt.Errorf("load the profile of %s: %w", Account, err)
	}
	home, err := tok.GetUserProfileDirectory()
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("profile folder of %s: %w", Account, err)
	}
	env, err := tok.Environ(false)
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("environment of %s: %w", Account, err)
	}
	tu, err := tok.GetTokenUser()
	if err != nil {
		tok.Close()
		return nil, err
	}
	sess = &session{token: tok, env: env, home: home, sid: tu.User.Sid.String(), profile: pi.Profile}
	return sess, nil
}

// registered reports whether the account's WSL knows the distribution.
func (s *session) registered() (basePath string, ok bool) {
	k, err := registry.OpenKey(registry.USERS, s.sid+`\Software\Microsoft\Windows\CurrentVersion\Lxss`, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	for _, n := range names {
		sub, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		name, _, _ := sub.GetStringValue("DistributionName")
		base, _, _ := sub.GetStringValue("BasePath")
		sub.Close()
		if strings.EqualFold(name, Distro) {
			return base, true
		}
	}
	return "", false
}
