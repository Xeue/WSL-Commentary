//go:build (dev || production || bindings) && darwin

// The one exit this application is allowed to take that macOS cannot refuse.
//
// Owner: WP-8, with app.go, main.go and exit_windows.go.
//
// This is the macOS half of the hard exit. It exists for the same REASON as
// exit_windows.go and by a completely different MECHANISM, and the difference
// was worth measuring rather than assuming, because the obvious guess — "dyld
// has no DLL_PROCESS_DETACH, so the Windows hazard cannot exist here" — is
// true and still does not get you a safe os.Exit.
//
// # What was measured, on this machine, before any of this was written
//
// A test binary registered a C atexit handler and a C++ static destructor and
// left by each of three doors. Results, macOS 15 / arm64, Go 1.25:
//
//	os.Exit(7)                      atexit RAN, static destructor RAN
//	syscall.Exit(7)                 atexit RAN, static destructor RAN
//	syscall.Kill(self, SIGKILL)     neither ran
//
// syscall.Exit is on that list because it is the plausible cheap answer and it
// is NOT one: on darwin Go reaches libc rather than the raw exit trap, so it is
// the same exit(3) os.Exit is and it runs the same hooks. There is no os.Exit
// variant that skips them.
//
// Then the same three doors against a termination hook that BLOCKS — not a
// sleep, which the Go runtime's preemption signals cut short and which made the
// first attempt at this measurement lie, but a pthread_mutex_lock on a mutex
// another thread already holds and never releases, which is the shape an
// abandoned GStreamer thread actually leaves behind:
//
//	os.Exit(7)                      never exited; had to be SIGKILLed from outside
//	syscall.Exit(7)                 never exited; had to be SIGKILLed from outside
//	syscall.Kill(self, SIGKILL)     exited immediately
//
// That is the whole argument. atexit(3) has no timeout, nothing on the Go side
// can reach a handler once exit(3) has started calling them, and a handler that
// blocks is a process that never dies — which is the identical outcome to the
// Windows loader-lock deadlock even though not one step of the mechanism is the
// same.
//
// # And the hooks are real, not hypothetical
//
// atexit and __cxa_atexit were interposed under DYLD_INSERT_LIBRARIES in a
// process that called gst_init and then made osxaudiosrc, atenc and srtsink —
// the send path's own elements. What registered:
//
//	libgobject-2.0.0.dylib   debug_objects_atexit            (at load)
//	libsrt.1.5.4.dylib       srt::CUDTUnited::~CUDTUnited    (when srtsink was made)
//	libsrt.1.5.4.dylib       srt::sync::Mutex::~Mutex
//	libsrt.1.5.4.dylib       srt::sync::CEvent::~CEvent
//	libsrt.1.5.4.dylib       srt::sync::CThreadError::~CThreadError
//	libsrt.1.5.4.dylib       srt_logging::LogConfig::~LogConfig
//	libsrt.1.5.4.dylib       srt::SrtOptionAction::~SrtOptionAction
//
// The ordering in that list is the point. The SRT destructors are not there when
// the process starts; they are registered at the moment srtsink is created,
// which is the moment this application enters the state the hard exit exists
// for. ~CUDTUnited is SRT's global socket table going away, and disassembly says
// it destroys a Mutex, destroys a Condition and walks a CCache on the way out.
//
// # Why that is dangerous HERE and not merely untidy
//
// macOS differs from Windows in the one way that makes it worse rather than
// better. ExitProcess kills every other thread FIRST and then runs the detach
// handlers over the wreckage; exit(3) does not stop anything. Measured with a
// spinning C thread and a handler that burned wall clock: the thread ticked 64
// times DURING the handler. So on macOS the termination hooks run CONCURRENTLY
// with every thread still alive, including the GStreamer thread that teardown
// has just given up on — and they are hooks that free SRT's global socket table
// and destroy the mutex protecting it.
//
// This is only ever reached after App.teardown has abandoned a step, which means
// a thread is somewhere inside gst_element_set_state that no Go-side deadline
// can reach; internal/gst explains why that call cannot be interrupted. Tearing
// libsrt's globals down underneath it is a race whose good outcome is that the
// process exits anyway and whose bad outcomes are a hang in a handler waiting on
// a lock that thread holds, or a crash report from a use-after-free at the exact
// moment the operator asked the application to close.
//
// # Why SIGKILL and not a raw exit syscall
//
// syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0) was tried and it WORKS: measured,
// it skips the atexit handlers and the static destructors, survives the blocking
// handler, and unlike SIGKILL it preserves the exit status. It was rejected
// anyway. Go reaches it through libc's syscall(3), which Apple deprecated and
// documents as removable, and its failure mode is the worst available one —
// RawSyscall would simply return and the function would fall through, from a
// caller that has already logged the process as ending. SIGKILL is the opposite
// bet: it is the oldest guarantee in the kernel, it cannot be caught, blocked or
// ignored by any handler libsrt or GLib could register, and there is nothing
// between here and it that can quietly stop working.
//
// # What is given up
//
// The exit status, and nothing else. The process is reported as signalled
// (status 137) rather than as having exited 0, where exit_windows.go's
// TerminateProcess(self, 0) yields a clean zero. Nothing reads it: this is a GUI
// process launched from the Dock or from Finder, and its parent is launchd.
// SIGKILL was checked against the crash reporter for exactly this reason and
// leaves no report — ~/Library/Logs/DiagnosticReports held 212 files before and
// 212 after, because ReportCrash triggers on Mach exceptions and on SIGSEGV,
// SIGBUS, SIGILL and SIGABRT, and Force Quit itself is a SIGKILL. So the
// operator gets no "wslcomms quit unexpectedly" dialog on a shutdown they asked
// for.
//
// Everything durable is already committed, exactly as exit_windows.go sets out:
// config.json, remote.json and mixer-golden.json are each written through a temp
// file and an os.Rename before the call that wrote them returns, Keychain writes
// are synchronous, and there is no buffered writer anywhere in the process. The
// audio device, both SRT sockets and the M2L-X fan-out slot are released by the
// kernel when the process object goes, which is as true of a signalled process
// as of an exited one.
//
// # Why it is not the normal exit
//
// Same answer as on Windows: a shutdown that completed has nothing to hide from,
// and the ordinary path lets the Go runtime finish and lets Wails return its
// error to main. This replaces forceExit, which teardown calls only when it has
// already abandoned a step.
package main

import (
	"log"
	"os"
	"syscall"
)

func init() {
	forceExit = terminateSelf
}

// terminateSelf ends this process without running an atexit handler or a static
// destructor.
//
// There is no error an ordinary run can produce here. kill(2) fails only with
// EINVAL (SIGKILL is valid), EPERM (a process may always signal itself) or
// ESRCH (this process exists, by inspection), so the branch below is not a
// recovery path — it is there because a function called terminateSelf must never
// return to a caller that has already logged the process as ending, and the
// ordinary exit is a worse outcome than this one rather than an impossible one.
//
// Nothing after the Kill runs in practice: SIGKILL is delivered on the return to
// user space, so the thread does not get another instruction. The lines below it
// are the shape of the promise, not a code path with a measurement behind it.
func terminateSelf() {
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGKILL); err != nil {
		log.Printf("wslcomms: could not SIGKILL this process (%v); falling back to the ordinary exit, "+
			"which runs the atexit handlers libsrt and GObject registered and may block in one of them "+
			"if a media thread was abandoned", err)
	}
	os.Exit(0)
}
