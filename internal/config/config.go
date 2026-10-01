// Package config loads agent settings from defaults, a JSON file, and
// DA_* environment variables (in that order of precedence, lowest first).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Runner is an allow-listed test command the server may launch via job.start.
type Runner struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	WorkDir string            `json:"workDir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type IOS struct {
	Backend               string `json:"backend"` // auto | go-ios | libimobiledevice
	GoIOSPath             string `json:"goIosPath,omitempty"`
	StartTunnel           bool   `json:"startTunnel"`
	WDABasePort           int    `json:"wdaBasePort"`
	WDADevicePort         int    `json:"wdaDevicePort"`
	WDABundleID           string `json:"wdaBundleId,omitempty"`
	WDATestRunnerBundleID string `json:"wdaTestRunnerBundleId,omitempty"`
	WDAXCTestConfig       string `json:"wdaXctestConfig,omitempty"`
	WDAProjectPath        string `json:"wdaProjectPath,omitempty"`
}

type Appium struct {
	URL       string   `json:"url"`
	AutoStart bool     `json:"autoStart"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
}

type Config struct {
	AgentName             string            `json:"agentName"`
	ServerURL             string            `json:"serverUrl,omitempty"`
	ServerToken           string            `json:"serverToken,omitempty"`
	ListenAddr            string            `json:"listenAddr"`
	APIToken              string            `json:"apiToken,omitempty"`
	AllowedOrigins        []string          `json:"allowedOrigins,omitempty"`
	ADBPath               string            `json:"adbPath,omitempty"`
	PollIntervalSec       int               `json:"pollIntervalSec"`
	MaxConcurrentRequests int               `json:"maxConcurrentRequests"`
	AllowShell            bool              `json:"allowShell"`
	DataDir               string            `json:"dataDir,omitempty"`
	LogLevel              string            `json:"logLevel"`
	IOS                   IOS               `json:"ios"`
	Appium                Appium            `json:"appium"`
	Runners               map[string]Runner `json:"runners,omitempty"`
}

// DefaultDir is where config.json, the agent id and the API token live.
func DefaultDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "mobile-device-agent")
}

func DefaultPath() string { return filepath.Join(DefaultDir(), "config.json") }

func Default() Config {
	host, _ := os.Hostname()
	return Config{
		AgentName:             host,
		ListenAddr:            "127.0.0.1:7100",
		PollIntervalSec:       2,
		MaxConcurrentRequests: 16,
		LogLevel:              "info",
		DataDir:               DefaultDir(),
		IOS:                   IOS{Backend: "auto", WDABasePort: 8100, WDADevicePort: 8100},
		Appium:                Appium{URL: "http://127.0.0.1:4723", Command: "appium"},
	}
}

// Load reads path (a missing file at the default path is not an error) and
// applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	explicit := path != ""
	if !explicit {
		path = DefaultPath()
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
	default:
		return cfg, err
	}
	applyEnv(&cfg)
	return cfg, nil
}

func applyEnv(c *Config) {
	set := func(dst *string, key string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	set(&c.AgentName, "DA_AGENT_NAME")
	set(&c.ServerURL, "DA_SERVER_URL")
	set(&c.ServerToken, "DA_SERVER_TOKEN")
	set(&c.ListenAddr, "DA_LISTEN")
	set(&c.APIToken, "DA_API_TOKEN")
	set(&c.ADBPath, "DA_ADB_PATH")
	set(&c.LogLevel, "DA_LOG_LEVEL")
	set(&c.Appium.URL, "DA_APPIUM_URL")
}

func (c *Config) Validate() error {
	if c.ServerURL != "" {
		u, err := url.Parse(c.ServerURL)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
			return fmt.Errorf("serverUrl must be a ws:// or wss:// URL, got %q", c.ServerURL)
		}
		if u.Scheme == "ws" && !isLoopback(u.Hostname()) {
			fmt.Fprintln(os.Stderr, "WARNING: serverUrl uses plaintext ws:// to a non-local host; use wss:// in production")
		}
	}
	if c.PollIntervalSec <= 0 {
		c.PollIntervalSec = 2
	}
	if c.MaxConcurrentRequests <= 0 {
		c.MaxConcurrentRequests = 16
	}
	for name, r := range c.Runners {
		if strings.TrimSpace(r.Command) == "" {
			return fmt.Errorf("runner %q has no command", name)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// EnsureSecret returns the contents of dir/name, generating a random value on
// first use. Used for the persistent agent id and the local API token.
func EnsureSecret(dir, name string, nbytes int) (string, error) {
	p := filepath.Join(dir, name)
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	}
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := hex.EncodeToString(buf)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return s, os.WriteFile(p, []byte(s+"\n"), 0o600)
}

// Example is written by `agent -init`.
func Example() Config {
	c := Default()
	c.ServerURL = "wss://device-hub.example.com/agent"
	c.ServerToken = "replace-with-server-issued-token"
	c.Runners = map[string]Runner{
		"pytest": {Command: "pytest", WorkDir: "/path/to/mbb-appium-e2e"},
		"patrol": {Command: "patrol", Args: []string{"test"}, WorkDir: "/path/to/flutter-app"},
	}
	c.IOS.WDABundleID = "com.facebook.WebDriverAgentRunner.xctrunner"
	return c
}
