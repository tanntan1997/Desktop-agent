// Command server is the web server that device agents connect to. Agents dial
// /agent over WebSocket; people and CI use the REST API under /api and the
// dashboard at /.
//
//	device-hub -agent-token AGENT_SECRET -api-token API_SECRET
//	device-agent -server ws://hub:8080/agent -server-token AGENT_SECRET
//	curl -H "Authorization: Bearer API_SECRET" hub:8080/api/devices
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/openai/openai-go/v3/option"

	"github.com/ababank/mobile-device-agent/internal/autotest"
	"github.com/ababank/mobile-device-agent/internal/hub"
	"github.com/ababank/mobile-device-agent/internal/server"
)

var version = "dev"

type config struct {
	Listen         string            `json:"listen"`
	TLSCert        string            `json:"tlsCert"`
	TLSKey         string            `json:"tlsKey"`
	AgentToken     string            `json:"agentToken"`  // shared by all agents
	AgentTokens    map[string]string `json:"agentTokens"` // agent ID -> token
	APITokens      []string          `json:"apiTokens"`
	AllowedOrigins []string          `json:"allowedOrigins"`
	LogLevel       string            `json:"logLevel"`
	DataDir        string            `json:"dataDir"` // tests and run results
	AI             aiConfig          `json:"ai"`
}

type aiConfig struct {
	Disabled bool   `json:"disabled"`
	Model    string `json:"model"`    // default gpt-5.5
	Effort   string `json:"effort"`   // reasoning effort: none..xhigh, "off", or empty for auto
	BaseURL  string `json:"baseUrl"`  // OpenAI-compatible endpoint (Azure, gateway)
	MaxTurns int    `json:"maxTurns"` // model requests per exploration
}

func main() {
	var (
		configPath  = flag.String("config", "", "JSON config file")
		listen      = flag.String("listen", "", "listen address (default :8080)")
		tlsCert     = flag.String("tls-cert", "", "TLS certificate file (serves HTTPS/WSS)")
		tlsKey      = flag.String("tls-key", "", "TLS key file")
		agentToken  = flag.String("agent-token", "", "token every agent must present")
		apiToken    = flag.String("api-token", "", "token for the REST API and dashboard (generated if unset)")
		logLevel    = flag.String("log-level", "", "debug | info | warn | error")
		insecure    = flag.Bool("insecure", false, "allow agents and API clients without tokens (local testing only)")
		dataDir     = flag.String("data", "", "directory for tests and run results (default ./hub-data)")
		aiModel     = flag.String("ai-model", "", "OpenAI model for test exploration (default "+autotest.DefaultModel+")")
		aiEffort    = flag.String("ai-effort", "", "reasoning effort for reasoning models: none | minimal | low | medium | high | xhigh, or off (default: medium for gpt-5/o-series, omitted otherwise)")
		aiBaseURL   = flag.String("ai-base-url", "", "OpenAI-compatible API base URL (default OPENAI_BASE_URL or api.openai.com)")
		noAI        = flag.Bool("no-ai", false, "disable AI test exploration (replay still works)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg := config{Listen: ":8080", LogLevel: "info", DataDir: "hub-data"}
	if *configPath != "" {
		b, err := os.ReadFile(*configPath)
		if err != nil {
			fatal(err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			fatal(fmt.Errorf("%s: %w", *configPath, err))
		}
	}
	// Precedence: flag > env > config file.
	override(&cfg.Listen, os.Getenv("HUB_LISTEN"), *listen)
	override(&cfg.TLSCert, os.Getenv("HUB_TLS_CERT"), *tlsCert)
	override(&cfg.TLSKey, os.Getenv("HUB_TLS_KEY"), *tlsKey)
	override(&cfg.AgentToken, os.Getenv("HUB_AGENT_TOKEN"), *agentToken)
	override(&cfg.LogLevel, os.Getenv("HUB_LOG_LEVEL"), *logLevel)
	override(&cfg.DataDir, os.Getenv("HUB_DATA_DIR"), *dataDir)
	override(&cfg.AI.Model, os.Getenv("HUB_AI_MODEL"), *aiModel)
	override(&cfg.AI.Effort, os.Getenv("HUB_AI_EFFORT"), *aiEffort)
	override(&cfg.AI.BaseURL, *aiBaseURL)
	cfg.AI.Disabled = cfg.AI.Disabled || *noAI
	for _, t := range []string{os.Getenv("HUB_API_TOKEN"), *apiToken} {
		if t != "" {
			cfg.APITokens = append(cfg.APITokens, t)
		}
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		fatal(fmt.Errorf("log level: %w", err))
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		fatal(errors.New("tls-cert and tls-key must be set together"))
	}
	if *insecure {
		log.Warn("running with -insecure: any agent may connect and the API is unauthenticated")
		cfg.AgentToken, cfg.AgentTokens, cfg.APITokens = "", nil, nil
	} else {
		if cfg.AgentToken == "" && len(cfg.AgentTokens) == 0 {
			fatal(errors.New("no agent token configured: set -agent-token, HUB_AGENT_TOKEN or agentTokens (or -insecure for local testing)"))
		}
		if len(cfg.APITokens) == 0 {
			t := randomToken()
			cfg.APITokens = []string{t}
			log.Warn("no API token configured; generated one for this run", "apiToken", t)
		}
	}

	h := hub.New(log)
	h.SharedToken = cfg.AgentToken
	h.AgentTokens = cfg.AgentTokens
	store, err := autotest.OpenStore(cfg.DataDir)
	if err != nil {
		fatal(fmt.Errorf("data dir: %w", err))
	}
	var llm autotest.LLM
	if !cfg.AI.Disabled {
		var opts []option.RequestOption
		if cfg.AI.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(cfg.AI.BaseURL))
		}
		llm = autotest.NewOpenAILLM(opts...)
		if os.Getenv("OPENAI_API_KEY") == "" {
			log.Warn("OPENAI_API_KEY is not set; AI exploration will fail until it is")
		}
	}
	tests := autotest.NewService(h, store, llm, autotest.ExploreConfig{
		Model: cfg.AI.Model, Effort: cfg.AI.Effort, MaxTurns: cfg.AI.MaxTurns}, log)
	log.Info("AI tests", "dataDir", cfg.DataDir, "enabled", llm != nil, "model", tests.Config.Model, "effort", tests.Config.Effort)
	srv := &server.Server{Hub: h, APITokens: cfg.APITokens, AllowedOrigins: cfg.AllowedOrigins, AutoTest: tests, Log: log}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: SSE streams and long-running RPCs stay open.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
	}()

	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	log.Info("device hub listening", "addr", cfg.Listen, "scheme", scheme, "version", version)
	if cfg.TLSCert != "" {
		err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
	log.Info("device hub stopped")
}

// override sets *dst to the last non-empty value.
func override(dst *string, vals ...string) {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "device-hub:", err)
	os.Exit(1)
}
