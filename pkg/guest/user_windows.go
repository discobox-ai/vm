package guest

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi32  = windows.NewLazySystemDLL("advapi32.dll")
	userenv   = windows.NewLazySystemDLL("userenv.dll")
	user32    = windows.NewLazySystemDLL("user32.dll")
	logonUser = advapi32.NewProc("LogonUserW")

	loadUserProfile   = userenv.NewProc("LoadUserProfileW")
	unloadUserProfile = userenv.NewProc("UnloadUserProfile")

	getProcessWindowStation = user32.NewProc("GetProcessWindowStation")
	openDesktop             = user32.NewProc("OpenDesktopW")
	closeDesktop            = user32.NewProc("CloseDesktop")
)

// runAs makes cmd run as the named local account, elevated when the account
// is an administrator, with its profile loaded and its own environment.
//
// The agent runs as SYSTEM, which may log any account on. Accounts the image
// made (the install's user) have a blank password, which Install allows for
// LogonUser by turning LimitBlankPasswordUse off. The process runs in the
// agent's session, on the agent's window station and desktop, which the
// account is granted: without that, anything that loads user32 fails to start
// (0xC0000142).
func runAs(cmd *exec.Cmd, name string) ([]string, func(), error) {
	namep, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil, err
	}
	dot, _ := windows.UTF16PtrFromString(".")
	empty, _ := windows.UTF16PtrFromString("")
	const logonInteractive, providerDefault = 2, 0
	var token windows.Token
	if r, _, e := logonUser.Call(uintptr(unsafe.Pointer(namep)), uintptr(unsafe.Pointer(dot)), uintptr(unsafe.Pointer(empty)),
		logonInteractive, providerDefault, uintptr(unsafe.Pointer(&token))); r == 0 {
		return nil, nil, fmt.Errorf("guest: log on as %q (accounts the image made have a blank password): %w", name, e)
	}
	// UAC hands an administrator's interactive logon a filtered token; the
	// full one is linked to it, and steps that install software need it.
	if linked, err := token.GetLinkedToken(); err == nil {
		var primary windows.Token
		if windows.DuplicateTokenEx(linked, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary) == nil {
			token.Close()
			token = primary
		}
		linked.Close()
	}
	fail := func(err error) ([]string, func(), error) {
		token.Close()
		return nil, nil, err
	}
	if err := grantDesktop(token); err != nil {
		return fail(fmt.Errorf("guest: let %q use the agent's desktop: %w", name, err))
	}

	// PROFILEINFOW: the profile makes HKCU the account's own hive.
	profile := struct {
		size        uint32
		flags       uint32
		userName    *uint16
		profilePath *uint16
		defaultPath *uint16
		serverName  *uint16
		policyPath  *uint16
		profile     windows.Handle
	}{flags: 1, userName: namep} // PI_NOUI
	profile.size = uint32(unsafe.Sizeof(profile))
	if r, _, e := loadUserProfile.Call(uintptr(token), uintptr(unsafe.Pointer(&profile))); r == 0 {
		return fail(fmt.Errorf("guest: load the profile of %q: %w", name, e))
	}
	var block *uint16
	if err := windows.CreateEnvironmentBlock(&block, token, false); err != nil {
		unloadUserProfile.Call(uintptr(token), uintptr(profile.profile))
		return fail(err)
	}
	env := parseEnvironmentBlock(block)
	_ = windows.DestroyEnvironmentBlock(block)

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(token)
	release := func() {
		unloadUserProfile.Call(uintptr(token), uintptr(profile.profile))
		token.Close()
	}
	return env, release, nil
}

var desktopGrants sync.Map // user SID string → struct{}

// grantDesktop adds an ACE for the token's user to the agent's window station
// and desktop, once per user.
func grantDesktop(token windows.Token) error {
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sid := user.User.Sid
	if _, done := desktopGrants.Load(sid.String()); done {
		return nil
	}
	winsta, _, e := getProcessWindowStation.Call()
	if winsta == 0 {
		return fmt.Errorf("GetProcessWindowStation: %w", e)
	}
	if err := grantObject(windows.Handle(winsta), sid); err != nil {
		return err
	}
	name, _ := windows.UTF16PtrFromString("Default")
	const readControl, writeDAC = 0x00020000, 0x00040000
	desktop, _, e := openDesktop.Call(uintptr(unsafe.Pointer(name)), 0, 0, readControl|writeDAC)
	if desktop == 0 {
		return fmt.Errorf("OpenDesktop: %w", e)
	}
	defer closeDesktop.Call(desktop)
	if err := grantObject(windows.Handle(desktop), sid); err != nil {
		return err
	}
	desktopGrants.Store(sid.String(), struct{}{})
	return nil
}

func grantObject(handle windows.Handle, sid *windows.SID) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_WINDOW_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, dacl)
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_WINDOW_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
