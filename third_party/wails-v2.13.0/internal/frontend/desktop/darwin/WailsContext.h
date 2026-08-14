//
//  WailsContext.h
//  test
//
//  Created by Lea Anthony on 10/10/21.
//

#ifndef WailsContext_h
#define WailsContext_h

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import "WailsWebView.h"

#if __has_include(<UniformTypeIdentifiers/UTType.h>)
#define USE_NEW_FILTERS
#import <UniformTypeIdentifiers/UTType.h>
#endif

#define ON_MAIN_THREAD(str) dispatch_async(dispatch_get_main_queue(), ^{ str; });
#define unicode(input) [NSString stringWithFormat:@"%C", input]

@interface WailsWindow : NSWindow

@property NSSize userMinSize;
@property NSSize userMaxSize;
@property bool disableEscapeExitsFullscreen;

- (BOOL) canBecomeKeyWindow;
- (void) applyWindowConstraints;
- (void) disableWindowConstraints;
@end

@interface WailsContext : NSObject <WKURLSchemeHandler,WKScriptMessageHandler,WKNavigationDelegate,WKUIDelegate>

@property (retain) WailsWindow* mainWindow;
@property (retain) WailsWebView* webview;
@property (nonatomic, assign) id appdelegate;

@property bool hideOnClose;
@property bool shuttingDown;
@property bool startHidden;
@property bool startFullscreen;

@property bool singleInstanceLockEnabled;
@property (retain) NSString* singleInstanceUniqueId;

@property (retain) NSEvent* mouseEvent;

@property bool alwaysOnTop;

@property bool devtoolsEnabled;
@property bool defaultContextMenuEnabled;

@property (retain) WKUserContentController* userContentController;

@property (retain) NSMenu* applicationMenu;

@property (retain) NSImage* aboutImage;
@property (retain) NSString* aboutTitle;
@property (retain) NSString* aboutDescription;

struct Preferences {
  bool *tabFocusesLinks;
  bool *textInteractionEnabled;
  bool *fullscreenEnabled;
  const char *applicationNameForUserAgent;
};

- (void) CreateWindow:(int)width :(int)height :(bool)frameless :(bool)resizable :(bool)zoomable :(bool)fullscreen :(bool)fullSizeContent :(bool)hideTitleBar :(bool)titlebarAppearsTransparent  :(bool)hideTitle :(bool)useToolbar :(bool)hideToolbarSeparator :(bool)webviewIsTransparent :(bool)hideWindowOnClose :(NSString *)appearance :(bool)windowIsTranslucent :(int)minWidth :(int)minHeight :(int)maxWidth :(int)maxHeight :(bool)fraudulentWebsiteWarningEnabled :(struct Preferences)preferences :(bool)enableDragAndDrop :(bool)disableWebViewDragAndDrop :(bool)disableEscapeExitsFullscreen;
- (void) SetSize:(int)width :(int)height;
- (void) SetPosition:(int)x :(int) y;
- (void) SetMinSize:(int)minWidth :(int)minHeight;
- (void) SetMaxSize:(int)maxWidth :(int)maxHeight;
- (void) SetTitle:(NSString*)title;
- (void) SetAlwaysOnTop:(int)onTop;
- (void) Center;
- (void) Fullscreen;
- (void) UnFullscreen;
- (bool) IsFullScreen;
- (void) Minimise;
- (void) UnMinimise;
- (bool) IsMinimised;
- (void) Maximise;
- (void) ToggleMaximise;
- (void) UnMaximise;
- (bool) IsMaximised;
- (void) SetBackgroundColour:(int)r :(int)g :(int)b :(int)a;
- (void) HideMouse;
- (void) ShowMouse;
- (void) Hide;
- (void) Show;
- (void) HideApplication;
- (void) ShowApplication;
- (void) Quit;

- (void) MessageDialog :(NSString*)dialogType :(NSString*)title :(NSString*)message :(NSString*)button1 :(NSString*)button2 :(NSString*)button3 :(NSString*)button4 :(NSString*)defaultButton :(NSString*)cancelButton :(void*)iconData :(int)iconDataLength;
- (void) OpenFileDialog :(NSString*)title :(NSString*)defaultFilename :(NSString*)defaultDirectory :(bool)allowDirectories :(bool)allowFiles :(bool)canCreateDirectories :(bool)treatPackagesAsDirectories :(bool)resolveAliases :(bool)showHiddenFiles :(bool)allowMultipleSelection :(NSString*)filters;
- (void) SaveFileDialog :(NSString*)title :(NSString*)defaultFilename :(NSString*)defaultDirectory :(bool)canCreateDirectories :(bool)treatPackagesAsDirectories :(bool)showHiddenFiles :(NSString*)filters;

- (bool) IsNotificationAvailable;
- (bool) CheckBundleIdentifier;
- (bool) EnsureDelegateInitialized;
- (void) RequestNotificationAuthorization:(int)channelID;
- (void) CheckNotificationAuthorization:(int)channelID;
- (void) SendNotification:(int)channelID :(const char *)identifier :(const char *)title :(const char *)subtitle :(const char *)body :(const char *)dataJSON;
- (void) SendNotificationWithActions:(int)channelID :(const char *)identifier :(const char *)title :(const char *)subtitle :(const char *)body :(const char *)categoryId :(const char *)actionsJSON;
- (void) RegisterNotificationCategory:(int)channelID :(const char *)categoryId :(const char *)actionsJSON :(bool)hasReplyField :(const char *)replyPlaceholder :(const char *)replyButtonTitle;
- (void) RemoveNotificationCategory:(int)channelID :(const char *)categoryId;
- (void) RemoveAllPendingNotifications;
- (void) RemovePendingNotification:(const char *)identifier;
- (void) RemoveAllDeliveredNotifications;
- (void) RemoveDeliveredNotification:(const char *)identifier;

- (void) loadRequest:(NSString*)url;
- (void) ExecJS:(NSString*)script;
- (NSScreen*) getCurrentScreen;

- (void) SetAbout :(NSString*)title :(NSString*)description :(void*)imagedata :(int)datalen;
- (void) dealloc;

// WSLCOMMS PATCH — see third_party/README.md.
//
// WKUIDelegate's media-capture callback. Upstream v2.13.0 declares WKUIDelegate
// above but implements only runOpenPanelWithParameters, and with no
// implementation of this method WebKit never answers the permission request:
// getUserMedia does not reject, it HANGS forever, which takes the whole
// headphone-device dropdown down with it. Declared here as well as defined in
// the .m purely so the next person reading the header can see that this class
// has been changed.
//
// Guarded on the SDK, not the deployment target: WKMediaCaptureType and
// WKPermissionDecision arrived in the macOS 12 SDK and we still build with
// -mmacosx-version-min=10.13. API_AVAILABLE keeps the compiler quiet about
// that; the method is simply never called on anything older.
#if defined(MAC_OS_VERSION_12_0) && MAC_OS_X_VERSION_MAX_ALLOWED >= MAC_OS_VERSION_12_0
- (void)webView:(WKWebView *)webView
    requestMediaCapturePermissionForOrigin:(WKSecurityOrigin *)origin
                          initiatedByFrame:(WKFrameInfo *)frame
                                      type:(WKMediaCaptureType)type
                           decisionHandler:(void (^)(WKPermissionDecision decision))decisionHandler
    API_AVAILABLE(macos(12.0));
#endif

// WSLCOMMS PATCH — see third_party/README.md.
//
// WKUIDelegate's three JavaScript panels. Upstream v2.13.0 implements none of
// them, and WKWebView draws no dialog of its own: with the methods absent,
// window.alert shows nothing, window.confirm returns false and window.prompt
// returns null, which silently disabled every preset operation on the Settings
// page. Declared here for the same reason as the callback above — so the header
// admits that this class differs from the release — and, unlike that one,
// without an SDK guard, because these three have existed since WebKit shipped
// WKWebView in macOS 10.10 and we build against far newer.
- (void)webView:(WKWebView *)webView runJavaScriptAlertPanelWithMessage:(NSString *)message
    initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(void))completionHandler;
- (void)webView:(WKWebView *)webView runJavaScriptConfirmPanelWithMessage:(NSString *)message
    initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(BOOL result))completionHandler;
- (void)webView:(WKWebView *)webView runJavaScriptTextInputPanelWithPrompt:(NSString *)prompt
    defaultText:(NSString *)defaultText initiatedByFrame:(WKFrameInfo *)frame
    completionHandler:(void (^)(NSString *result))completionHandler;

@end


#endif /* WailsContext_h */
