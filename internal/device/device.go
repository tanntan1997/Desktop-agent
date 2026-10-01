// Package device defines the platform-neutral device model and the Driver
// interface implemented by the Android (adb) and iOS (go-ios /
// libimobiledevice + WebDriverAgent) backends.
package device

import (
	"context"

	"github.com/ababank/mobile-device-agent/internal/protocol"
)

type Platform string

const (
	Android Platform = "android"
	IOS     Platform = "ios"
)

const (
	StateOnline       = "online"
	StateOffline      = "offline"
	StateUnauthorized = "unauthorized"
)

var (
	ErrNotSupported = &protocol.Error{Code: protocol.CodeNotSupported, Message: "not supported on this platform"}
	ErrToolMissing  = &protocol.Error{Code: protocol.CodeUnavailable, Message: "required host tool not found"}
)

// Info describes one attached device.
type Info struct {
	Serial       string            `json:"serial"`
	Platform     Platform          `json:"platform"`
	State        string            `json:"state"`
	Transport    string            `json:"transport,omitempty"` // usb, tcp, emulator
	Name         string            `json:"name,omitempty"`
	Model        string            `json:"model,omitempty"`
	Manufacturer string            `json:"manufacturer,omitempty"`
	OSVersion    string            `json:"osVersion,omitempty"`
	ScreenWidth  int               `json:"screenWidth,omitempty"`  // pixels
	ScreenHeight int               `json:"screenHeight,omitempty"` // pixels
	Extra        map[string]string `json:"extra,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// Driver is implemented once per platform. Coordinates are always physical
// pixels in the same space as Screenshot, so a click on a rendered screenshot
// maps directly to Tap.
type Driver interface {
	Platform() Platform
	// List returns attached devices with at least Serial and State set.
	List(ctx context.Context) ([]Info, error)
	// Describe returns full details for an online device.
	Describe(ctx context.Context, serial string) (Info, error)

	Screenshot(ctx context.Context, serial string) ([]byte, error) // PNG
	Source(ctx context.Context, serial string) (string, error)     // native UI hierarchy XML
	Tap(ctx context.Context, serial string, x, y int) error
	Swipe(ctx context.Context, serial string, x1, y1, x2, y2, durationMs int) error
	InputText(ctx context.Context, serial, text string) error
	PressKey(ctx context.Context, serial, key string) error

	InstallApp(ctx context.Context, serial, path string) error
	LaunchApp(ctx context.Context, serial, appID string) error
	TerminateApp(ctx context.Context, serial, appID string) error
}

// Attacher is implemented by drivers that need per-device helpers (port
// forwarding, WDA) while a device is connected.
type Attacher interface {
	Attach(info Info)
	Detach(serial string)
}

// Sheller is implemented by drivers that can run a raw shell command.
type Sheller interface {
	Shell(ctx context.Context, serial, command string) (string, error)
}

// EnvProvider lets a driver expose per-device environment variables to test
// jobs (e.g. ANDROID_SERIAL, WDA_URL).
type EnvProvider interface {
	JobEnv(serial string) map[string]string
}
