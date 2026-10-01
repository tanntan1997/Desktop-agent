// Package android drives Android devices through the adb CLI.
package android

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/ababank/mobile-device-agent/internal/device"
	"github.com/ababank/mobile-device-agent/internal/execx"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

const dumpPath = "/data/local/tmp/mda_window_dump.xml"

var (
	packageRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)
	componentRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]*/[A-Za-z0-9_.$]+$`)
	pngMagic    = []byte("\x89PNG\r\n\x1a\n")
)

var keyCodes = map[string]int{
	"home": 3, "back": 4, "call": 5, "endcall": 6,
	"volume_up": 24, "volume_down": 25, "power": 26, "camera": 27,
	"tab": 61, "enter": 66, "delete": 67, "menu": 82, "search": 84,
	"app_switch": 187, "wakeup": 224, "sleep": 223,
}

type Driver struct {
	adb string
	log *slog.Logger

	dumpLocks sync.Map // serial -> *sync.Mutex; uiautomator can't run twice at once
}

// New returns an adb-backed driver. adbPath may be empty to auto-detect.
func New(adbPath string, log *slog.Logger) *Driver {
	if adbPath == "" {
		adbPath = findADB()
	}
	if adbPath == "" {
		log.Warn("adb not found; Android support disabled (install platform-tools or set adbPath)")
	} else {
		log.Info("android backend ready", "adb", adbPath)
	}
	return &Driver{adb: adbPath, log: log}
}

func (d *Driver) Path() string { return d.adb }

func (d *Driver) Platform() device.Platform { return device.Android }

func (d *Driver) run(ctx context.Context, serial string, args ...string) ([]byte, error) {
	if d.adb == "" {
		return nil, fmt.Errorf("%w: adb", device.ErrToolMissing)
	}
	return execx.Output(ctx, d.adb, append([]string{"-s", serial}, args...)...)
}

// shell runs a device shell command line. The string is interpreted by the
// device's sh, so callers must quote untrusted parts with shellQuote.
func (d *Driver) shell(ctx context.Context, serial, cmdline string) (string, error) {
	out, err := d.run(ctx, serial, "shell", cmdline)
	return strings.ReplaceAll(string(out), "\r\n", "\n"), err
}

func (d *Driver) List(ctx context.Context) ([]device.Info, error) {
	if d.adb == "" {
		return nil, fmt.Errorf("%w: adb", device.ErrToolMissing)
	}
	out, err := execx.Output(ctx, d.adb, "devices", "-l")
	if err != nil {
		return nil, err
	}
	return parseDevices(string(out)), nil
}

func parseDevices(out string) []device.Info {
	var list []device.Info
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "List of devices") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		info := device.Info{Serial: f[0], Platform: device.Android, State: device.StateOffline}
		switch f[1] {
		case "device":
			info.State = device.StateOnline
		case "unauthorized":
			info.State = device.StateUnauthorized
			info.Error = "USB debugging not authorized: accept the RSA prompt on the device"
		}
		switch {
		case strings.HasPrefix(info.Serial, "emulator-"):
			info.Transport = "emulator"
		case strings.Contains(line, " usb:"):
			info.Transport = "usb"
		case strings.Contains(info.Serial, ":"):
			info.Transport = "tcp"
		}
		for _, kv := range f[2:] {
			if v, ok := strings.CutPrefix(kv, "model:"); ok {
				info.Model = strings.ReplaceAll(v, "_", " ")
			}
		}
		list = append(list, info)
	}
	return list
}

func (d *Driver) Describe(ctx context.Context, serial string) (device.Info, error) {
	out, err := d.shell(ctx, serial,
		"getprop ro.product.model; getprop ro.product.manufacturer; getprop ro.build.version.release; getprop ro.build.version.sdk; wm size")
	if err != nil {
		return device.Info{}, err
	}
	lines := strings.Split(out, "\n")
	for len(lines) < 5 {
		lines = append(lines, "")
	}
	info := device.Info{
		Serial:       serial,
		Platform:     device.Android,
		Model:        strings.TrimSpace(lines[0]),
		Manufacturer: strings.TrimSpace(lines[1]),
		OSVersion:    strings.TrimSpace(lines[2]),
		Extra:        map[string]string{"sdk": strings.TrimSpace(lines[3])},
	}
	info.Name = strings.TrimSpace(info.Manufacturer + " " + info.Model)
	// "Physical size: 1080x2340" optionally followed by "Override size: ...";
	// the override is what the screen actually renders at.
	for _, l := range lines[4:] {
		if _, v, ok := strings.Cut(l, "size:"); ok {
			if w, h, ok := strings.Cut(strings.TrimSpace(v), "x"); ok {
				info.ScreenWidth, _ = strconv.Atoi(w)
				info.ScreenHeight, _ = strconv.Atoi(h)
			}
		}
	}
	return info, nil
}

func (d *Driver) JobEnv(serial string) map[string]string {
	return map[string]string{"ANDROID_SERIAL": serial, "ADB_PATH": d.adb}
}

func (d *Driver) Screenshot(ctx context.Context, serial string) ([]byte, error) {
	png, err := d.run(ctx, serial, "exec-out", "screencap", "-p")
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(png, pngMagic) {
		return nil, fmt.Errorf("screencap returned non-PNG data (%d bytes): %.120q", len(png), png)
	}
	return png, nil
}

func (d *Driver) Source(ctx context.Context, serial string) (string, error) {
	mu, _ := d.dumpLocks.LoadOrStore(serial, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	msg, err := d.shell(ctx, serial, "uiautomator dump "+dumpPath)
	if err != nil {
		return "", err
	}
	xml, err := d.run(ctx, serial, "exec-out", "cat", dumpPath)
	if err != nil {
		return "", err
	}
	if !bytes.Contains(xml, []byte("<hierarchy")) {
		// Typical causes: Appium's UiAutomator2 server holds the accessibility
		// connection, or the screen never goes idle (animations).
		return "", fmt.Errorf("uiautomator dump failed: %s", strings.TrimSpace(msg))
	}
	return string(xml), nil
}

func (d *Driver) Tap(ctx context.Context, serial string, x, y int) error {
	_, err := d.shell(ctx, serial, fmt.Sprintf("input tap %d %d", x, y))
	return err
}

func (d *Driver) Swipe(ctx context.Context, serial string, x1, y1, x2, y2, durationMs int) error {
	if durationMs <= 0 {
		durationMs = 300
	}
	_, err := d.shell(ctx, serial, fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, durationMs))
	return err
}

func (d *Driver) InputText(ctx context.Context, serial, text string) error {
	for _, r := range text {
		if r > 0x7e || (r < 0x20 && r != '\n') {
			return protocol.Errorf(protocol.CodeBadRequest,
				"adb input text supports printable ASCII only; use Appium for unicode input")
		}
	}
	// Newlines become ENTER presses between chunks.
	for i, chunk := range strings.Split(text, "\n") {
		if i > 0 {
			if err := d.PressKey(ctx, serial, "enter"); err != nil {
				return err
			}
		}
		if chunk == "" {
			continue
		}
		if _, err := d.shell(ctx, serial, "input text "+shellQuote(strings.ReplaceAll(chunk, " ", "%s"))); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) PressKey(ctx context.Context, serial, key string) error {
	code, ok := keyCodes[strings.ToLower(key)]
	if !ok {
		n, err := strconv.Atoi(key)
		if err != nil {
			return protocol.Errorf(protocol.CodeBadRequest, "unknown key %q", key)
		}
		code = n
	}
	_, err := d.shell(ctx, serial, fmt.Sprintf("input keyevent %d", code))
	return err
}

func (d *Driver) InstallApp(ctx context.Context, serial, path string) error {
	out, err := d.run(ctx, serial, "install", "-r", "-g", path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "Success") {
		return fmt.Errorf("adb install: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Driver) LaunchApp(ctx context.Context, serial, appID string) error {
	var cmdline string
	switch {
	case componentRe.MatchString(appID):
		cmdline = "am start -n " + appID
	case packageRe.MatchString(appID):
		cmdline = "monkey -p " + appID + " -c android.intent.category.LAUNCHER 1"
	default:
		return protocol.Errorf(protocol.CodeBadRequest, "invalid package or component %q", appID)
	}
	out, err := d.shell(ctx, serial, cmdline)
	if err != nil {
		return err
	}
	if strings.Contains(out, "No activities found") || strings.Contains(out, "Error:") {
		return fmt.Errorf("launch %s: %s", appID, strings.TrimSpace(out))
	}
	return nil
}

func (d *Driver) TerminateApp(ctx context.Context, serial, appID string) error {
	if !packageRe.MatchString(appID) {
		return protocol.Errorf(protocol.CodeBadRequest, "invalid package %q", appID)
	}
	_, err := d.shell(ctx, serial, "am force-stop "+appID)
	return err
}

func (d *Driver) Shell(ctx context.Context, serial, command string) (string, error) {
	return d.shell(ctx, serial, command)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func findADB() string {
	if p := execx.Lookup("adb"); p != "" {
		return p
	}
	exe := "adb"
	if runtime.GOOS == "windows" {
		exe = "adb.exe"
	}
	var roots []string
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(env); v != "" {
			roots = append(roots, v)
		}
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		roots = append(roots, filepath.Join(home, "Library", "Android", "sdk"))
	case "windows":
		roots = append(roots, filepath.Join(os.Getenv("LOCALAPPDATA"), "Android", "Sdk"))
	default:
		roots = append(roots, filepath.Join(home, "Android", "Sdk"))
	}
	for _, r := range roots {
		p := filepath.Join(r, "platform-tools", exe)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
