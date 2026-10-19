//go:build darwin

package main

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa -framework UserNotifications -framework UniformTypeIdentifiers
#import <Cocoa/Cocoa.h>
#import <UserNotifications/UserNotifications.h>

static void omSetBadge(const char *label) {
	NSString *s = label ? [NSString stringWithUTF8String:label] : nil;
	dispatch_async(dispatch_get_main_queue(), ^{ [[NSApp dockTile] setBadgeLabel:(s.length ? s : nil)]; });
}

static int omIsActive(void) {
	__block int active = 0;
	if ([NSThread isMainThread]) { return [NSApp isActive] ? 1 : 0; }
	dispatch_sync(dispatch_get_main_queue(), ^{ active = [NSApp isActive] ? 1 : 0; });
	return active;
}

static void omNotify(const char *title, const char *body) {
	NSString *t = [NSString stringWithUTF8String:title];
	NSString *b = [NSString stringWithUTF8String:body];
	UNUserNotificationCenter *c = [UNUserNotificationCenter currentNotificationCenter];
	[c requestAuthorizationWithOptions:(UNAuthorizationOptionAlert | UNAuthorizationOptionSound | UNAuthorizationOptionBadge) completionHandler:^(BOOL granted, NSError *err) {
		if (!granted) { return; }
		UNMutableNotificationContent *n = [UNMutableNotificationContent new];
		n.title = t; n.body = b; n.sound = [UNNotificationSound defaultSound];
		UNNotificationRequest *r = [UNNotificationRequest requestWithIdentifier:[[NSUUID UUID] UUIDString] content:n trigger:nil];
		[c addNotificationRequest:r withCompletionHandler:nil];
	}];
}
*/
import "C"

import (
	"strconv"
	"unsafe"
)

func setBadge(n int) {
	if n <= 0 {
		C.omSetBadge(nil)
		return
	}
	s := C.CString(strconv.Itoa(n))
	defer C.free(unsafe.Pointer(s))
	C.omSetBadge(s)
}

func appIsActive() bool { return C.omIsActive() == 1 }

func notify(title, body string) {
	t, b := C.CString(title), C.CString(body)
	defer C.free(unsafe.Pointer(t))
	defer C.free(unsafe.Pointer(b))
	C.omNotify(t, b)
}
