package android

import (
	"testing"

	"github.com/ababank/mobile-device-agent/internal/device"
)

func TestParseDevices(t *testing.T) {
	out := "* daemon not running; starting now at tcp:5037\n" +
		"* daemon started successfully\n" +
		"List of devices attached\n" +
		"R5CWC3QFR0B            device usb:20-3 product:e2sxxx model:SM_S926B device:e2s transport_id:1\r\n" +
		"emulator-5554          device product:sdk_gphone64 model:sdk_gphone64_x86_64 transport_id:2\n" +
		"ZY22ABC                unauthorized usb:1-1 transport_id:3\n" +
		"192.168.1.20:5555      offline\n\n"
	got := parseDevices(out)
	want := []struct {
		serial, state, transport, model string
	}{
		{"R5CWC3QFR0B", device.StateOnline, "usb", "SM S926B"},
		{"emulator-5554", device.StateOnline, "emulator", "sdk gphone64 x86 64"},
		{"ZY22ABC", device.StateUnauthorized, "usb", ""},
		{"192.168.1.20:5555", device.StateOffline, "tcp", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d devices, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Serial != w.serial || g.State != w.state || g.Transport != w.transport || g.Model != w.model {
			t.Errorf("device %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"hello":      "'hello'",
		"it's":       `'it'\''s'`,
		"a;rm -rf /": "'a;rm -rf /'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppIDValidation(t *testing.T) {
	for _, s := range []string{"com.ababank.mobile", "com.example.app_dev"} {
		if !packageRe.MatchString(s) {
			t.Errorf("package %q rejected", s)
		}
	}
	for _, s := range []string{"com.x;reboot", "nodots", "com.x app", "$(id)"} {
		if packageRe.MatchString(s) {
			t.Errorf("package %q accepted", s)
		}
	}
	if !componentRe.MatchString("com.ababank.mobile/.MainActivity") {
		t.Error("component rejected")
	}
}
