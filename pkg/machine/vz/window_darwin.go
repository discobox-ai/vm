//go:build darwin && cgo

package vz

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Virtualization
#include <stdlib.h>
#import <AppKit/AppKit.h>
#import <Virtualization/Virtualization.h>

// The app never quits on its own account: its windows only look at VMs that a
// build or a shim owns, and quitting would take every one of them down.
@interface DiscoVMAppDelegate : NSObject <NSApplicationDelegate>
@end

@implementation DiscoVMAppDelegate
- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)sender {
	return NSTerminateCancel;
}
- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
	return NO;
}
@end

static DiscoVMAppDelegate *discoDelegate;

// discoAppStart makes this process a regular app, with a Dock icon and windows
// that can take focus. It runs on the main thread.
static void discoAppStart(void) {
	[NSApplication sharedApplication];
	discoDelegate = [DiscoVMAppDelegate new];
	NSApp.delegate = discoDelegate;
	[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
}

// discoAppRun runs the event loop until discoAppStop.
static void discoAppRun(void) {
	[NSApp run];
}

static void discoAppStop(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[NSApp stop:nil];
		// stop takes effect after the next event, so post one.
		NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
		                                   location:NSZeroPoint
		                              modifierFlags:0
		                                  timestamp:0
		                               windowNumber:0
		                                    context:nil
		                                    subtype:0
		                                      data1:0
		                                      data2:0];
		[NSApp postEvent:wake atStart:YES];
	});
}

// discoOpenWindow shows a VM's display in a new window and returns the window,
// retained, or NULL when machine is not a VZVirtualMachine.
static void *discoOpenWindow(void *machine, const char *title) {
	id object = (__bridge id)machine;
	if (![object isKindOfClass:[VZVirtualMachine class]]) {
		return NULL;
	}
	VZVirtualMachine *vm = (VZVirtualMachine *)object;
	NSString *name = [NSString stringWithUTF8String:title];
	__block void *handle = NULL;
	dispatch_sync(dispatch_get_main_queue(), ^{
		static NSPoint cascade;
		VZVirtualMachineView *view = [[VZVirtualMachineView alloc] init];
		view.virtualMachine = vm;
		view.capturesSystemKeys = YES;
		if (@available(macOS 14.0, *)) {
			// The guest's resolution follows the window.
			view.automaticallyReconfiguresDisplay = YES;
		}
		NSWindow *window = [[NSWindow alloc]
			initWithContentRect:NSMakeRect(0, 0, 1440, 900)
			          styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
			                    NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
			            backing:NSBackingStoreBuffered
			              defer:NO];
		window.releasedWhenClosed = NO;
		window.title = name;
		window.contentView = view;
		window.contentMinSize = NSMakeSize(640, 400);
		[window center];
		cascade = [window cascadeTopLeftFromPoint:cascade];
		[window makeKeyAndOrderFront:nil];
		[window makeFirstResponder:view];
		[NSApp activateIgnoringOtherApps:YES];
		handle = (__bridge_retained void *)window;
	});
	return handle;
}

// discoCloseWindow closes a window from discoOpenWindow and lets it go, and
// with it the view's hold on the VM.
static void discoCloseWindow(void *handle) {
	dispatch_async(dispatch_get_main_queue(), ^{
		NSWindow *window = (__bridge_transfer NSWindow *)handle;
		window.contentView = nil;
		[window close];
	});
}
*/
import "C"

import (
	"errors"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/Code-Hex/vz/v3"

	"github.com/discobox-ai/vm/pkg/machine"
)

// The process's main thread idles in mainLoop until the first window is asked
// for, and only then becomes an AppKit app. A command that opens no window, and
// the guest agent, which has no window server at all, never touch AppKit.
var app struct {
	running atomic.Bool
	start   chan struct{}
	ready   chan struct{}
	once    sync.Once
}

func init() {
	app.start = make(chan struct{})
	app.ready = make(chan struct{})
	machine.SetMainLoop(mainLoop)
}

func mainLoop(fn func() int) int {
	app.running.Store(true)
	done := make(chan int, 1)
	go func() { done <- fn() }()
	select {
	case code := <-done:
		return code
	case <-app.start:
	}
	C.discoAppStart()
	close(app.ready)
	exit := make(chan int, 1)
	go func() {
		exit <- <-done
		C.discoAppStop()
	}()
	C.discoAppRun()
	return <-exit
}

// startApp brings AppKit up on the main thread, once.
func startApp() error {
	if !app.running.Load() {
		return errors.New("vz: a window needs the process's main thread, and this program did not run through machine.Main")
	}
	app.once.Do(func() {
		app.start <- struct{}{}
		<-app.ready
	})
	return nil
}

// openWindow shows a VM's display in a native window and returns what closes
// it. Closing the window by hand leaves the VM running.
func openWindow(vm *vz.VirtualMachine, title string) (func(), error) {
	if err := startApp(); err != nil {
		return nil, err
	}
	object := vmObject(vm)
	if object == nil {
		return nil, errors.New("vz: cannot find the framework's VM object behind the bindings' VirtualMachine")
	}
	ctitle := C.CString(title)
	defer C.free(unsafe.Pointer(ctitle))
	handle := C.discoOpenWindow(object, ctitle)
	runtime.KeepAlive(vm)
	if handle == nil {
		return nil, errors.New("vz: the bindings' VirtualMachine does not hold a VZVirtualMachine")
	}
	var once sync.Once
	return func() { once.Do(func() { C.discoCloseWindow(handle) }) }, nil
}

// vmObject is the VZVirtualMachine a vz.VirtualMachine wraps. The bindings keep
// it unexported (an embedded *objc.Pointer whose only field is the pointer), so
// it is read by reflection; if their layout changes this returns nil, or a
// pointer discoOpenWindow refuses because it is not a VZVirtualMachine. The
// clean fix is an exported accessor in discobox's fork.
func vmObject(vm *vz.VirtualMachine) unsafe.Pointer {
	field := reflect.ValueOf(vm).Elem().FieldByName("pointer")
	if !field.IsValid() || field.Kind() != reflect.Pointer {
		return nil
	}
	ptr := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	if ptr.IsNil() || ptr.Elem().Kind() != reflect.Struct || ptr.Elem().NumField() != 1 {
		return nil
	}
	inner := ptr.Elem().Field(0)
	if inner.Kind() != reflect.UnsafePointer {
		return nil
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(inner.UnsafeAddr()))
}
