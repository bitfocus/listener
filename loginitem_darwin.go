//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation -framework ServiceManagement
#import <Foundation/Foundation.h>
#import <ServiceManagement/ServiceManagement.h>
#include <stdlib.h>

// SMAppService only works for an app bundle; `go run` has no bundle identifier.
int loginItemBundled(void) {
	return [[NSBundle mainBundle] bundleIdentifier] != nil;
}

int loginItemStatus(void) {
	return (int)[SMAppService mainAppService].status;
}

// setLoginItemEnabled returns NULL on success, or an error message the caller frees.
char *setLoginItemEnabled(int on) {
	@autoreleasepool {
		NSError *err = nil;
		SMAppService *svc = [SMAppService mainAppService];
		BOOL ok = on ? [svc registerAndReturnError:&err] : [svc unregisterAndReturnError:&err];
		if (ok) {
			return NULL;
		}
		return strdup(err.localizedDescription.UTF8String ?: "unknown error");
	}
}

void openLoginItemsSettings(void) {
	[SMAppService openSystemSettingsLoginItems];
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

const loginItemBlockedHint = "Turned off in System Settings → General → Login Items."

func loginItemSupported() bool { return C.loginItemBundled() != 0 }

func getLoginItemStatus() loginItemStatus {
	switch C.loginItemStatus() {
	case C.SMAppServiceStatusEnabled:
		return loginItemOn
	case C.SMAppServiceStatusRequiresApproval:
		return loginItemBlocked
	default:
		return loginItemOff
	}
}

func setLoginItem(on bool) error {
	var flag C.int
	if on {
		flag = 1
	}
	msg := C.setLoginItemEnabled(flag)
	if msg == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(msg))
	// Registering an item the user switched off in System Settings fails, but
	// leaves it waiting for their approval there; the caller handles that state.
	if on && getLoginItemStatus() == loginItemBlocked {
		return nil
	}
	return errors.New(C.GoString(msg))
}

// macOS tracks the app bundle itself, so a moved app needs no update.
func syncLoginItem() {}

func openLoginItemSettings() { C.openLoginItemsSettings() }
