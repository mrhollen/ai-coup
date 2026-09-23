// Package http implements the §11 HTTP surface of spec §2.1: a chi router
// over stdlib net/http, request-id (A17) and access-log (§12/§13) middleware,
// bearer-token seat auth (§11.1), and the handlers for game creation,
// per-viewer snapshots, long-poll events, decisions, forfeit votes and the
// COUP_DEBUG-gated full-state dump (A10). Handlers stay thin: validation and
// state live in the application layer; this package maps wire shape ↔ app
// types and AppError → §11.1 envelope only.
package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/store"
)

// maxBodyBytes caps request bodies (1 MiB): decision and create payloads are
// tiny; a larger body is malformed by construction (§11.1 invalid_payload).
const maxBodyBytes = 1 << 20

// Server holds the composition-bound dependencies of the HTTP surface. App is
// the use-case container; Events backs the unfiltered debug event history
// (MemoryEventStore.All, §11); PollHold is the configured long-poll hold
// (clamped per A1 at construction); Debug gates the /debug routes (A10);
// DriverMode names the configured LLM driver for the debug config block (§1);
// WebRoot is the embedded client root served at "/" (ui-spec U1 — same origin
// as the API, relative URLs only).
type Server struct {
	App        *app.Application
	Events     *store.MemoryEventStore
	PollHold   time.Duration
	Debug      bool
	DriverMode string
	Log        *slog.Logger
	WebRoot    fs.FS
	web        http.Handler // http.FileServerFS over WebRoot (built once)
}

// NewServer builds the HTTP surface. The hold is clamped to [1s,30s] per A1
// before use; a nil Log falls back to slog.Default(). webRoot is the client
// root served at "/" — production passes the go:embed copy from main (the
// binary serves the UI from any working directory); tests pass their own.
func NewServer(a *app.Application, events *store.MemoryEventStore, pollHold time.Duration, debug bool, driverMode string, log *slog.Logger, webRoot fs.FS) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		App:        a,
		Events:     events,
		PollHold:   app.ClampPollHold(pollHold), // A1
		Debug:      debug,
		DriverMode: driverMode,
		Log:        log,
		WebRoot:    webRoot,
		web:        http.FileServerFS(webRoot),
	}
}

// Handler assembles the chi router with middleware and the §11.2 route table:
//
//	POST   /games                          create (no auth) → 201 + one-time tokens
//	GET    /games/{id}                     per-viewer snapshot (bearer) → 200
//	GET    /games/{id}/events?after=N      long-poll (bearer) → 200, empty on hold expiry (A1)
//	POST   /games/{id}/decisions           the single decisions endpoint (bearer) → 202
//	POST   /games/{id}/forfeit-votes       ballot initiation (bearer, human) → 201
//	POST   /games/{id}/forfeit-votes/{vote_id}/ballots   ballot cast (bearer, voter) → 200
//	GET    /debug/games/{id}               full state dump — mounted ONLY when COUP_DEBUG=1 (A10: else 404, not 403), unauthenticated
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(requestIDMiddleware)     // A17: echo or generate, every response + log line
	r.Use(s.accessLogMiddleware()) // §12/§13: one INFO line per request

	r.Post("/games", s.handleCreateGame)
	r.Route("/games/{id}", func(r chi.Router) {
		r.Get("/", s.handleSnapshot)
		r.Get("/events", s.handleEvents)
		r.Post("/decisions", s.handleDecision)
		r.Post("/forfeit-votes", s.handleInitiateForfeitVote)
		r.Post("/forfeit-votes/{vote_id}/ballots", s.handleCastForfeitVote)
	})

	// §11.2 debug endpoint (A10): gated on COUP_DEBUG at ROUTE level — with the
	// gate off the path simply does not exist, so it 404s like any unknown path
	// (not 403). No auth: the env gate is the security boundary (local operator
	// tool).
	if s.Debug {
		r.Get("/debug/games/{id}", s.handleDebugGame)
	}

	// Static mount: the web client is served from the EMBEDDED "web/" root
	// (go:embed in main, passed in via NewServer.WebRoot) rooted at "/" — so
	// the UI works from any process CWD, not just the repository root (ui-spec
	// U1 delivery model — same origin as the API, relative URLs only). The
	// hash-routed SPA needs no path fallback: index.html lands at "/", assets
	// under their own names, unknown paths 404. The handler keeps the §11.1
	// JSON envelope on misses (the established contract for unknown routes —
	// asserted by the httptest suites) instead of net/http's text 404 page.
	r.HandleFunc("/*", s.handleStaticMount)

	// Envelope-consistent fallbacks (the catch-all above covers unknown paths;
	// these guard 405-adjacent edge cases). §11.1: everything except 405 gets
	// application/json.
	notFound := func(rw http.ResponseWriter, r *http.Request) {
		s.writeError(rw, r, app.NewAppError(app.ErrNotFound, "no such route", 404))
	}
	r.NotFound(notFound)

	return r
}

// staticMissRecorder lets a file-server miss (4xx: missing path, invalid path)
// be intercepted before the client sees net/http's text 404 page, so the
// response can be re-answered with the §11.1 JSON envelope. Sub-400 statuses
// (file bodies, redirects) pass through untouched.
type staticMissRecorder struct {
	http.ResponseWriter
	status int
	missed bool
}

func (w *staticMissRecorder) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	if code >= 400 {
		w.missed = true // swallow the text page; the envelope replaces it
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *staticMissRecorder) Write(b []byte) (int, error) {
	if w.missed {
		return len(b), nil // drop "404 page not found" — nothing reached the client
	}
	return w.ResponseWriter.Write(b)
}

// handleStaticMount serves the embedded web client rooted at "/" (see the
// Handler comment). Missing files answer the standard not_found envelope so
// unknown routes keep their §11.1 contract; a healthy serve is byte-identical
// to a bare net/http file server. s.web is one FileServerFS over the embedded
// FS — stateless and safe for concurrent use by every request.
func (s *Server) handleStaticMount(rw http.ResponseWriter, r *http.Request) {
	rec := &staticMissRecorder{ResponseWriter: rw}
	s.web.ServeHTTP(rec, r)
	if rec.missed {
		s.writeError(rw, r, app.NewAppError(app.ErrNotFound, "no such route", 404))
	}
}

// ---------------------------------------------------------------------------
// request context plumbing
// ---------------------------------------------------------------------------

type requestIDKey struct{}

type sessionKey struct{}

// authSession is the bearer identity resolved once per request by the access-
// log middleware: the game and human seat a token was issued for (§6.1). Nil
// in context = unauthenticated (no valid bearer presented).
type authSession struct {
	GameID string
	SeatID string
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func sessionFrom(ctx context.Context) *authSession {
	sess, _ := ctx.Value(sessionKey{}).(*authSession)
	return sess
}

// newRequestID generates the server-side 32-hex-char request id (A17).
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000" // crypto fault: keep the wire shape, log upstream
	}
	return hex.EncodeToString(b[:])
}

// requestIDMiddleware echoes a client-supplied X-Request-Id or generates one
// (A17). Every response carries it in the X-Request-Id header; error envelopes
// also embed it as request_id (§11.1).
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		rw.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

// accessLogMiddleware resolves the bearer token once per request (serving both
// the §12 access-log seat field and the handlers' authorize helper) and emits
// one INFO line per request: request id, method, path, player seat ("-" when
// unauthenticated), status, latency_ms. Field discipline per §13: access logs
// carry the request id instead of turn data.
func (s *Server) accessLogMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			start := time.Now()
			var sess *authSession
			if tok := bearerToken(r); tok != "" {
				if g, sid, ok := s.App.ResolveToken(tok); ok {
					sess = &authSession{GameID: g, SeatID: sid}
				}
			}
			ctx := context.WithValue(r.Context(), sessionKey{}, sess)
			rec := &statusRecorder{ResponseWriter: rw} // zero status ⇒ implicit 200 if no WriteHeader
			next.ServeHTTP(rec, r.WithContext(ctx))

			seat := "-"
			if sess != nil {
				seat = sess.SeatID
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK // written without an explicit WriteHeader
			}
			s.Log.Info("http_request",
				"request_id", requestIDFrom(ctx),
				"method", r.Method,
				"path", r.URL.Path,
				"seat_id", seat,
				"status", status,
				"latency_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// bearerToken extracts the Bearer token from the Authorization header (§11.1).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// authorize enforces bearer auth on seat-scoped routes (§11.1): the token must
// resolve AND have been issued for the game in the URL (tokens are bound to a
// specific game, §6.1). Missing/invalid token → 401 unauthorized; a valid token
// of another game is unauthorized HERE. Handlers write nothing else on failure.
func (s *Server) authorize(rw http.ResponseWriter, r *http.Request, pathGameID string) (seatID string, ok bool) {
	sess := sessionFrom(r.Context())
	if sess == nil {
		s.writeError(rw, r, app.ErrUnauthorized401)
		return "", false
	}
	if sess.GameID != pathGameID {
		s.writeError(rw, r, app.NewAppError(app.ErrUnauthorized, "this token was not issued for that game", 401))
		return "", false
	}
	return sess.SeatID, true
}

// ---------------------------------------------------------------------------
// JSON rendering (§11.1)
// ---------------------------------------------------------------------------

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// writeJSON renders v as the response body: application/json (§11.1 — all
// endpoints except 405 responses) with the status code.
func (s *Server) writeJSON(rw http.ResponseWriter, r *http.Request, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	if err := json.NewEncoder(rw).Encode(v); err != nil {
		s.Log.Error("http_encode_failed",
			"request_id", requestIDFrom(r.Context()),
			"path", r.URL.Path,
			"err", err)
	}
}

// writeError renders the §11.1 error envelope for any failure: AppErrors keep
// their wire code, table-voice message and status verbatim (§4.5); anything
// else is a 500 server_error (and gets logged — it indicates a bug).
func (s *Server) writeError(rw http.ResponseWriter, r *http.Request, err error) {
	code, message, status := "server_error", "internal server error", http.StatusInternalServerError
	if ae, ok := app.AsAppError(err); ok {
		code, message, status = ae.Code, ae.Message, ae.Status
	} else if err != nil {
		s.Log.Error("http_unexpected_error",
			"request_id", requestIDFrom(r.Context()),
			"path", r.URL.Path,
			"err", err)
	}
	s.writeJSON(rw, r, status, struct {
		Error errorBody `json:"error"`
	}{Error: errorBody{Code: code, Message: message, RequestID: requestIDFrom(r.Context())}})
}

// decodeJSON parses the request body as JSON (capped at maxBodyBytes). Unknown
// fields are ignored — wire contracts only ever gain optional fields.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	return dec.Decode(v)
}
