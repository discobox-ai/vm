//go:build darwin && cgo

package vz

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation -framework CoreGraphics
#include <stdlib.h>
#include <Security/Security.h>
#include <CoreGraphics/CoreGraphics.h>

// hasEntitlement is 1 when this process is signed with a true boolean
// entitlement, 0 when it is not, and -1 when it cannot tell.
static int hasEntitlement(const char *name) {
	SecTaskRef task = SecTaskCreateFromSelf(NULL);
	if (task == NULL) {
		return -1;
	}
	CFStringRef key = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
	CFTypeRef value = SecTaskCopyValueForEntitlement(task, key, NULL);
	CFRelease(key);
	CFRelease(task);
	if (value == NULL) {
		return 0;
	}
	int ok = CFGetTypeID(value) == CFBooleanGetTypeID() && CFBooleanGetValue((CFBooleanRef)value);
	CFRelease(value);
	return ok;
}

// screenLocked is 1 when the console session's screen is locked, 0 when it is
// not, and -1 when this process has no console session to ask.
static int screenLocked(void) {
	CFDictionaryRef session = CGSessionCopyCurrentDictionary();
	if (session == NULL) {
		return -1;
	}
	CFBooleanRef locked = CFDictionaryGetValue(session, CFSTR("CGSSessionScreenIsLocked"));
	int result = locked != NULL && CFBooleanGetValue(locked);
	CFRelease(session);
	return result;
}
*/
import "C"

import "unsafe"

const virtualizationEntitlement = "com.apple.security.virtualization"

// entitled reports whether this binary may create VMs. Without the entitlement
// the framework fails at its boundary with an opaque internal error, so Check
// asks up front.
func entitled() bool {
	name := C.CString(virtualizationEntitlement)
	defer C.free(unsafe.Pointer(name))
	return C.hasEntitlement(name) == 1
}

// screenLocked reports whether the Mac's screen is locked. A restore needs it
// unlocked: the key that protects saved state sits behind the login session,
// so a restore on a locked Mac fails with "permission denied" while a save
// still works. A process with no console session is not called locked; its
// restore falls back to a cold boot instead.
func screenLocked() bool { return C.screenLocked() == 1 }
