// Command ai-coup is the composition root of spec §2: it parses the CLOSED
// configuration surface (§1.1 — env vars with flag parity, flag wins), selects
// the slog handler (§1/§12: text|json), wires every port to its concrete
// adapter (the only place that happens), and runs the HTTP server on COUP_PORT
// (default :8090). Port 8080 is a startup error, not a runtime surprise: a
// llama-server owns it in dev (§1) and must never be disturbed. The only
// third-party dependency is chi (§1).
//
// The MoveDriver is selected by COUP_DRIVER_MODE (default "heuristic"): the
// built-in deterministic heuristic driver (A7, zero external services) or the
// OpenAI-compatible tool-calling driver (§6.4; requires COUP_LLM_MODEL — A6
// fail-fast above). Both claim LLMCapable, so games with LLM seats run
// unattended end-to-end.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	httpserver "forgejo.lan/mrhollen/ai-coup/infrastructure/http"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/store"
	clock "forgejo.lan/mrhollen/ai-coup/infrastructure/time"
)

// portForbidden is the §1 bind prohibition: this system must never listen on
// 8080 (a llama-server owns it in dev). Defense in depth — startup fails
// fast rather than killing or shadowing another process.
const portForbidden = 8080

// configRaw holds the closed configuration surface of spec §1.1 as raw strings
// exactly as env/flags provide them. There are NO other knobs: adding one
// requires updating that table.
type configRaw struct {
	Port       string // COUP_PORT (default 8090)
	PollHold   string // COUP_POLL_HOLD duration (default 25s, clamped [1s,30s] A1)
	LLMBaseURL string // COUP_LLM_BASE_URL (client-side only; used by the openai driver later)
	LLMModel   string // COUP_LLM_MODEL (required when DriverMode=openai, A6)
	DriverMode string // COUP_DRIVER_MODE: heuristic|openai (default heuristic)
	Debug      string // COUP_DEBUG: 0/1 or true/false (default 0)
	LogFormat  string // COUP_LOG_FORMAT: text|json (default text)
}

// config is the validated, normalized configuration main operates on.
type config struct {
	Port       int
	PollHold   time.Duration
	LLMBaseURL string
	LLMModel   string
	DriverMode string
	Debug      bool
	LogFormat  string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseConfig applies flag-wins-over-env for every knob (§1.1), validates the
// closed surface, and returns the normalized config or a startup error. It
// parses the given argv slice with a private FlagSet (no global flag state),
// which keeps repeated invocations — production once, tests many — safe.
func parseConfig(args []string) (config, error) {
	raw := configRaw{
		Port:       envOr("COUP_PORT", "8090"),
		PollHold:   envOr("COUP_POLL_HOLD", "25s"),
		LLMBaseURL: envOr("COUP_LLM_BASE_URL", "http://localhost:8080/v1"),
		LLMModel:   os.Getenv("COUP_LLM_MODEL"),
		DriverMode: envOr("COUP_DRIVER_MODE", "heuristic"),
		Debug:      envOr("COUP_DEBUG", "0"),
		LogFormat:  envOr("COUP_LOG_FORMAT", "text"),
	}
	fs := flag.NewFlagSet("ai-coup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&raw.Port, "port", raw.Port, "TCP listen port (env COUP_PORT; default 8090; 8080 is forbidden)")
	fs.StringVar(&raw.PollHold, "poll-hold", raw.PollHold, "long-poll hold duration (env COUP_POLL_HOLD; default 25s, clamped to [1s,30s])")
	fs.StringVar(&raw.LLMBaseURL, "llm-base-url", raw.LLMBaseURL, "OpenAI-compatible API base URL for the openai driver (env COUP_LLM_BASE_URL; client-side only)")
	fs.StringVar(&raw.LLMModel, "llm-model", raw.LLMModel, "model name sent in chat-completion requests (env COUP_LLM_MODEL; required with --driver-mode=openai)")
	fs.StringVar(&raw.DriverMode, "driver-mode", raw.DriverMode, "MoveDriver backing LLM seats: heuristic|openai (env COUP_DRIVER_MODE; default heuristic)")
	fs.StringVar(&raw.Debug, "debug", raw.Debug, "debug mode 0/1 or true/false (env COUP_DEBUG; gates /debug endpoints and DEBUG logs)")
	fs.StringVar(&raw.LogFormat, "log-format", raw.LogFormat, "slog handler: text|json (env COUP_LOG_FORMAT; default text)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	cfg := config{LLMBaseURL: strings.TrimRight(raw.LLMBaseURL, "/"), LLMModel: raw.LLMModel, DriverMode: strings.ToLower(strings.TrimSpace(raw.DriverMode))}

	port, err := strconv.Atoi(raw.Port)
	if err != nil || port <= 0 || port > 65535 {
		return cfg, fmt.Errorf("invalid listen port %q: must be a number in 1..65535", raw.Port)
	}
	if port == portForbidden { // §1: never bind or dial 8080
		return cfg, fmt.Errorf("port %d is forbidden to this system: a llama-server owns it in dev (spec §1) — set COUP_PORT to another value", portForbidden)
	}
	cfg.Port = port

	hold, err := time.ParseDuration(raw.PollHold)
	if err != nil {
		return cfg, fmt.Errorf("invalid COUP_POLL_HOLD %q: must be a Go duration (e.g. \"25s\")", raw.PollHold)
	}
	cfg.PollHold = app.ClampPollHold(hold) // A1: [1s,30s]

	switch {
	case cfg.DriverMode == "heuristic", cfg.DriverMode == "openai":
	default:
		return cfg, fmt.Errorf("invalid COUP_DRIVER_MODE %q: must be \"heuristic\" or \"openai\"", raw.DriverMode)
	}
	if cfg.DriverMode == "openai" && cfg.LLMModel == "" { // A6
		return cfg, fmt.Errorf("COUP_LLM_MODEL is required when COUP_DRIVER_MODE=openai")
	}

	debug, err := parseBoolFlag(raw.Debug)
	if err != nil {
		return cfg, fmt.Errorf("invalid COUP_DEBUG %q: must be 0/1 or true/false", raw.Debug)
	}
	cfg.Debug = debug

	switch strings.ToLower(strings.TrimSpace(raw.LogFormat)) {
	case "text":
		cfg.LogFormat = "text"
	case "json":
		cfg.LogFormat = "json"
	default:
		return cfg, fmt.Errorf("invalid COUP_LOG_FORMAT %q: must be \"text\" or \"json\"", raw.LogFormat)
	}
	return cfg, nil
}

// parseBoolFlag accepts the §1.1 spellings for COUP_DEBUG (0/1 and true/false).
func parseBoolFlag(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true, nil
	case "0", "false", "":
		return false, nil
	default:
		return false, fmt.Errorf("not a boolean")
	}
}

// buildLogger selects the slog handler per §1/§12. COUP_DEBUG=1 raises the
// level to DEBUG, which is exactly what the application layer gates its
// per-event payload and driver debug lines on (§12: INFO = normal operation,
// DEBUG = per-event batch contents, tool payloads, random draws).
func buildLogger(cfg config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if cfg.Debug {
		opts.Level = slog.LevelDebug
	}
	var h slog.Handler
	switch cfg.LogFormat {
	case "json":
		h = slog.NewJSONHandler(os.Stdout, opts)
	default:
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h).With("service", "ai-coup")
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "ai-coup:", err)
		}
		os.Exit(2)
	}
	log := buildLogger(cfg)

	// --- wire the ports (spec §2: composition root is the only binding site) --
	events := store.NewMemoryEventStore(clock.NewWallClock())
	games := store.NewMemoryGameStore()
	// COUP_DRIVER_MODE selects the MoveDriver backing LLM seats (§6.2). Both
	// real drivers claim LLMCapable, so LLM seats are admitted at creation and
	// driven to completion (A4 guarantees no decision can permadeath a game).
	var driver app.MoveDriver
	switch cfg.DriverMode {
	case "openai": // §6.4: base URL + model from env/flags (validated above)
		driver = llm.NewOpenAIDriver(cfg.LLMBaseURL, cfg.LLMModel, log, cfg.Debug)
	default: // heuristic (default; the only other validated value)
		driver = llm.NewHeuristicDriver()
	}
	a := app.NewApplication(app.Dependencies{
		Events: events,
		Games:  games,
		Clock:  clock.NewWallClock(),
		IDs:    clock.NewRealIDGen(),
		Driver: driver,
		Log:    log,
	}) // default Rng + TokenSource (crypto-random) are fine for production use
	webRoot, err := fs.Sub(webFS, "web") // embedded client root — CWD-independent (ui-spec U1)
	if err != nil {
		log.Error("embedded_web_root_failed", "err", err)
		os.Exit(2)
	}
	server := httpserver.NewServer(a, events, cfg.PollHold, cfg.Debug, cfg.DriverMode, log, webRoot)

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout on purpose: long-poll holds last up to 30 s (A1 cap)
		// and a global write deadline would cut them off (§9).
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if cfg.DriverMode == "openai" {
			log.Info("llm_driver_selected", "driver_mode", "openai", "llm_base_url", cfg.LLMBaseURL, "llm_model", cfg.LLMModel)
		} else {
			log.Info("llm_driver_selected", "driver_mode", "heuristic")
		}
		log.Info("listening", "addr", httpSrv.Addr, "poll_hold_ms", int(cfg.PollHold.Milliseconds()), "debug", cfg.Debug, "llm_base_url", cfg.LLMBaseURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server_failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done() // SIGINT/SIGTERM
	log.Info("shutting_down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http_shutdown_failed", "err", err)
	}
	if err := a.Close(); err != nil { // cancels in-flight LLM decisions, stops timers (§9)
		log.Error("app_close_failed", "err", err)
	}
	log.Info("stopped")
}
