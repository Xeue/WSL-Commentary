//go:build !windows && (dev || production || bindings)

// rig_other.go is the field rig's platform half off Windows: enough to build
// and to run from a terminal. The rig is a Windows-laptop instrument.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func rigOpenConsole() (*os.File, error) { return os.Stdout, nil }

func rigWaitForEnter() {
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func rigDesktopDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, "Desktop")
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", fmt.Errorf("no Desktop at %s", p)
	}
	return p, nil
}

func rigRevealFile(string) {}

func rigNumCPU() int { return runtime.NumCPU() }

func rigProcessCPU() time.Duration { return 0 }

func rigRunningApps() []string { return nil }

func rigMachineCensus() []string {
	return []string{"os: " + runtime.GOOS + " " + runtime.GOARCH}
}
