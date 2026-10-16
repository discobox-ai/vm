//go:build darwin && cgo

package vz

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <Security/Security.h>

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
