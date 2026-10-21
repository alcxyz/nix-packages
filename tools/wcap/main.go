package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// State persists between start and stop across separate wcap invocations.
type State struct {
	RecorderPID     int    `json:"recorder_pid"`
	StreamIndex     int    `json:"stream_index"`
	OriginalSink    int    `json:"original_sink"`
	OutputFile      string `json:"output_file"`
	LoopbackPID     int    `json:"loopback_pid,omitempty"`
	WcapSinkModule  int    `json:"wcap_sink_module,omitempty"`
	CreatedWcapSink bool   `json:"created_wcap_sink,omitempty"`
}

// pactlSinkInput mirrors the JSON output of `pactl --format=json list sink-inputs`.
type pactlSinkInput struct {
	Index      int               `json:"index"`
	Sink       int               `json:"sink"`
	Properties map[string]string `json:"properties"`
}

// pactlSink mirrors the JSON output of `pactl --format=json list sinks`.
type pactlSink struct {
	Index      int               `json:"index"`
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("wcap: ")

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wcap <start|stop|monitor>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "start":
		os.Exit(runStart(os.Args[2:]))
	case "stop":
		os.Exit(runStop(os.Args[2:]))
	case "monitor":
		os.Exit(runMonitor(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nusage: wcap <start|stop|monitor>\n", os.Args[1])
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// State management
// ---------------------------------------------------------------------------

var (
	errNoRecording  = errors.New("no active recording")
	errInvalidState = errors.New("invalid recording state")
)

func stateDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "wcap")
	}
	return filepath.Join(os.TempDir(), "wcap-"+strconv.Itoa(os.Getuid()))
}

func privateOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && info.Mode().Perm()&0077 == 0
}

func openStateDir() (*os.File, error) {
	dir := stateDir()
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening state directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), dir)
	info, err := file.Stat()
	if err != nil || !info.IsDir() || !privateOwned(info) {
		file.Close()
		return nil, fmt.Errorf("state directory must be private and owned by the current user")
	}
	return file, nil
}

func loadState() (*State, error) {
	dir, err := openStateDir()
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := syscall.Openat(int(dir.Fd()), "state.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, errNoRecording
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "state.json")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !privateOwned(info) {
		return nil, fmt.Errorf("state file must be regular, private and owned by the current user")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidState, err)
	}
	if s.RecorderPID <= 0 {
		return nil, fmt.Errorf("%w: recorder PID must be positive", errInvalidState)
	}
	return &s, nil
}

func saveState(s *State) error {
	dir, err := openStateDir()
	if err != nil {
		return err
	}
	defer dir.Close()
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	name := ".state-" + rand.Text()
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer syscall.Unlinkat(int(dir.Fd()), name)
	file := os.NewFile(uintptr(fd), name)
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return syscall.Renameat(int(dir.Fd()), name, int(dir.Fd()), "state.json")
}

func removeState() error {
	dir, err := openStateDir()
	if err != nil {
		return err
	}
	defer dir.Close()
	return syscall.Unlinkat(int(dir.Fd()), "state.json")
}

// ---------------------------------------------------------------------------
// PipeWire / PulseAudio helpers
// ---------------------------------------------------------------------------

func listSinkInputs() ([]pactlSinkInput, error) {
	out, err := exec.Command("pactl", "--format=json", "list", "sink-inputs").Output()
	if err != nil {
		return nil, fmt.Errorf("pactl list sink-inputs: %w", err)
	}
	var inputs []pactlSinkInput
	if err := json.Unmarshal(out, &inputs); err != nil {
		return nil, fmt.Errorf("parsing sink-inputs json: %w", err)
	}
	return inputs, nil
}

func findSinkIndex(name string) (int, error) {
	out, err := exec.Command("pactl", "--format=json", "list", "sinks").Output()
	if err != nil {
		return 0, fmt.Errorf("pactl list sinks: %w", err)
	}
	var sinks []pactlSink
	if err := json.Unmarshal(out, &sinks); err != nil {
		return 0, fmt.Errorf("parsing sinks json: %w", err)
	}
	for _, s := range sinks {
		if s.Name == name {
			return s.Index, nil
		}
	}
	return 0, fmt.Errorf("sink %q not found", name)
}

func ensureWcapSink() (sinkIndex int, moduleID int, created bool, err error) {
	if sinkIndex, err = findSinkIndex("wcap-sink"); err == nil {
		return sinkIndex, 0, false, nil
	}

	out, err := exec.Command(
		"pactl",
		"load-module",
		"module-null-sink",
		"sink_name=wcap-sink",
		"sink_properties=device.description=wcap_recording_sink",
		"channels=2",
		"channel_map=front-left,front-right",
	).Output()
	if err != nil {
		return 0, 0, false, fmt.Errorf("creating wcap-sink: %w", err)
	}

	moduleID, err = strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, 0, true, fmt.Errorf("parsing wcap-sink module id %q: %w", strings.TrimSpace(string(out)), err)
	}

	sinkIndex, err = findSinkIndex("wcap-sink")
	if err != nil {
		exec.Command("pactl", "unload-module", strconv.Itoa(moduleID)).Run()
		return 0, 0, true, fmt.Errorf("created wcap-sink but cannot find it: %w", err)
	}

	return sinkIndex, moduleID, true, nil
}

func unloadModule(moduleID int) {
	if moduleID > 0 {
		exec.Command("pactl", "unload-module", strconv.Itoa(moduleID)).Run()
	}
}

func moveSinkInput(streamIndex, sinkIndex int) error {
	return exec.Command("pactl", "move-sink-input",
		strconv.Itoa(streamIndex),
		strconv.Itoa(sinkIndex),
	).Run()
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// ---------------------------------------------------------------------------
// fuzzel picker
// ---------------------------------------------------------------------------

func fuzzelPick(items []string, prompt string) (string, error) {
	cmd := exec.Command("fuzzel", "--dmenu", "--prompt", prompt+" ")
	cmd.Stdin = strings.NewReader(strings.Join(items, "\n"))
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("fuzzel cancelled or failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

func runStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join(os.Getenv("HOME"), "Videos", "wcap"), "output directory")
	fs.Parse(args)

	if _, err := loadState(); err == nil {
		log.Println("recording already active (run 'wcap stop' first)")
		return 1
	} else if !errors.Is(err, errNoRecording) {
		log.Printf("reading recording state: %v", err)
		return 1
	}

	wcapIdx, wcapModuleID, createdWcapSink, err := ensureWcapSink()
	if err != nil {
		log.Printf("%v\n", err)
		return 1
	}

	// List active audio streams.
	inputs, err := listSinkInputs()
	if err != nil {
		log.Printf("%v\n", err)
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}
	if len(inputs) == 0 {
		log.Println("no active audio streams — start playing audio first, then retry")
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}

	// Build picker labels.
	var lines []string
	for _, inp := range inputs {
		app := inp.Properties["application.name"]
		media := inp.Properties["media.name"]
		label := app
		if label == "" {
			label = media
		}
		if label == "" {
			label = fmt.Sprintf("stream #%d", inp.Index)
		}
		lines = append(lines, fmt.Sprintf("%d\t%s — %s", inp.Index, label, media))
	}

	selected, err := fuzzelPick(lines, "Audio stream:")
	if err != nil {
		log.Printf("%v\n", err)
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}
	selectedIdx, err := strconv.Atoi(strings.Fields(selected)[0])
	if err != nil {
		log.Printf("bad selection: %v\n", err)
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}

	// Find the stream.
	var target *pactlSinkInput
	for i := range inputs {
		if inputs[i].Index == selectedIdx {
			target = &inputs[i]
			break
		}
	}
	if target == nil {
		log.Printf("stream #%d not found\n", selectedIdx)
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}

	// Move stream to wcap-sink.
	if err := moveSinkInput(target.Index, wcapIdx); err != nil {
		log.Printf("failed to move stream: %v\n", err)
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}
	log.Printf("routed stream #%d → wcap-sink\n", target.Index)

	// Prepare output file.
	if err := os.MkdirAll(*dir, 0755); err != nil {
		moveSinkInput(target.Index, target.Sink)
		log.Printf("creating output dir: %v\n", err)
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}
	outFile := filepath.Join(*dir, time.Now().Format("2006-01-02_150405")+".mp4")

	// Launch gpu-screen-recorder.
	// -w portal  → xdg-desktop-portal window picker
	// -a         → audio from wcap-sink monitor
	cmd := exec.Command("gpu-screen-recorder",
		"-w", "portal",
		"-a", "wcap-sink.monitor",
		"-f", "60",
		"-o", outFile,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		moveSinkInput(target.Index, target.Sink) // restore on failure
		log.Printf("starting gpu-screen-recorder: %v\n", err)
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}

	// Wait briefly — if the process exits immediately, the user cancelled the
	// portal dialog or something is misconfigured.
	time.Sleep(2 * time.Second)
	if !processAlive(cmd.Process.Pid) {
		moveSinkInput(target.Index, target.Sink)
		log.Println("gpu-screen-recorder exited immediately — portal cancelled?")
		if createdWcapSink {
			unloadModule(wcapModuleID)
		}
		return 1
	}

	state := &State{
		RecorderPID:     cmd.Process.Pid,
		StreamIndex:     target.Index,
		OriginalSink:    target.Sink,
		OutputFile:      outFile,
		WcapSinkModule:  wcapModuleID,
		CreatedWcapSink: createdWcapSink,
	}
	if err := saveState(state); err != nil {
		log.Printf("warning: could not save state: %v\n", err)
	}

	fmt.Printf("recording → %s\n", outFile)
	return 0
}

// ---------------------------------------------------------------------------
// stop
// ---------------------------------------------------------------------------

func runStop(_ []string) int {
	state, err := loadState()
	if err != nil {
		if errors.Is(err, errInvalidState) {
			if err := removeState(); err != nil {
				log.Printf("clearing invalid recording state: %v", err)
			} else {
				log.Println("cleared invalid recording state; recorder and audio state could not be restored")
			}
		} else {
			log.Printf("reading recording state: %v", err)
		}
		return 1
	}

	// Kill loopback if active.
	if state.LoopbackPID > 0 && processAlive(state.LoopbackPID) {
		if p, err := os.FindProcess(state.LoopbackPID); err == nil {
			p.Signal(syscall.SIGTERM)
		}
	}

	// Signal gpu-screen-recorder to finish cleanly.
	if processAlive(state.RecorderPID) {
		p, _ := os.FindProcess(state.RecorderPID)
		p.Signal(syscall.SIGINT)

		// Poll until it exits (up to 10 s).
		for range 20 {
			time.Sleep(500 * time.Millisecond)
			if !processAlive(state.RecorderPID) {
				break
			}
		}
		if processAlive(state.RecorderPID) {
			p.Kill()
		}
	}

	// Restore audio stream (best-effort).
	moveSinkInput(state.StreamIndex, state.OriginalSink)
	if state.CreatedWcapSink {
		unloadModule(state.WcapSinkModule)
	}

	removeState()

	exec.Command("notify-send", "wcap", fmt.Sprintf("Recording saved: %s", state.OutputFile)).Run()
	fmt.Printf("saved: %s\n", state.OutputFile)
	return 0
}

// ---------------------------------------------------------------------------
// monitor
// ---------------------------------------------------------------------------

func runMonitor(_ []string) int {
	state, err := loadState()
	if err != nil {
		log.Println("no active recording — start one first")
		return 1
	}

	// Toggle off if already running.
	if state.LoopbackPID > 0 && processAlive(state.LoopbackPID) {
		p, _ := os.FindProcess(state.LoopbackPID)
		p.Signal(syscall.SIGTERM)
		state.LoopbackPID = 0
		saveState(state)
		fmt.Println("monitor off")
		return 0
	}

	// Start pw-loopback capturing from wcap-sink.
	cmd := exec.Command("pw-loopback", "--capture-props", "node.target=wcap-sink")
	if err := cmd.Start(); err != nil {
		log.Printf("starting pw-loopback: %v\n", err)
		return 1
	}

	state.LoopbackPID = cmd.Process.Pid
	saveState(state)
	fmt.Println("monitor on")
	return 0
}
