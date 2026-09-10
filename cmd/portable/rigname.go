package main

import (
	"path/filepath"
	"strings"
)

// rigLauncherName reports whether the launcher was started under a file name
// that asks for the field rig: "wslcomms-rig-v1.6.2.exe", say. The application
// itself decides from WSLCOMMS_RIG; this is only how a double-click, which can
// pass no argument and set no variable, gets to say so.
func rigLauncherName(arg0 string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(arg0)), "rig")
}
