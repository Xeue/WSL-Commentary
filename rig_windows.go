//go:build windows && (dev || production || bindings)

// rig_windows.go is the field rig's Windows half: a console for a process that
// was linked without one, the machine census, the running-process check, the
// Desktop, and the process-CPU clock. See rig.go.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	rigKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	rigUser32   = windows.NewLazySystemDLL("user32.dll")

	rigProcAllocConsole         = rigKernel32.NewProc("AllocConsole")
	rigProcAttachConsole        = rigKernel32.NewProc("AttachConsole")
	rigProcSetConsoleTitleW     = rigKernel32.NewProc("SetConsoleTitleW")
	rigProcGlobalMemoryStatusEx = rigKernel32.NewProc("GlobalMemoryStatusEx")
	rigProcGetSystemPowerStatus = rigKernel32.NewProc("GetSystemPowerStatus")
	rigProcGetSystemMetrics     = rigUser32.NewProc("GetSystemMetrics")
)

// rigOpenConsole gives this -H windowsgui process a console to talk through:
// the parent's, if it was started from one, otherwise a new window.
func rigOpenConsole() (*os.File, error) {
	// Redirected already (a file or a pipe, as on the bench): that is the console.
	if st, err := os.Stdout.Stat(); err == nil && (st.Mode().IsRegular() || st.Mode()&os.ModeNamedPipe != 0) {
		return os.Stdout, nil
	}
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	if r, _, _ := rigProcAttachConsole.Call(attachParentProcess); r == 0 {
		if r, _, err := rigProcAllocConsole.Call(); r == 0 {
			return nil, fmt.Errorf("AllocConsole: %v", err)
		}
	}
	if title, err := windows.UTF16PtrFromString("WSL Commentary — field rig"); err == nil {
		rigProcSetConsoleTitleW.Call(uintptr(unsafe.Pointer(title)))
	}
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening CONOUT$: %w", err)
	}
	return f, nil
}

// rigWaitForEnter blocks until the operator presses Enter in the console, or
// for a minute if there is no console to read.
func rigWaitForEnter() {
	f, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		time.Sleep(time.Minute)
		return
	}
	defer f.Close()
	_, _ = bufio.NewReader(f).ReadString('\n')
}

// rigDesktopDir is the user's Desktop: where a field operator will find the
// zip without being told a path.
func rigDesktopDir() (string, error) {
	p, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, 0)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", fmt.Errorf("Desktop folder %q is not usable", p)
	}
	return p, nil
}

// rigRevealFile opens Explorer with the file selected.
func rigRevealFile(path string) {
	_ = exec.Command("explorer.exe", "/select,"+path).Start()
}

func rigNumCPU() int { return runtime.NumCPU() }

// rigProcessCPU is this process's kernel+user CPU time so far.
func rigProcessCPU() time.Duration {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0
	}
	return time.Duration(kernel.Nanoseconds() + user.Nanoseconds())
}

// rigRunningApps lists the other WSL Commentary processes: the application, its
// PGM monitor, another rig. This process and its launcher are not "other".
func rigRunningApps() []string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	me := windows.GetCurrentProcessId()
	var parent uint32
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	type proc struct {
		name string
		pid  uint32
	}
	var procs []proc
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		name := windows.UTF16ToString(pe.ExeFile[:])
		if pe.ProcessID == me {
			parent = pe.ParentProcessID
			continue
		}
		procs = append(procs, proc{name, pe.ProcessID})
	}
	var out []string
	for _, p := range procs {
		lower := strings.ToLower(p.name)
		if !strings.Contains(lower, "wslcomms") || strings.Contains(lower, ".test") || p.pid == parent {
			continue
		}
		out = append(out, fmt.Sprintf("%s (pid %d)", p.name, p.pid))
	}
	return out
}

type rigMemoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type rigSystemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// rigMachineCensus describes the machine the way a decode-performance question
// needs it described: OS build, CPU, memory, every display adapter and its
// driver, whether it is on battery, whether the session is remote.
func rigMachineCensus() []string {
	var out []string
	v := windows.RtlGetVersion()
	out = append(out, fmt.Sprintf("os: Windows %d.%d build %d", v.MajorVersion, v.MinorVersion, v.BuildNumber))
	if h, err := os.Hostname(); err == nil {
		out = append(out, "computer: "+h)
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		name, _, _ := k.GetStringValue("ProcessorNameString")
		mhz, _, _ := k.GetIntegerValue("~MHz")
		k.Close()
		out = append(out, fmt.Sprintf("cpu: %s (~%d MHz)", strings.TrimSpace(name), mhz))
	}
	var ms rigMemoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r, _, _ := rigProcGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
		out = append(out, fmt.Sprintf("memory: %d MB total, %d MB free (%d%% in use)", ms.TotalPhys>>20, ms.AvailPhys>>20, ms.MemoryLoad))
	}
	const displayClass = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClass, registry.ENUMERATE_SUB_KEYS); err == nil {
		subs, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, sub := range subs {
			if len(sub) != 4 {
				continue
			}
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClass+`\`+sub, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			desc, _, _ := sk.GetStringValue("DriverDesc")
			ver, _, _ := sk.GetStringValue("DriverVersion")
			date, _, _ := sk.GetStringValue("DriverDate")
			sk.Close()
			if desc != "" {
				out = append(out, fmt.Sprintf("gpu: %s (driver %s, %s)", desc, ver, date))
			}
		}
	}
	var ps rigSystemPowerStatus
	if r, _, _ := rigProcGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&ps))); r != 0 {
		ac := "unknown"
		switch ps.ACLineStatus {
		case 0:
			ac = "ON BATTERY"
		case 1:
			ac = "mains power"
		}
		bat := "no battery"
		if ps.BatteryFlag&128 == 0 && ps.BatteryLifePercent != 255 {
			bat = fmt.Sprintf("battery %d%%", ps.BatteryLifePercent)
		}
		out = append(out, "power: "+ac+", "+bat)
	}
	const smRemoteSession = 0x1000
	if r, _, _ := rigProcGetSystemMetrics.Call(smRemoteSession); r != 0 {
		out = append(out, "session: REMOTE (RDP or similar)")
	} else {
		out = append(out, "session: local console")
	}
	if s := os.Getenv("SESSIONNAME"); s != "" {
		out = append(out, "session name: "+s)
	}
	if outb, err := exec.Command("powercfg", "/getactivescheme").Output(); err == nil {
		out = append(out, "power plan: "+strings.TrimSpace(string(outb)))
	}
	return out
}
