//go:build dev || production || bindings

// rig.go is the FIELD RIG: one double-click on a laptop that tears the picture,
// and every measurement the investigation needs comes back in one zip on the
// Desktop. It exists because the decisive experiments were designed on
// 2026-08-25 (docs/picture-diagnostics-runbook.md) and then never run — they
// were four tools, six commands and a PowerShell session, on a machine whose
// operator has a match to call. This is the same four experiments with nothing
// to type.
//
// # What one run measures
//
// The tear is decode corruption (grey/green garbage that clears at the next
// keyframe), the same stream decodes clean on the dev box, and it tears on
// several different laptops. The rig therefore takes the four readings that
// separate "the bytes", "this machine" and "real time" from one another:
//
//  1. The TRANSPORT STREAM AS RECEIVED, with no decoder anywhere near it
//     (internal/tsprobe, pure Go): real continuity holes versus flagged
//     discontinuities on the video PID, DTS monotonicity, the NAL census. And
//     the bytes are BANKED to cap.ts, so every later reading — here and on the
//     dev box — is over exactly these bytes.
//  2. The LIVE pipeline dissection (internal/gst.RunPipelineDiagnosticResult,
//     software decoder): the app's own receive path under real-time pressure,
//     stage by stage, with damaged frames COUNTED at the decoder.
//  3. The same dissection over cap.ts, as fast as the decoder can go: the same
//     bytes with no real-time pressure. Frames per second here is this
//     laptop's software decode ceiling for this stream, against the 50 it
//     needs.
//  4. cap.ts through the hardware decoder (d3d11h265dec), which either proves
//     the machine has one and it is clean, or records exactly why not.
//
// Plus the machine census (CPU, GPU and driver, memory, power, remote session),
// the app's recent logs and configuration, and a summary that states what the
// numbers say and what to do with the zip.
//
// # How it is started
//
// WSLCOMMS_RIG=1 in the environment, or --rig on the command line. The portable
// launcher sets the variable when its own file name contains "rig", so
// wslcomms-rig-v<ver>.exe IS the rig and wslcomms-portable-v<ver>.exe is the
// application, from one build. It runs after gst.Init and before wails.Run:
// no window, no single-instance lock, no capture leg — exactly like
// WSLCOMMS_DIAGNOSE, which it subsumes.
//
// # What it must never do
//
// Dial the return while the application holds it (an M2L-X output takes one
// caller): it waits for the app to be closed. Put the return passphrase in a
// file, a log line or a URI: the passphrase reaches srtsrc as a property and
// the zip records only that one was present.
package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"wslcomms/internal/applog"
	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/presets"
	"wslcomms/internal/secrets"
	"wslcomms/internal/tsprobe"
)

const (
	// rigEnv asks for the rig; any non-empty value.
	rigEnv = "WSLCOMMS_RIG"

	// rigPresetEnv names the preset whose instance is measured; default matchg.
	rigPresetEnv     = "WSLCOMMS_RIG_PRESET"
	rigDefaultPreset = "matchg"

	// rigSecsEnv is the live capture length in seconds; default 60. Sixty
	// seconds of the 15 Mbit/s return is about 112 MB of cap.ts.
	rigSecsEnv     = "WSLCOMMS_RIG_SECS"
	rigDefaultSecs = 60

	// rigTargetEnv overrides the dialled host:port — for the bench, where the
	// "M2L-X" is a local listener replaying a capture. Recorded loudly.
	rigTargetEnv = "WSLCOMMS_RIG_TARGET"

	// rigOutEnv overrides where the output folder and zip go; default Desktop.
	rigOutEnv = "WSLCOMMS_RIG_OUT"

	// rigNoWaitEnv ends the process without waiting for Enter — the bench, where
	// nobody is at the keyboard.
	rigNoWaitEnv = "WSLCOMMS_RIG_NOWAIT"

	// rigLogWindow and rigLogBudget bound what is collected from the log
	// directory: files touched in the last week, newest first, up to 300 MB.
	rigLogWindow = 7 * 24 * time.Hour
	rigLogBudget = 300 << 20

	// rigFileReplayCap bounds a file replay that never reaches EOS.
	rigFileReplayCap = 15 * time.Minute

	// rigWaitForClose is how long the rig waits for the application to be
	// closed before giving up on the SRT phases.
	rigWaitForClose = 10 * time.Minute

	rigSoftwareDecoder = "avdec_h265"
	rigHardwareDecoder = "d3d11h265dec"

	// rigStreamFPS is the return's frame rate, the bar a decoder has to clear.
	rigStreamFPS = 50.0
)

// rigRequested reports whether this process should run the rig instead of the
// application: WSLCOMMS_RIG set, or --rig / rig on the command line.
func rigRequested(args []string, getenv func(string) string) bool {
	if getenv(rigEnv) != "" {
		return true
	}
	for _, a := range args {
		switch strings.ToLower(a) {
		case "--rig", "-rig", "rig":
			return true
		}
	}
	return false
}

// rigSeconds is the live capture length: WSLCOMMS_RIG_SECS, or the default.
func rigSeconds(getenv func(string) string) int {
	if v := getenv(rigSecsEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return rigDefaultSecs
}

// rigTarget is the endpoint under test and where it came from.
type rigTarget struct {
	PresetID   string
	PresetName string
	M2LXHost   string
	Host       string
	Port       int
	LatencyMs  int
	PBKeyLen   int

	// Passphrase is used and never written; HasPassphrase is what is written.
	Passphrase    string
	HasPassphrase bool

	VideoSource   string
	SRTPort       int
	SRTSecondPort int
	Overridden    bool
	Notes         []string
}

func (t rigTarget) addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// rigDissect is one dissection's reading, the numbers the summary reasons on.
type rigDissect struct {
	Label      string
	Decoder    string
	Elapsed    time.Duration
	EndedEarly bool
	Received   uint64 // buffers at the raw stage
	Demuxed    uint64 // buffers into the parser
	Parsed     uint64 // access units into the decoder
	Decoded    uint64
	Corrupted  uint64
	Discont    uint64 // DISCONT flags on the demuxed video — the demuxer reacting to a hole
	Gap        uint64
	Continuity uint64 // tsdemux CONTINUITY mismatches on the bus
	Broken     uint64 // h265parse broken/invalid drops on the bus
	Errors     []string
	Err        string // the dissection itself failed (no decoder, no connection)
	Report     string // the dissector's own text
}

// FPS is decoded frames per second of running time.
func (d rigDissect) FPS() float64 {
	if d.Elapsed <= 0 {
		return 0
	}
	return float64(d.Decoded) / d.Elapsed.Seconds()
}

// rigDissectFrom reads a DiagnosticResult into a rigDissect.
func rigDissectFrom(label, decoder string, res gst.DiagnosticResult) rigDissect {
	d := rigDissect{
		Label:      label,
		Decoder:    decoder,
		Elapsed:    res.Elapsed,
		EndedEarly: res.EndedEarly,
		Decoded:    res.Decoded(),
		Corrupted:  res.Corrupted(),
		Continuity: res.BusMatching("continuity"),
		Broken:     res.BusMatching("broken/invalid"),
		Errors:     res.Errors,
		Report:     res.Report,
	}
	if len(res.Stages) > 0 {
		d.Received = res.Stages[0].Buffers
	}
	if len(res.Stages) > 1 {
		d.Demuxed = res.Stages[1].Buffers
		d.Discont = res.Stages[1].Flags["DISCONT"]
		d.Gap = res.Stages[1].Flags["GAP"]
	}
	if len(res.Stages) > 2 {
		d.Parsed = res.Stages[2].Buffers
	}
	return d
}

func (d rigDissect) line() string {
	if d.Err != "" {
		return fmt.Sprintf("%s: NOT AVAILABLE: %s", d.Label, d.Err)
	}
	how := ""
	if d.EndedEarly {
		how = " (ended at end of stream)"
	}
	return fmt.Sprintf("%s: decoded %d frames in %s%s = %.1f fps; CORRUPTED frames %d; "+
		"demux DISCONT %d, GAP %d; tsdemux continuity mismatches %d; h265parse broken/invalid %d; "+
		"raw buffers %d -> demuxed %d -> parsed AUs %d",
		d.Label, d.Decoded, d.Elapsed.Round(time.Millisecond), how, d.FPS(), d.Corrupted,
		d.Discont, d.Gap, d.Continuity, d.Broken, d.Received, d.Demuxed, d.Parsed)
}

// rig is one run.
type rig struct {
	con    io.Writer
	getenv func(string) string
	now    func() time.Time
	cpu    func() time.Duration

	stamp   string
	base    string // where the folder and the zip go
	dir     string // the output folder
	logFile *os.File
	secs    int

	target  rigTarget
	capPath string
	capSize int64

	machine  []string
	probe    *tsprobe.Summary
	probeErr string
	live     *rigDissect
	fileAV   *rigDissect
	fileHW   *rigDissect
	findings []string
	problems []string
	files    []string
}

// runRigAndExit runs the rig and ends the process. Like the diagnostic and the
// ordinary shutdown it leaves through forceExit, so the GStreamer, D3D11 and
// libsrt DLLs this process loaded cannot deadlock in DLL_PROCESS_DETACH.
func runRigAndExit(appDir string, gstInitErr error) {
	conFile, conErr := rigOpenConsole()
	var con io.Writer = io.Discard
	if conFile != nil {
		con = conFile
	}
	r := &rig{
		con:    con,
		getenv: os.Getenv,
		now:    time.Now,
		cpu:    rigProcessCPU,
	}
	r.stamp = r.now().Format("20060102-150405")
	r.secs = rigSeconds(r.getenv)

	fmt.Fprintf(con, "\n  WSL Commentary — field rig\n  ==========================\n\n")
	if conErr != nil {
		log.Printf("rig: no console: %v", conErr)
	}
	exe, _ := os.Executable()
	r.say("exe %s; app dir %s", exe, appDir)

	code := r.run(gstInitErr)

	r.say("")
	if code == 0 {
		r.say("DONE. Send the zip named above. Press Enter to close this window.")
	} else {
		r.say("FINISHED WITH PROBLEMS (listed above and in summary.txt). Send the zip anyway. Press Enter to close.")
	}
	if r.getenv(rigNoWaitEnv) == "" {
		rigWaitForEnter()
	}
	forceExit()
}

// say writes one line to the console, the rig log and the application log.
func (r *rig) say(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := r.now().Format("15:04:05") + "  " + msg
	fmt.Fprintln(r.con, line)
	if r.logFile != nil {
		fmt.Fprintln(r.logFile, line)
	}
	if msg != "" {
		log.Printf("rig: %s", msg)
	}
}

// phase runs one step, timing it and its CPU, and records a failure without
// stopping the run: every later reading that does not depend on it is still
// worth having.
func (r *rig) phase(name string, fn func() error) {
	r.say("")
	r.say("--- %s", name)
	t0, c0 := r.now(), r.cpu()
	err := fn()
	dt, dc := r.now().Sub(t0), r.cpu()-c0
	cores := 0.0
	if dt > 0 {
		cores = dc.Seconds() / dt.Seconds()
	}
	if err != nil {
		r.problems = append(r.problems, name+": "+err.Error())
		r.say("    FAILED after %s: %v", dt.Round(time.Millisecond), err)
		return
	}
	r.say("    done in %s (CPU %.2f cores busy)", dt.Round(time.Millisecond), cores)
}

func (r *rig) run(gstInitErr error) int {
	if err := r.openOutput(); err != nil {
		r.say("cannot create the output folder: %v", err)
		return 1
	}
	r.phase("machine census", r.phaseMachine)
	r.phase("the target, from the preset", r.phaseTarget)
	r.phase("the application must be closed", r.phaseWaitForApp)
	r.phase("SRT capture and transport-stream probe (no decoder)", r.phaseProbe)
	if gstInitErr != nil {
		r.problems = append(r.problems, "GStreamer did not initialise, so no pipeline dissection: "+gstInitErr.Error())
		r.say("GStreamer did not initialise: %v", gstInitErr)
	} else {
		r.phase("live pipeline dissection, software decoder", r.phaseLive)
		r.phase("file replay through the software decoder", r.phaseFileSoftware)
		r.phase("file replay through the hardware decoder", r.phaseFileHardware)
	}
	r.phase("collect logs and configuration", r.phaseLogs)
	r.phase("summary", r.phaseSummary)

	zipPath, err := r.zipUp()
	if err != nil {
		r.problems = append(r.problems, "zip: "+err.Error())
		r.say("could not zip the folder (%v); send the folder itself: %s", err, r.dir)
		return 1
	}
	r.say("")
	r.say("ZIP: %s", zipPath)
	r.say("folder: %s", r.dir)
	rigRevealFile(zipPath)
	if len(r.problems) > 0 {
		return 1
	}
	return 0
}

// --- output folder ----------------------------------------------------------

func (r *rig) openOutput() error {
	base := r.getenv(rigOutEnv)
	if base == "" {
		var err error
		base, err = rigDesktopDir()
		if err != nil {
			if base, err = applog.DefaultDir(); err != nil {
				return err
			}
		}
	}
	r.base = base
	r.dir = filepath.Join(base, "WSLComms-rig-"+r.stamp)
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(r.dir, "rig.log"))
	if err != nil {
		return err
	}
	r.logFile = f
	r.say("output folder: %s", r.dir)
	return nil
}

// write puts one file in the output folder and remembers it for the summary.
func (r *rig) write(name string, content []byte) error {
	p := filepath.Join(r.dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		return err
	}
	r.files = append(r.files, name)
	return nil
}

// --- phases -------------------------------------------------------------------

func (r *rig) phaseMachine() error {
	r.machine = rigMachineCensus()
	r.machine = append(r.machine, "logical processors: "+strconv.Itoa(rigNumCPU()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "WSLCOMMS_") && !strings.HasPrefix(kv, "WSLCOMMS_RIG") {
			r.machine = append(r.machine, "env: "+kv)
		}
	}
	for _, l := range r.machine {
		r.say("    %s", l)
	}
	return r.write("machine.txt", []byte(strings.Join(r.machine, "\n")+"\n"))
}

func (r *rig) phaseTarget() error {
	t := rigTarget{}
	cfg, err := config.Load()
	if err != nil {
		t.Notes = append(t.Notes, "config.json could not be loaded ("+err.Error()+"); defaults used underneath the preset")
		cfg = config.Defaults()
	}
	id := r.getenv(rigPresetEnv)
	if id == "" {
		id = rigDefaultPreset
	}
	t.PresetID = id
	scope := id
	p, perr := presets.Load(id)
	switch {
	case perr == nil:
		t.PresetName = p.Name
		if p.CredentialScope != "" {
			scope = p.CredentialScope
		}
		if ignored, aerr := presets.Apply(cfg, p.Fields); aerr != nil {
			t.Notes = append(t.Notes, "applying the preset: "+aerr.Error())
		} else if len(ignored) > 0 {
			t.Notes = append(t.Notes, "preset keys ignored: "+strings.Join(ignored, ", "))
		}
	case strings.HasPrefix(id, "match") && len(id) == len("match")+1:
		// Not on this machine's disk: the built-in values are the same thing.
		t.PresetName = "built-in " + id
		t.Notes = append(t.Notes, "preset "+id+" is not saved on this machine ("+perr.Error()+"); the built-in values were used")
		if _, aerr := presets.Apply(cfg, builtinFields(strings.TrimPrefix(id, "match"))); aerr != nil {
			return fmt.Errorf("applying the built-in %s: %w", id, aerr)
		}
	default:
		return fmt.Errorf("preset %q: %w", id, perr)
	}

	t.M2LXHost = cfg.M2LXHost
	t.Host = cfg.EffectiveSRTReturnHost()
	t.Port = cfg.EffectiveSRTReturnPort()
	t.LatencyMs = cfg.EffectivePictureLatencyMs()
	t.PBKeyLen = cfg.SRTReturnPBKeyLen
	t.VideoSource = cfg.EffectiveVideoSource()
	t.SRTPort = cfg.SRTPort
	t.SRTSecondPort = cfg.SRTSecondPort

	if ov := r.getenv(rigTargetEnv); ov != "" {
		host, port, err := net.SplitHostPort(tsprobe.NormalizeAddr(ov))
		if err != nil {
			return fmt.Errorf("%s=%q: %w", rigTargetEnv, ov, err)
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return fmt.Errorf("%s=%q: port: %w", rigTargetEnv, ov, err)
		}
		t.Host, t.Port, t.Overridden = host, n, true
		t.Notes = append(t.Notes, "TARGET OVERRIDDEN by "+rigTargetEnv+": this is not the preset's return")
	}

	if t.PBKeyLen != 0 {
		store := secrets.New()
		key, kerr := secrets.ScopedKey(scope, secrets.KeySRTReturn)
		var v string
		var gerr error = kerr
		if kerr == nil {
			v, gerr = store.Get(key)
		}
		if gerr != nil {
			v, gerr = store.Get(secrets.KeySRTReturn)
		}
		if gerr == nil && v != "" {
			t.Passphrase, t.HasPassphrase = v, true
		} else {
			t.Notes = append(t.Notes, fmt.Sprintf("the return is encrypted (pbkeylen %d) but no passphrase is stored in %s for scope %q; the listener will refuse the SRT phases",
				t.PBKeyLen, secrets.StoreName(), scope))
		}
	}
	r.target = t

	enc := "none"
	if t.PBKeyLen != 0 {
		enc = fmt.Sprintf("pbkeylen %d, passphrase present: %v", t.PBKeyLen, t.HasPassphrase)
	}
	lines := []string{
		"preset        : " + t.PresetID + " (" + t.PresetName + ")",
		"m2lxHost      : " + t.M2LXHost,
		"return        : " + t.addr() + " (srtsrc caller, latency " + strconv.Itoa(t.LatencyMs) + " ms, encryption " + enc + ")",
		"videoSource   : " + t.VideoSource,
		"send ports    : " + strconv.Itoa(t.SRTPort) + " and " + strconv.Itoa(t.SRTSecondPort) + " (0 = no second output)",
		"capture secs  : " + strconv.Itoa(r.secs),
	}
	for _, n := range t.Notes {
		lines = append(lines, "note          : "+n)
	}
	for _, l := range lines {
		r.say("    %s", l)
	}
	return r.write("target.txt", []byte(strings.Join(lines, "\n")+"\n"))
}

func (r *rig) phaseWaitForApp() error {
	if r.target.Overridden {
		// The bench: the target is a local listener, not the facility's output,
		// so a running application is not in the way.
		r.say("    target overridden; not waiting for the application to close")
		return nil
	}
	deadline := r.now().Add(rigWaitForClose)
	lastSaid := time.Time{}
	for {
		apps := rigRunningApps()
		if len(apps) == 0 {
			r.say("    no other WSL Commentary process is running")
			return nil
		}
		if r.now().Sub(lastSaid) >= 15*time.Second {
			r.say("    WSL Commentary is running: %s", strings.Join(apps, ", "))
			r.say("    CLOSE IT — the main window and the PGM monitor — so the rig can take the picture connection. Waiting...")
			lastSaid = r.now()
		}
		if r.now().After(deadline) {
			return fmt.Errorf("still running after %s: %s (the SRT phases will be refused by the listener)",
				rigWaitForClose, strings.Join(apps, ", "))
		}
		time.Sleep(2 * time.Second)
	}
}

func (r *rig) phaseProbe() error {
	t := r.target
	if t.Host == "" {
		return errors.New("no target: the preset phase failed")
	}
	capPath := filepath.Join(r.dir, "cap.ts")
	f, err := os.Create(capPath)
	if err != nil {
		return err
	}
	r.say("    capturing %s for %d s into cap.ts", t.addr(), r.secs)
	an, dur, cerr := tsprobe.Capture(context.Background(), t.addr(), tsprobe.CaptureOpts{
		LatencyMs:  t.LatencyMs,
		Passphrase: t.Passphrase,
		Duration:   time.Duration(r.secs) * time.Second,
		Save:       f,
		Progress:   func(s string) { r.say("    %s", s) },
	})
	_ = f.Close()
	if st, serr := os.Stat(capPath); serr == nil && st.Size() > 0 {
		r.capPath, r.capSize = capPath, st.Size()
		r.files = append(r.files, "cap.ts")
	} else {
		_ = os.Remove(capPath)
	}
	if an != nil {
		if werr := r.write("probe-live.txt", []byte(an.Report(t.addr(), dur))); werr != nil {
			return werr
		}
		s := an.Summary()
		r.probe = &s
		r.say("    %d bytes (%.2f Mbit/s), video PID 0x%04x %s; continuity on the video PID: REAL %d, flagged %d, dup %d; DTS backwards %d; verdict %s",
			s.Bytes, s.Mbps, s.VideoPID, s.VideoCodec, s.RealCC, s.FlaggedCC, s.DupCC, s.DTSBackwards, s.Verdict)
	}
	if cerr != nil {
		r.probeErr = cerr.Error()
		return cerr
	}
	return nil
}

func (r *rig) dissect(label, uri string, dur time.Duration, decoder string, live bool) (rigDissect, error) {
	opts := gst.DiagnosticOpts{}
	if live {
		opts = gst.DiagnosticOpts{LatencyMs: r.target.LatencyMs, PBKeyLen: r.target.PBKeyLen, Passphrase: r.target.Passphrase}
	}
	res, err := gst.RunPipelineDiagnosticResult(uri, dur, decoder, opts)
	if err != nil {
		return rigDissect{Label: label, Decoder: decoder, Err: err.Error()}, err
	}
	d := rigDissectFrom(label, decoder, res)
	r.say("    %s", d.line())
	for _, e := range d.Errors {
		r.say("    bus ERROR: %s", e)
	}
	return d, nil
}

func (r *rig) phaseLive() error {
	t := r.target
	if t.Host == "" {
		return errors.New("no target: the preset phase failed")
	}
	uri := "srt://" + t.addr()
	r.say("    srtsrc ! tsdemux ! queue ! h265parse ! %s ! fakesink on %s for %d s (real time)", rigSoftwareDecoder, t.addr(), r.secs)
	d, err := r.dissect("live, this laptop, "+rigSoftwareDecoder, uri, time.Duration(r.secs)*time.Second, rigSoftwareDecoder, true)
	r.live = &d
	if err != nil {
		return err
	}
	return r.write("dissect-live-"+rigSoftwareDecoder+".txt", []byte(d.Report))
}

func (r *rig) phaseFileSoftware() error {
	if r.capPath == "" {
		return errors.New("no cap.ts to replay: the capture phase produced nothing")
	}
	r.say("    filesrc cap.ts ! tsdemux ! queue ! h265parse ! %s ! fakesink, as fast as the decoder can go", rigSoftwareDecoder)
	d, err := r.dissect("file replay, this laptop, "+rigSoftwareDecoder, r.capPath, rigFileReplayCap, rigSoftwareDecoder, false)
	r.fileAV = &d
	if err != nil {
		return err
	}
	return r.write("dissect-file-"+rigSoftwareDecoder+".txt", []byte(d.Report))
}

func (r *rig) phaseFileHardware() error {
	if r.capPath == "" {
		return errors.New("no cap.ts to replay: the capture phase produced nothing")
	}
	r.say("    filesrc cap.ts ! tsdemux ! queue ! h265parse ! %s ! fakesink", rigHardwareDecoder)
	d, err := r.dissect("file replay, this laptop, "+rigHardwareDecoder, r.capPath, rigFileReplayCap, rigHardwareDecoder, false)
	r.fileHW = &d
	if err != nil {
		// A machine with no hardware HEVC decoder is a finding, not a failure.
		r.say("    hardware decoder not usable here: %v", err)
		return nil
	}
	return r.write("dissect-file-"+rigHardwareDecoder+".txt", []byte(d.Report))
}

func (r *rig) phaseLogs() error {
	logDir, err := applog.DefaultDir()
	if err != nil {
		return err
	}
	files, err := rigSelectLogFiles(logDir, r.now(), rigLogWindow, rigLogBudget)
	if err != nil {
		return err
	}
	dst := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var total int64
	for _, src := range files {
		n, err := rigCopyFile(src, filepath.Join(dst, filepath.Base(src)))
		if err != nil {
			r.say("    could not copy %s: %v", src, err)
			continue
		}
		total += n
	}
	r.say("    %d log files, %d MB, from %s", len(files), total>>20, logDir)

	cdir := filepath.Join(r.dir, "config")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return err
	}
	if p, err := config.Path(); err == nil {
		if _, err := rigCopyFile(p, filepath.Join(cdir, "config.json")); err == nil {
			r.say("    config.json copied")
		}
	}
	if pdir, err := presets.Dir(); err == nil {
		src := filepath.Join(pdir, r.target.PresetID+".json")
		if _, err := rigCopyFile(src, filepath.Join(cdir, r.target.PresetID+".json")); err == nil {
			r.say("    preset %s.json copied", r.target.PresetID)
		}
	}
	return nil
}

// rigSelectLogFiles picks the regular files in dir modified within window of
// now, newest first, until budget bytes have been chosen.
func rigSelectLogFiles(dir string, now time.Time, window time.Duration, budget int64) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type cand struct {
		path string
		mod  time.Time
		size int64
	}
	var cands []cand
	cutoff := now.Add(-window)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		cands = append(cands, cand{filepath.Join(dir, e.Name()), info.ModTime(), info.Size()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	var out []string
	var total int64
	for _, c := range cands {
		if total+c.size > budget && len(out) > 0 {
			break
		}
		out = append(out, c.path)
		total += c.size
	}
	return out, nil
}

func rigCopyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// --- the summary ------------------------------------------------------------------

func (r *rig) phaseSummary() error {
	r.findings = rigFindings(r.probe, r.live, r.fileAV, r.fileHW)
	text := rigSummaryText(r)
	for _, f := range r.findings {
		r.say("    * %s", f)
	}
	return r.write("summary.txt", []byte(text))
}

// rigFindings turns the four readings into sentences, each one a fact the
// numbers support and nothing more. The cross-machine comparison (the same
// cap.ts on the dev box) is deliberately not claimed here: it has not been
// done yet when this runs.
func rigFindings(probe *tsprobe.Summary, live, fileAV, fileHW *rigDissect) []string {
	var out []string
	if probe != nil {
		switch probe.Verdict {
		case tsprobe.VerdictNoVideo:
			out = append(out, "The probe saw no video PID: nothing usable arrived over SRT.")
		case tsprobe.VerdictDTSBackwards:
			out = append(out, fmt.Sprintf("The video DTS went backwards %d time(s) as received: a sender-side restart fault, upstream of every decoder.", probe.DTSBackwards))
		case tsprobe.VerdictRealHoles:
			out = append(out, fmt.Sprintf("The transport stream ARRIVES WITH %d REAL HOLES on the video PID in %.0f s (no decoder involved): the bytes are damaged before decode.", probe.RealCC, probe.Span.Seconds()))
		case tsprobe.VerdictFlaggedOnly:
			out = append(out, fmt.Sprintf("The transport stream arrives intact: %d flagged (legal) discontinuities and 0 real holes on the video PID.", probe.FlaggedCC))
		case tsprobe.VerdictClean:
			out = append(out, "The transport stream arrives clean: no continuity errors of any kind on the video PID.")
		}
	}
	if fileAV != nil && fileAV.Err == "" && fileAV.Decoded > 0 {
		fps := fileAV.FPS()
		switch {
		case fps < rigStreamFPS:
			out = append(out, fmt.Sprintf("SOFTWARE DECODE CANNOT KEEP UP ON THIS LAPTOP: %.1f fps flat out against the %.0f fps the stream needs (file replay, %s).", fps, rigStreamFPS, fileAV.Decoder))
		case fps < rigStreamFPS*1.3:
			out = append(out, fmt.Sprintf("Software decode is MARGINAL on this laptop: %.1f fps flat out against %.0f needed (file replay, %s); anything else running tips it over.", fps, rigStreamFPS, fileAV.Decoder))
		default:
			out = append(out, fmt.Sprintf("Software decode has headroom on this laptop: %.1f fps flat out against %.0f needed (file replay, %s).", fps, rigStreamFPS, fileAV.Decoder))
		}
	}
	if live != nil && live.Err == "" {
		switch {
		case live.Corrupted > 0 && fileAV != nil && fileAV.Err == "" && fileAV.Corrupted == 0:
			out = append(out, fmt.Sprintf("The decoder flagged %d corrupted frames LIVE but 0 replaying the same bytes from the file: the damage comes from real-time pressure on this laptop, not from the bytes.", live.Corrupted))
		case live.Corrupted > 0 && fileAV != nil && fileAV.Err == "" && fileAV.Corrupted > 0:
			out = append(out, fmt.Sprintf("The decoder flagged corrupted frames both live (%d) and replaying the file (%d): the same bytes decode damaged on this laptop with no real-time pressure. Replay cap.ts on the dev box to split bytes from CPU.", live.Corrupted, fileAV.Corrupted))
		case live.Corrupted > 0:
			out = append(out, fmt.Sprintf("The decoder flagged %d corrupted frames live (no file replay to compare).", live.Corrupted))
		case live.Corrupted == 0 && live.Decoded > 0:
			out = append(out, "The decoder flagged 0 corrupted frames live in this run; if the picture tore during it, the damage is not flagged by the decoder (silent wrong decode) or is downstream.")
		}
		if live.Continuity > 0 && probe != nil && probe.RealCC == 0 {
			out = append(out, fmt.Sprintf("tsdemux reported %d continuity mismatches in the live dissection while the probe saw 0 real holes: the holes are made on this laptop's receive path (a reader that falls behind), not on the wire.", live.Continuity))
		}
		if live.Broken > 0 {
			out = append(out, fmt.Sprintf("h265parse dropped %d broken/invalid NALs live (the rare parser-state burst).", live.Broken))
		}
		if live.Decoded > 0 && live.FPS() < rigStreamFPS*0.9 {
			out = append(out, fmt.Sprintf("Live, the decoder produced %.1f fps over the run against %.0f expected: it fell behind real time.", live.FPS(), rigStreamFPS))
		}
	}
	if fileHW != nil {
		switch {
		case fileHW.Err != "":
			out = append(out, "No usable hardware HEVC decoder on this laptop ("+rigHardwareDecoder+"): "+fileHW.Err)
		case fileHW.Decoded > 0 && fileHW.Corrupted == 0:
			out = append(out, fmt.Sprintf("The HARDWARE decoder is available here and decoded the file clean: %d frames at %.1f fps, 0 corrupted.", fileHW.Decoded, fileHW.FPS()))
		case fileHW.Decoded > 0:
			out = append(out, fmt.Sprintf("The hardware decoder ran but flagged %d corrupted frames of %d.", fileHW.Corrupted, fileHW.Decoded))
		default:
			out = append(out, "The hardware decoder element was created but decoded nothing.")
		}
	}
	return out
}

func rigSummaryText(r *rig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WSL Commentary — field rig — %s\n", r.stamp)
	fmt.Fprintf(&b, "==========================================\n\n")
	fmt.Fprintf(&b, "MACHINE\n")
	for _, l := range r.machine {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	t := r.target
	fmt.Fprintf(&b, "\nTARGET\n  preset %s (%s), m2lxHost %s\n  return %s, latency %d ms, pbkeylen %d, passphrase present %v\n  videoSource %s, send ports %d/%d\n",
		t.PresetID, t.PresetName, t.M2LXHost, t.addr(), t.LatencyMs, t.PBKeyLen, t.HasPassphrase, t.VideoSource, t.SRTPort, t.SRTSecondPort)
	for _, n := range t.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}

	fmt.Fprintf(&b, "\nTHE FOUR READINGS\n")
	if r.probe != nil {
		p := r.probe
		fmt.Fprintf(&b, "  1. probe (bytes as received, no decoder): %d bytes, %.2f Mbit/s over %.1f s; video PID 0x%04x %s; REAL holes %d, flagged %d, dup %d, TEI %d; DTS backwards %d; NALs %d, IDR %d; verdict %s\n",
			p.Bytes, p.Mbps, p.Span.Seconds(), p.VideoPID, p.VideoCodec, p.RealCC, p.FlaggedCC, p.DupCC, p.TEI, p.DTSBackwards, p.NALUnits, p.IDRFrames, p.Verdict)
	} else {
		fmt.Fprintf(&b, "  1. probe: NOT RUN (%s)\n", r.probeErr)
	}
	if r.capPath != "" {
		fmt.Fprintf(&b, "     cap.ts banked: %d MB\n", r.capSize>>20)
	}
	for i, d := range []*rigDissect{r.live, r.fileAV, r.fileHW} {
		if d == nil {
			fmt.Fprintf(&b, "  %d. NOT RUN\n", i+2)
			continue
		}
		fmt.Fprintf(&b, "  %d. %s\n", i+2, d.line())
	}

	fmt.Fprintf(&b, "\nWHAT THE NUMBERS SAY\n")
	if len(r.findings) == 0 {
		fmt.Fprintf(&b, "  (no readings to speak of)\n")
	}
	for _, f := range r.findings {
		fmt.Fprintf(&b, "  * %s\n", f)
	}

	fmt.Fprintf(&b, "\nHOW TO READ THEM\n")
	fmt.Fprintf(&b, "  Reading 1 is the bytes before any decoder: REAL holes there mean the stream arrives damaged.\n")
	fmt.Fprintf(&b, "  Reading 2 is the app's own receive path under real time; reading 3 is the same bytes with no clock.\n")
	fmt.Fprintf(&b, "  Corruption in 2 but not 3: this laptop cannot keep up live. In both: these bytes decode damaged on this CPU —\n")
	fmt.Fprintf(&b, "  the next step is replaying cap.ts on the dev box, which is why it is in the zip. Reading 4 says whether the\n")
	fmt.Fprintf(&b, "  laptop has a hardware HEVC decoder at all. The 3 fps figure is this laptop's software decode ceiling.\n")

	if len(r.problems) > 0 {
		fmt.Fprintf(&b, "\nPROBLEMS DURING THE RUN\n")
		for _, p := range r.problems {
			fmt.Fprintf(&b, "  ! %s\n", p)
		}
	}
	fmt.Fprintf(&b, "\nFILES\n")
	for _, f := range r.files {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	fmt.Fprintf(&b, "  logs/ (the app's last week of logs), config/ (config.json and the preset), rig.log\n")
	return b.String()
}

// --- the zip ----------------------------------------------------------------------

func (r *rig) zipUp() (string, error) {
	host, _ := os.Hostname()
	if host == "" {
		host = "laptop"
	}
	zipPath := filepath.Join(r.base, "WSLComms-rig-"+rigSlug(host)+"-"+r.stamp+".zip")
	if r.logFile != nil {
		_ = r.logFile.Sync()
	}
	out, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(out)
	root := filepath.Base(r.dir)
	walkErr := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(r.dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = root + "/" + filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	})
	if cerr := zw.Close(); walkErr == nil {
		walkErr = cerr
	}
	if cerr := out.Close(); walkErr == nil {
		walkErr = cerr
	}
	if walkErr != nil {
		return "", walkErr
	}
	return zipPath, nil
}

func rigSlug(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
