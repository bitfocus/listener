//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework ApplicationServices
#import <Cocoa/Cocoa.h>
#import <ApplicationServices/ApplicationServices.h>

void setAccessoryActivationPolicy(void) {
	[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
}

// The GLFW content view swallows mouse-downs, so the title bar area would no
// longer move the window. This transparent strip hands them to the window drag.
// The drag starts on the first mouse-dragged event: starting it on a plain
// click makes AppKit log "Window move completed without beginning".
@interface ListenerDragView : NSView {
	NSEvent *downEvent;
}
@end

@implementation ListenerDragView
- (BOOL)mouseDownCanMoveWindow {
	return YES;
}
- (void)mouseDown:(NSEvent *)event {
	[downEvent release];
	downEvent = [event retain];
}
- (void)mouseDragged:(NSEvent *)event {
	if (downEvent == nil) {
		return;
	}
	[self.window performWindowDragWithEvent:downEvent];
	[downEvent release];
	downEvent = nil;
}
// Double-clicking a title bar follows the user's Dock setting.
- (void)mouseUp:(NSEvent *)event {
	[downEvent release];
	downEvent = nil;
	if (event.clickCount != 2) {
		return;
	}
	NSString *action = [[NSUserDefaults standardUserDefaults] stringForKey:@"AppleActionOnDoubleClick"];
	if ([action isEqualToString:@"Minimize"]) {
		[self.window performMiniaturize:nil];
	} else if (![action isEqualToString:@"None"]) {
		[self.window performZoom:nil];
	}
}
- (void)dealloc {
	[downEvent release];
	[super dealloc];
}
@end

// A unified toolbar makes the title bar 52pt tall with the traffic lights
// centred in it; content then draws underneath so the app header shares the row.
void unifyTitlebar(uintptr_t ptr, double dragHeight) {
	NSWindow *win = (NSWindow *)ptr;
	void (^apply)(void) = ^{
		NSView *content = win.contentView;
		// Already applied: don't stack a second toolbar and drag strip.
		for (NSView *sub in content.subviews) {
			if ([sub isKindOfClass:[ListenerDragView class]]) {
				return;
			}
		}
		// cgo builds Objective-C without ARC, so release what we alloc once
		// its owner has retained it.
		if (@available(macOS 11.0, *)) {
			NSToolbar *toolbar = [[NSToolbar alloc] initWithIdentifier:@"listener.main"];
			win.toolbar = toolbar;
			[toolbar release];
			win.toolbarStyle = NSWindowToolbarStyleUnified;
		}
		// GLFW opts the window into full screen; a settings window has no use for
		// it, and the green button then zooms instead.
		win.collectionBehavior = (win.collectionBehavior & ~NSWindowCollectionBehaviorFullScreenPrimary) |
			NSWindowCollectionBehaviorFullScreenNone;
		win.titleVisibility = NSWindowTitleHidden;
		win.titlebarAppearsTransparent = YES;
		win.styleMask |= NSWindowStyleMaskFullSizeContentView;

		NSRect bounds = content.bounds;
		CGFloat y = content.isFlipped ? 0 : bounds.size.height - dragHeight;
		ListenerDragView *drag = [[ListenerDragView alloc]
			initWithFrame:NSMakeRect(0, y, bounds.size.width, dragHeight)];
		drag.autoresizingMask = NSViewWidthSizable |
			(content.isFlipped ? NSViewMaxYMargin : NSViewMinYMargin);
		[content addSubview:drag];
		[drag release];
	};
	if ([NSThread isMainThread]) {
		apply();
	} else {
		dispatch_sync(dispatch_get_main_queue(), apply);
	}
}
*/
import "C"

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
	"fyne.io/fyne/v2/theme"
)

// titlebarHeight is the unified title bar height; the in-app header fills it.
const titlebarHeight = 52

func titlebarInsets() float32 { return titlebarHeight }

// applyWindowChrome hides the native title, lets content fill the title bar
// and keeps that band draggable. The window must already be shown so the
// NSWindow exists. The header band must stay free of interactive widgets.
func applyWindowChrome(w fyne.Window) {
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		return
	}
	nw.RunNative(func(ctx any) {
		if mac, ok := ctx.(driver.MacWindowContext); ok && mac.NSWindow != 0 {
			C.unifyTitlebar(C.uintptr_t(mac.NSWindow), C.double(titlebarHeight))
		}
	})
}

// trayIconResource returns the menu bar glyph. Fyne only marks a systray icon
// as a macOS template image when the resource is a *theme.ThemedResource, so
// wrap the monochrome glyph in one; macOS then tints it for the current menu bar
// appearance instead of drawing our full-colour app icon. ThemedResource
// recolours its content as SVG, so the glyph must be SVG rather than PNG.
func trayIconResource(fyne.Resource) fyne.Resource {
	return theme.NewThemedResource(fyne.NewStaticResource("tray_icon.svg", trayIconSVG))
}

// hideDockIcon runs the app as an accessory (menu-bar) process so it does not
// appear in the Dock. Needed because GLFW/Fyne can reset activation policy even
// when Info.plist has LSUIElement=true.
func hideDockIcon() {
	C.setAccessoryActivationPolicy()
}

// accessibilityTrusted reports whether macOS has granted this process
// Accessibility permission, which keyboard and mouse control depend on.
func accessibilityTrusted() bool {
	return C.AXIsProcessTrusted() != 0
}
