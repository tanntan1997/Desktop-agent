// Package ios drives iOS devices. Discovery, install and port forwarding use
// go-ios (`ios`, works on macOS and Windows, required for iOS 17+) or
// libimobiledevice as a fallback; UI inspection and input go through
// WebDriverAgent running on the device.
package ios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/ababank/mobile-device-agent/internal/device"
	"github.com/ababank/mobile-device-agent/internal/execx"
	"github.com/ababank/mobile-device-agent/internal/proc"
	"github.com/ababank/mobile-device-agent/internal/protocol"
	"github.com/ababank/mobile-device-agent/internal/wda"
)

var bundleRe = regexp.MustCompile(`^[A-Za-z0-9.\-]+$`)

type Options struct {
	Backend               string // auto | go-ios | libimobiledevice
	GoIOSPath             string
	StartTunnel           bool // run `ios tunnel start --userspace` (iOS 17+)
	WDABasePort           int  // first host port used for per-device WDA forwarding
	WDADevicePort         int  // WDA port on the device (8100)
	WDABundleID           string
	WDATestRunnerBundleID string
	WDAXCTestConfig       string
	WDAProjectPath        string // macOS only: WebDriverAgent.xcodeproj, launched via xcodebuild
}

type libTools struct{ id, info, shot, installer, iproxy string }

type devState struct {
	cancel context.CancelFunc
	wda    *wda.Client

	mu    sync.Mutex
	scale float64 // screenshot pixels per WDA point
}

type Driver struct {
	opt   Options
	log   *slog.Logger
	root  context.Context
	goios string
	lib   libTools

	mu       sync.Mutex
	ports    map[string]int
	nextPort int
	devs     map[string]*devState
}

// New detects the available tooling. root bounds the lifetime of helper
// processes started for attached devices.
func New(root context.Context, opt Options, log *slog.Logger) *Driver {
	if opt.WDABasePort == 0 {
		opt.WDABasePort = 8100
	}
	if opt.WDADevicePort == 0 {
		opt.WDADevicePort = 8100
	}
	if opt.WDATestRunnerBundleID == "" {
		opt.WDATestRunnerBundleID = opt.WDABundleID
	}
	if opt.WDAXCTestConfig == "" {
		opt.WDAXCTestConfig = "WebDriverAgentRunner.xctest"
	}
	d := &Driver{opt: opt, log: log, root: root, ports: map[string]int{}, devs: map[string]*devState{}, nextPort: opt.WDABasePort}

	if opt.Backend != "libimobiledevice" {
		d.goios = execx.Lookup(opt.GoIOSPath, "ios", "go-ios")
	}
	if opt.Backend != "go-ios" && d.goios == "" {
		d.lib = libTools{
			id:        execx.Lookup("idevice_id"),
			info:      execx.Lookup("ideviceinfo"),
			shot:      execx.Lookup("idevicescreenshot"),
			installer: execx.Lookup("ideviceinstaller"),
			iproxy:    execx.Lookup("iproxy"),
		}
	}
	switch {
	case d.goios != "":
		log.Info("ios backend ready", "backend", "go-ios", "path", d.goios)
		if opt.StartTunnel {
			s := &proc.Supervisor{Name: "ios-tunnel", Path: d.goios, Args: []string{"tunnel", "start", "--userspace"}, Log: log}
			go s.Run(root)
		}
	case d.lib.id != "":
		log.Info("ios backend ready", "backend", "libimobiledevice", "idevice_id", d.lib.id, "iproxy", d.lib.iproxy)
	default:
		log.Warn("neither go-ios nor libimobiledevice found; iOS support disabled")
	}
	return d
}

func (d *Driver) Platform() device.Platform { return device.IOS }

func (d *Driver) Backend() string {
	switch {
	case d.goios != "":
		return "go-ios"
	case d.lib.id != "":
		return "libimobiledevice"
	}
	return ""
}

func (d *Driver) List(ctx context.Context) ([]device.Info, error) {
	var udids []string
	switch {
	case d.goios != "":
		out, err := execx.Output(ctx, d.goios, "list")
		if err != nil {
			return nil, err
		}
		var r struct {
			DeviceList []string `json:"deviceList"`
		}
		if err := json.Unmarshal(jsonLine(out), &r); err != nil {
			return nil, fmt.Errorf("ios list: %w", err)
		}
		udids = r.DeviceList
	case d.lib.id != "":
		out, err := execx.Output(ctx, d.lib.id, "-l")
		if err != nil {
			return nil, err
		}
		udids = strings.Fields(string(out))
	default:
		return nil, fmt.Errorf("%w: go-ios or libimobiledevice", device.ErrToolMissing)
	}
	list := make([]device.Info, 0, len(udids))
	for _, u := range udids {
		list = append(list, device.Info{Serial: u, Platform: device.IOS, State: device.StateOnline, Transport: "usb"})
	}
	return list, nil
}

func (d *Driver) Describe(ctx context.Context, udid string) (device.Info, error) {
	vals := map[string]string{}
	switch {
	case d.goios != "":
		out, err := execx.Output(ctx, d.goios, "info", "--udid="+udid)
		if err != nil {
			return device.Info{}, err
		}
		var m map[string]any
		if err := json.Unmarshal(jsonLine(out), &m); err != nil {
			return device.Info{}, fmt.Errorf("ios info: %w", err)
		}
		for k, v := range m {
			vals[k] = fmt.Sprint(v)
		}
	case d.lib.info != "":
		out, err := execx.Output(ctx, d.lib.info, "-u", udid)
		if err != nil {
			return device.Info{}, fmt.Errorf("%w (is the device unlocked and trusted?)", err)
		}
		for _, l := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok {
				vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return device.Info{
		Serial:       udid,
		Platform:     device.IOS,
		Name:         vals["DeviceName"],
		Model:        vals["ProductType"],
		Manufacturer: "Apple",
		OSVersion:    vals["ProductVersion"],
		Extra:        map[string]string{"wdaUrl": d.wdaURL(udid), "backend": d.Backend()},
	}, nil
}

func (d *Driver) JobEnv(udid string) map[string]string {
	return map[string]string{"IOS_UDID": udid, "WDA_URL": d.wdaURL(udid)}
}

func (d *Driver) wdaURL(udid string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d", d.portLocked(udid))
}

// portLocked gives each UDID a stable host port for the life of the agent.
func (d *Driver) portLocked(udid string) int {
	if p, ok := d.ports[udid]; ok {
		return p
	}
	p := d.nextPort
	d.nextPort++
	d.ports[udid] = p
	return p
}

// Attach starts port forwarding to WDA and, when configured, launches WDA.
func (d *Driver) Attach(info device.Info) {
	udid := info.Serial
	d.mu.Lock()
	if _, ok := d.devs[udid]; ok {
		d.mu.Unlock()
		return
	}
	port := d.portLocked(udid)
	ctx, cancel := context.WithCancel(d.root)
	d.devs[udid] = &devState{cancel: cancel, wda: wda.New(fmt.Sprintf("http://127.0.0.1:%d", port))}
	d.mu.Unlock()

	devPort := strconv.Itoa(d.opt.WDADevicePort)
	switch {
	case d.goios != "":
		s := &proc.Supervisor{Name: "forward-" + udid, Path: d.goios,
			Args: []string{"forward", "--udid=" + udid, strconv.Itoa(port), devPort}, Log: d.log}
		go s.Run(ctx)
	case d.lib.iproxy != "":
		s := &proc.Supervisor{Name: "iproxy-" + udid, Path: d.lib.iproxy,
			Args: []string{fmt.Sprintf("%d:%s", port, devPort), "-u", udid}, Log: d.log}
		go s.Run(ctx)
	default:
		d.log.Warn("no port forwarder available; WDA unreachable", "udid", udid)
	}

	switch {
	case d.goios != "" && d.opt.WDABundleID != "":
		s := &proc.Supervisor{Name: "wda-" + udid, Path: d.goios, Log: d.log, Args: []string{
			"runwda",
			"--bundleid=" + d.opt.WDABundleID,
			"--testrunnerbundleid=" + d.opt.WDATestRunnerBundleID,
			"--xctestconfig=" + d.opt.WDAXCTestConfig,
			"--udid=" + udid,
		}}
		go s.Run(ctx)
	case runtime.GOOS == "darwin" && d.opt.WDAProjectPath != "":
		s := &proc.Supervisor{Name: "wda-" + udid, Path: "xcodebuild", Log: d.log, Args: []string{
			"-project", d.opt.WDAProjectPath,
			"-scheme", "WebDriverAgentRunner",
			"-destination", "id=" + udid,
			"-allowProvisioningUpdates",
			"test",
		}}
		go s.Run(ctx)
	}
	d.log.Info("ios device attached", "udid", udid, "wda", fmt.Sprintf("http://127.0.0.1:%d", port))
}

func (d *Driver) Detach(udid string) {
	d.mu.Lock()
	st, ok := d.devs[udid]
	delete(d.devs, udid)
	d.mu.Unlock()
	if ok {
		st.cancel()
	}
}

// WDA returns the WDA client for a device, verifying it responds.
func (d *Driver) WDA(ctx context.Context, udid string) (*wda.Client, error) {
	st, err := d.state(ctx, udid)
	if err != nil {
		return nil, err
	}
	return st.wda, nil
}

func (d *Driver) state(ctx context.Context, udid string) (*devState, error) {
	d.mu.Lock()
	st, ok := d.devs[udid]
	d.mu.Unlock()
	if !ok {
		return nil, protocol.Errorf(protocol.CodeNotFound, "ios device %q not attached", udid)
	}
	if err := st.wda.Status(ctx); err != nil {
		return nil, protocol.Errorf(protocol.CodeUnavailable,
			"WebDriverAgent not reachable at %s (start WDA on the device or set ios.wdaBundleId): %v", st.wda.BaseURL, err)
	}
	return st, nil
}

// scale converts screenshot pixels to WDA points, computed once per device.
func (d *Driver) scale(ctx context.Context, st *devState) float64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.scale > 0 {
		return st.scale
	}
	shot, err := st.wda.Screenshot(ctx)
	if err != nil {
		return 1
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(shot))
	if err != nil {
		return 1
	}
	w, _, err := st.wda.WindowSize(ctx)
	if err != nil || w <= 0 {
		return 1
	}
	st.scale = float64(cfg.Width) / w
	return st.scale
}

func (d *Driver) Screenshot(ctx context.Context, udid string) ([]byte, error) {
	st, wdaErr := d.state(ctx, udid)
	if wdaErr == nil {
		if img, err := st.wda.Screenshot(ctx); err == nil {
			return img, nil
		} else {
			wdaErr = err
		}
	}
	// Fall back to the screenshot service (needs a mounted developer image).
	tmp, err := os.CreateTemp("", "mda-shot-*.png")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	switch {
	case d.goios != "":
		_, err = execx.Output(ctx, d.goios, "screenshot", "--udid="+udid, "--output="+tmp.Name())
	case d.lib.shot != "":
		_, err = execx.Output(ctx, d.lib.shot, "-u", udid, tmp.Name())
	default:
		return nil, wdaErr
	}
	if err != nil {
		return nil, fmt.Errorf("screenshot via WDA failed (%v) and via CLI failed (%w)", wdaErr, err)
	}
	return os.ReadFile(tmp.Name())
}

func (d *Driver) Source(ctx context.Context, udid string) (string, error) {
	st, err := d.state(ctx, udid)
	if err != nil {
		return "", err
	}
	return st.wda.Source(ctx)
}

func (d *Driver) Tap(ctx context.Context, udid string, x, y int) error {
	st, err := d.state(ctx, udid)
	if err != nil {
		return err
	}
	s := d.scale(ctx, st)
	return st.wda.Tap(ctx, float64(x)/s, float64(y)/s)
}

func (d *Driver) Swipe(ctx context.Context, udid string, x1, y1, x2, y2, durationMs int) error {
	st, err := d.state(ctx, udid)
	if err != nil {
		return err
	}
	if durationMs <= 0 {
		durationMs = 300
	}
	s := d.scale(ctx, st)
	return st.wda.Swipe(ctx, float64(x1)/s, float64(y1)/s, float64(x2)/s, float64(y2)/s, durationMs)
}

func (d *Driver) InputText(ctx context.Context, udid, text string) error {
	st, err := d.state(ctx, udid)
	if err != nil {
		return err
	}
	return st.wda.Type(ctx, text)
}

func (d *Driver) PressKey(ctx context.Context, udid, key string) error {
	st, err := d.state(ctx, udid)
	if err != nil {
		return err
	}
	switch strings.ToLower(key) {
	case "home":
		return st.wda.Home(ctx)
	case "volume_up":
		return st.wda.PressButton(ctx, "volumeUp")
	case "volume_down":
		return st.wda.PressButton(ctx, "volumeDown")
	case "enter":
		return st.wda.Type(ctx, "\n")
	case "delete":
		return st.wda.Type(ctx, "\b")
	}
	return fmt.Errorf("%w: key %q on iOS (supported: home, volume_up, volume_down, enter, delete)", device.ErrNotSupported, key)
}

func (d *Driver) InstallApp(ctx context.Context, udid, path string) error {
	switch {
	case d.goios != "":
		_, err := execx.Output(ctx, d.goios, "install", "--path="+path, "--udid="+udid)
		return err
	case d.lib.installer != "":
		_, err := execx.Output(ctx, d.lib.installer, "-u", udid, "-i", path)
		return err
	}
	return fmt.Errorf("%w: go-ios or ideviceinstaller", device.ErrToolMissing)
}

func (d *Driver) LaunchApp(ctx context.Context, udid, bundleID string) error {
	if !bundleRe.MatchString(bundleID) {
		return protocol.Errorf(protocol.CodeBadRequest, "invalid bundle id %q", bundleID)
	}
	if st, err := d.state(ctx, udid); err == nil {
		return st.wda.Launch(ctx, bundleID)
	} else if d.goios == "" {
		return err
	}
	_, err := execx.Output(ctx, d.goios, "launch", bundleID, "--udid="+udid)
	return err
}

func (d *Driver) TerminateApp(ctx context.Context, udid, bundleID string) error {
	if !bundleRe.MatchString(bundleID) {
		return protocol.Errorf(protocol.CodeBadRequest, "invalid bundle id %q", bundleID)
	}
	if st, err := d.state(ctx, udid); err == nil {
		return st.wda.Terminate(ctx, bundleID)
	} else if d.goios == "" {
		return err
	}
	_, err := execx.Output(ctx, d.goios, "kill", bundleID, "--udid="+udid)
	return err
}

// jsonLine returns the last line of out that looks like a JSON object; go-ios
// may interleave log lines with its JSON result.
func jsonLine(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if l := bytes.TrimSpace(lines[i]); len(l) > 0 && l[0] == '{' {
			return l
		}
	}
	return out
}
