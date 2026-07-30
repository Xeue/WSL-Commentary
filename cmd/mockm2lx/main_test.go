package main

import (
	"testing"
	"time"

	"wslcomms/internal/m2lx"
)

func TestParseFlags_Defaults(t *testing.T) {
	opts, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if opts.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", opts.Addr)
	}
	if opts.SRTAddr != ":4001" {
		t.Errorf("SRTAddr = %q, want :4001", opts.SRTAddr)
	}
	if !opts.OnePeerOnly {
		t.Errorf("OnePeerOnly default = false, want true")
	}
	if opts.RefusalWindow != 6*time.Second {
		t.Errorf("RefusalWindow = %s, want 6s", opts.RefusalWindow)
	}
	if opts.TokenTTL != m2lx.TokenLifetime {
		t.Errorf("TokenTTL = %s, want m2lx.TokenLifetime (%s)", opts.TokenTTL, m2lx.TokenLifetime)
	}
	if opts.SRTPBKeylen != 16 {
		t.Errorf("SRTPBKeylen = %d, want 16", opts.SRTPBKeylen)
	}
	if opts.StatusKey != "cam7" {
		t.Errorf("StatusKey = %q, want cam7", opts.StatusKey)
	}
}

func TestParseFlags_Overrides(t *testing.T) {
	opts, err := parseFlags([]string{
		"-addr=127.0.0.1:9000",
		"-srt-addr=127.0.0.1:9001",
		"-alias=someone",
		"-password=hunter2",
		"-status-key=cam3",
		"-status-interval=500ms",
		"-token-ttl=1h",
		"-srt-passphrase=letmein",
		"-srt-pbkeylen=32",
		"-one-peer-only=false",
		"-refusal-window=2s",
		"-stall-status=true",
		"-lie-stream-state=starting",
		"-drop-audio=true",
		"-verbose=true",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}

	want := Options{
		Addr:           "127.0.0.1:9000",
		SRTAddr:        "127.0.0.1:9001",
		Alias:          "someone",
		Password:       "hunter2",
		StatusKey:      "cam3",
		StatusInterval: 500 * time.Millisecond,
		TokenTTL:       time.Hour,
		SRTPassphrase:  "letmein",
		SRTPBKeylen:    32,
		OnePeerOnly:    false,
		RefusalWindow:  2 * time.Second,
		StallStatus:    true,
		LieStreamState: m2lx.StreamStateStarting,
		DropAudio:      true,
		Verbose:        true,
	}
	if opts != want {
		t.Errorf("parseFlags = %+v, want %+v", opts, want)
	}
}

func TestParseFlags_RejectsBadPBKeylen(t *testing.T) {
	if _, err := parseFlags([]string{"-srt-pbkeylen=17"}); err == nil {
		t.Fatalf("expected an error for -srt-pbkeylen=17")
	}
}

func TestParseFlags_RejectsBadLieStreamState(t *testing.T) {
	if _, err := parseFlags([]string{"-lie-stream-state=on-fire"}); err == nil {
		t.Fatalf("expected an error for an invalid -lie-stream-state")
	}
}

func TestParseFlags_RejectsNegativeRefusalWindow(t *testing.T) {
	if _, err := parseFlags([]string{"-refusal-window=-1s"}); err == nil {
		t.Fatalf("expected an error for a negative -refusal-window")
	}
}

func TestParseFlags_RejectsZeroStatusInterval(t *testing.T) {
	if _, err := parseFlags([]string{"-status-interval=0s"}); err == nil {
		t.Fatalf("expected an error for a zero -status-interval")
	}
}

func TestParseFlags_AcceptsEmptyLieStreamState(t *testing.T) {
	opts, err := parseFlags([]string{"-lie-stream-state="})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.LieStreamState != "" {
		t.Errorf("LieStreamState = %q, want empty", opts.LieStreamState)
	}
}
