; ===========================================================================
;  WSL Studios Commentary Contribution - Windows installer
;  Owner: WP-6. Specification section 11.
;
;  Inno Setup, not WiX: this installs one feature, per-machine, with no
;  options, and WiX's advantages - component GUIDs, patches, transforms,
;  administrative installs - buy nothing here while costing an MSI authoring
;  toolchain and a build-time dependency on the WiX version in use.
;
;  Build with Inno Setup 6.3 or newer:
;      "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" build\installer.iss
;  See build\README.md for the whole Gate B sequence that produces dist\.
;
;  *** UNVERIFIED - GATE B MUST CONFIRM ***
;  No Inno Setup compiler exists on the machine this file was written on, so
;  this script has never been compiled. It is written against Inno Setup 6.3+
;  syntax. Anything it gets wrong will surface as a compile error in ISCC, not
;  as a broken installer, with one exception noted at [Files].
; ===========================================================================

#define AppName        "WSL Commentary"
#define AppExeName     "wslcomms.exe"
#define AppPublisher   "WSL Studios"

; Version. UNVERIFIED - there is no released version yet, so this is a
; placeholder. Pass a real one from the build:
;     ISCC.exe /DAppVersion=1.0.0 build\installer.iss
#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif

; SrcDir is the staging folder, which is a byte-for-byte image of the installed
; program folder: wslcomms.exe, slate.png, licenses\, gst\, and (in the default
; AppDir runtime layout) the GStreamer runtime DLLs alongside the executable.
; build\bundle-gst.ps1 fills in gst\. See build\README.md for the rest.
#ifndef SrcDir
  #define SrcDir AddBackslash(SourcePath) + "..\dist"
#endif

; --------------------------------------------------------------------------
; PROVENANCE GATE.
; gst\BUNDLE-MANIFEST.txt is written only by build\bundle-gst.ps1, which copies
; the GStreamer runtime from an explicit file list and fails if anything
; matching *x264* is anywhere near the output. Refusing to compile without it
; means a hand-assembled or directory-copied gst\ folder - the exact mistake
; that would put GPL x264 into a commercial deliverable - cannot be packaged by
; accident.
;
; If your ISPP build rejects the #error line's syntax, the compile still fails,
; which is the required outcome. Do not delete this block to make it compile.
; --------------------------------------------------------------------------
#if !FileExists(AddBackslash(SrcDir) + "gst\BUNDLE-MANIFEST.txt")
  #error dist\gst\BUNDLE-MANIFEST.txt is missing. Run build\bundle-gst.ps1 first - the GStreamer bundle must be produced by that script.
#endif

#if !FileExists(AddBackslash(SrcDir) + AppExeName)
  #error dist\wslcomms.exe is missing. Run "wails build -webview2 embed" and stage the executable - see build\README.md.
#endif

#if !FileExists(AddBackslash(SrcDir) + "slate.png")
  #error dist\slate.png is missing. Copy assets\slate.png into dist\ - see build\README.md.
#endif

#if !FileExists(AddBackslash(SrcDir) + "licenses\LGPL-2.1.txt")
  #error dist\licenses is missing. Copy build\licenses\ into dist\licenses\ - shipping without the LGPL text and the written offer is a licence breach, not a cosmetic omission.
#endif

[Setup]
; AppId identifies this product to Windows for upgrade and uninstall. It is
; fixed forever. Changing it turns an upgrade into a second, parallel
; installation, and the first one becomes impossible to uninstall from the
; installer. Never change this value.
AppId={{1424551D-FF20-4B9B-A9C7-29F83C01EAB1}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher={#AppPublisher}
VersionInfoVersion={#AppVersion}

; Per-machine. One commentary position, installed by the facility engineer;
; every user who logs in at that position gets the same working installation.
; PrivilegesRequiredOverridesAllowed is deliberately left at its default
; (empty): there is no /CURRENTUSER escape hatch. A per-user installation would
; put the GStreamer bundle somewhere the next commentator to log in cannot see.
PrivilegesRequired=admin

DefaultDirName={autopf}\WSL Studios\{#AppName}
DefaultGroupName={#AppName}

; Windows 11 x64 (specification section 1). WebView2 Evergreen is part of
; Windows 11, which is why the app needs nothing else installed. 10.0.22000 is
; the first Windows 11 build. If a Windows 10 test machine is ever needed,
; lower this AND confirm the WebView2 Evergreen runtime is present on it - the
; embedded bootstrapper can install it, but that is a network operation and
; facility networks have no outbound web access.
MinVersion=10.0.22000
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

; No options, no configuration screens (specification section 11). One feature,
; no components, no tasks, no post-install checkbox, nothing to get wrong at
; two in the morning before a match. Everything configurable lives in the app's
; own Settings screen and in %APPDATA%\WSLComms\config.json.
DisableWelcomePage=yes
DisableDirPage=yes
DisableProgramGroupPage=yes
DisableReadyPage=yes
DisableFinishedPage=no
AllowNoIcons=no
WizardStyle=modern

; ~70-120 MB installed (specification section 11): a ~15 MB executable plus the
; GStreamer folder. Solid LZMA2 compresses a folder of DLLs well.
Compression=lzma2/max
SolidCompression=yes
OutputDir={#SrcDir}\installer
OutputBaseFilename=wslcomms-setup
UninstallDisplayName={#AppName}
UninstallDisplayIcon={app}\{#AppExeName}

; If the app is running during an upgrade its DLLs are locked. Ask Restart
; Manager to close it, but never restart it afterwards: this machine may be
; live, and nothing should launch a media application without a human doing it.
CloseApplications=yes
RestartApplications=no

; Writes %TEMP%\Setup Log*.txt. A facility engineer who hits a problem at 14:00
; on a match day should not have to reproduce it to explain it.
SetupLogging=yes

; Authenticode signing. UNVERIFIED - no certificate is known to this project.
; A broadcast facility will usually require a signed installer, and SmartScreen
; will warn on an unsigned one. Configure a SignTool in the IDE and uncomment:
; SignTool=standard
; SignedUninstaller=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
; wslcomms.exe: Wails, WebView2 bootstrapper embedded, frontend embedded.
Source: "{#SrcDir}\{#AppExeName}"; DestDir: "{app}"; Flags: ignoreversion

; The 1920x1080 slate fed to filesrc ! pngdec ! imagefreeze. config.json's
; slatePath defaults to "slate.png" relative to the program folder, so this
; file must sit next to the executable.
Source: "{#SrcDir}\slate.png"; DestDir: "{app}"; Flags: ignoreversion

; Licence texts, the written offer and the third-party notice. Not optional:
; the LGPL-2.1 requires that a copy of the licence is supplied with the work.
Source: "{#SrcDir}\licenses\*"; DestDir: "{app}\licenses"; Flags: ignoreversion recursesubdirs createallsubdirs

; The GStreamer bundle. internal\gst points GST_PLUGIN_PATH_1_0 at
; <appdir>\gst\lib\gstreamer-1.0, so this tree must keep its shape.
;
; This IS a recursive copy, and that is safe here for one reason only: the
; contents of dist\gst were produced file-by-file from the explicit allowlist
; in build\bundle-gst.ps1, which audits its own output and refuses to leave
; anything unlisted or anything matching *x264* behind. The provenance gate at
; the top of this file is what enforces that this is true. Never point this at
; a GStreamer installation directory.
Source: "{#SrcDir}\gst\*"; DestDir: "{app}\gst"; Flags: ignoreversion recursesubdirs createallsubdirs

; The GStreamer, GLib and MinGW runtime DLLs in the default AppDir layout,
; where they sit beside the executable because the Windows loader resolves
; wslcomms.exe's imports before any Go code can alter the search path.
; skipifsourcedoesntexist covers the GstBin layout, where these files are
; inside gst\bin and are installed by the line above instead.
;
; The one thing the compiler cannot check for you: if you built dist\ with the
; GstBin layout and wslcomms.exe cannot find its DLLs at startup, that is a
; runtime failure (0xC0000135), not a compile failure. See build\README.md,
; "DLL search order".
Source: "{#SrcDir}\*.dll"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist

[Icons]
; A desktop shortcut, because that is how the commentator starts the
; application (specification section 1). No Tasks page offering the choice:
; there is no choice.
;
; WorkingDir is set explicitly rather than left to default. config.json's
; slatePath defaults to the bare relative name "slate.png" (contract,
; specification section 9), so anything that resolves it against the process
; working directory needs that directory to be the program folder. Costs
; nothing to pin; costs a match if it is wrong.
Name: "{autoprograms}\{#AppName}"; Filename: "{app}\{#AppExeName}"; WorkingDir: "{app}"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#AppExeName}"; WorkingDir: "{app}"

; ===========================================================================
;  UNINSTALL
;
;  There is deliberately no [UninstallDelete] section, and there must never be
;  one that touches user data. Uninstalling removes the program folder and
;  nothing else. In particular it MUST NOT remove:
;
;    %APPDATA%\WSLComms\config.json
;        The M2L-X host, alias, event id, SRT host and port, statusKey, the
;        chosen DVS input and headphone endpoints, the monitor tile geometry
;        and the return mid. Specification section 9. Some of these values are
;        only obtainable from someone else's system - the statusKey, for one -
;        so destroying them can leave a commentary position unusable until an
;        engineer with M2L-X access is available. An uninstall, including the
;        one that happens silently in the middle of an upgrade, must not be
;        able to do that.
;
;    Windows Credential Manager: WSLComms/m2lx and WSLComms/srt
;        The M2L-X password and the SRT passphrase. Deleting a credential the
;        user cannot re-derive is worse than leaving one behind, and neither is
;        a secret that an uninstaller has any business touching. Inno does not
;        touch Credential Manager unless told to; it is not told to.
;
;    %LOCALAPPDATA%\WSLComms\registry.bin
;        The GStreamer plugin registry cache. Rebuildable in under a second, so
;        removing it would gain nothing - and under an admin uninstall
;        {localappdata} resolves to the ADMINISTRATOR's profile, not the
;        commentator's, so the attempt would delete the wrong user's file or
;        nothing at all. Left alone on purpose.
;
;  If a genuine "remove my settings too" requirement ever appears, it belongs
;  in the application, behind a confirmation, not in an uninstaller.
; ===========================================================================
