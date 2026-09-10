package main

import "testing"

func TestRigLauncherName(t *testing.T) {
	for _, tc := range []struct {
		arg0 string
		want bool
	}{
		{`C:\Users\x\Desktop\wslcomms-rig-v1.6.2.exe`, true},
		{`wslcomms-RIG.exe`, true},
		{`C:\Users\x\Desktop\wslcomms-portable-v1.6.2.exe`, false},
		{`wslcomms.exe`, false},
		// Only the file name counts: a folder called "rig" is not a request.
		{`C:\rig\wslcomms-portable.exe`, false},
	} {
		if got := rigLauncherName(tc.arg0); got != tc.want {
			t.Errorf("rigLauncherName(%q) = %v, want %v", tc.arg0, got, tc.want)
		}
	}
}
