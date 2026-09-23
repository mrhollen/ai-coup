package main

import (
	"testing"
	"time"
)

// allKnobs are the §1.1 env vars; tests blank them so each case sees exactly
// what it sets (t.Setenv restores the originals after each test).
var allKnobs = []string{"COUP_PORT", "COUP_POLL_HOLD", "COUP_LLM_BASE_URL", "COUP_LLM_MODEL", "COUP_DRIVER_MODE", "COUP_DEBUG", "COUP_LOG_FORMAT"}

// withConfig runs parseConfig over a controlled argv (flag parity, §1.1: flag
// wins over env) and the given env values; it fails the test on startup errors.
func withConfig(t *testing.T, env map[string]string, args ...string) config {
	t.Helper()
	for _, k := range allKnobs {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := parseConfig(args)
	if err != nil {
		t.Fatalf("parseConfig(%v, env=%v): %v", args, env, err)
	}
	return cfg
}

func withConfigErr(t *testing.T, env map[string]string, args ...string) error {
	t.Helper()
	for _, k := range allKnobs {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	_, err := parseConfig(args)
	return err
}

// TestParseConfig_Defaults: `go run .` with no env at all boots the defaults —
// port 8090, 25 s hold (A1), heuristic driver, text logs, debug off, and the
// llama-server's base URL as the client-side LLM default (§1).
func TestParseConfig_Defaults(t *testing.T) {
	cfg := withConfig(t, nil)
	if cfg.Port != 8090 {
		t.Errorf("default port = %d, want 8090", cfg.Port)
	}
	if cfg.PollHold != 25*time.Second {
		t.Errorf("default poll hold = %v, want 25s", cfg.PollHold)
	}
	if cfg.DriverMode != "heuristic" || cfg.LLMModel != "" {
		t.Errorf("default driver mode/model = %q/%q, want heuristic/empty", cfg.DriverMode, cfg.LLMModel)
	}
	if cfg.Debug || cfg.LogFormat != "text" {
		t.Errorf("debug/log format = %v/%q, want false/text", cfg.Debug, cfg.LogFormat)
	}
	if cfg.LLMBaseURL != "http://localhost:8080/v1" {
		t.Errorf("default LLM base URL = %q, want http://localhost:8080/v1 (client-side only)", cfg.LLMBaseURL)
	}
}

// TestParseConfig_FlagBeatsEnv (§1.1 flag parity): a set flag overrides the env
// value; an unset flag falls back to env.
func TestParseConfig_FlagBeatsEnv(t *testing.T) {
	cfg := withConfig(t, map[string]string{"COUP_PORT": "9000", "COUP_DEBUG": "1"}, "--port", "9001")
	if cfg.Port != 9001 {
		t.Errorf("flag should beat env: port = %d, want 9001", cfg.Port)
	}
	if !cfg.Debug {
		t.Error("env COUP_DEBUG=1 not applied (no --debug flag given)")
	}

	cfg = withConfig(t, map[string]string{"COUP_PORT": "9002"})
	if cfg.Port != 9002 {
		t.Errorf("unset flag should fall back to env: port = %d, want 9002", cfg.Port)
	}
}

// TestParseConfig_Port8080Refused (§1): binding 8080 must fail fast at startup,
// both via env and via flag — this system never binds (or dials in tests) it.
func TestParseConfig_Port8080Refused(t *testing.T) {
	if err := withConfigErr(t, map[string]string{"COUP_PORT": "8080"}); err == nil {
		t.Fatal("COUP_PORT=8080 must be a startup error (spec §1)")
	}
	if err := withConfigErr(t, nil, "--port", "8080"); err == nil {
		t.Fatal("--port 8080 must be a startup error (spec §1)")
	}
}

// TestParseConfig_DriverModeValidation: A6 (openai requires a model), the closed
// mode set, COUP_DEBUG's accepted spellings, and hold clamping (A1).
func TestParseConfig_DriverModeValidation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr bool
	}{
		{"openai without model fails (A6)", map[string]string{"COUP_DRIVER_MODE": "openai"}, nil, true},
		{"openai with model passes", map[string]string{"COUP_DRIVER_MODE": "openai", "COUP_LLM_MODEL": "qwen2.5"}, nil, false},
		{"bogus mode fails", map[string]string{"COUP_DRIVER_MODE": "gpt-local"}, nil, true},
		{"debug=1 accepted", map[string]string{"COUP_DEBUG": "1"}, nil, false},
		{"debug=true accepted", map[string]string{"COUP_DEBUG": "true"}, nil, false},
		{"debug=bogus fails", map[string]string{"COUP_DEBUG": "maybe"}, nil, true},
		{"bad log format fails", map[string]string{"COUP_LOG_FORMAT": "yaml"}, nil, true},
		{"bad hold fails", map[string]string{"COUP_POLL_HOLD": "soon"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg config
			var err error
			if tc.wantErr {
				err = withConfigErr(t, tc.env, tc.args...)
				if err == nil {
					t.Fatalf("expected a startup error, got %+v", cfg)
				}
				return
			}
			cfg = withConfig(t, tc.env, tc.args...)
			_ = cfg
		})
	}

	// The clamp (A1) applies to the normalized hold: sub-1s floors to 1s and
	// over-30s caps to 30s.
	if got := withConfig(t, map[string]string{"COUP_POLL_HOLD": "500ms"}).PollHold; got != time.Second {
		t.Errorf("500ms hold = %v, want clamped 1s (A1)", got)
	}
	if got := withConfig(t, map[string]string{"COUP_POLL_HOLD": "90s"}).PollHold; got != 30*time.Second {
		t.Errorf("90s hold = %v, want clamped 30s (A1)", got)
	}
}
