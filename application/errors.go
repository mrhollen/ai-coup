package application

import (
	"errors"
	"fmt"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// Wire error codes and HTTP statuses for the API envelope of spec §11.1. The
// domain's table-voice messages travel through unchanged; AppError.Code is the
// machine-readable wire code a handler puts in the JSON body, and Status the
// HTTP status to answer with.
const (
	ErrUnauthorized  = "unauthorized"
	ErrNotFound      = "not_found"
	ErrInvalidInput  = "invalid_payload"
	ErrServer        = "server_error"
)

// AppError is the typed error every use case returns for client-facing
// failures. It carries both the §11.1 wire code and the HTTP status so the
// handler layer stays a thin mapping of (status, {"error":{"code","message"}}).
type AppError struct {
	Code    string
	Message string
	Status  int
}

func (e *AppError) Error() string { return e.Message }

// NewAppError builds an AppError with the given wire code, message and HTTP
// status.
func NewAppError(code, message string, status int) *AppError {
	return &AppError{Code: code, Message: message, Status: status}
}

var (
	// ErrUnauthorized401 is returned when a seat-scoped resource is requested
	// without a valid human bearer token context.
	ErrUnauthorized401 = NewAppError(ErrUnauthorized, "a valid player token is required", 401)
)

// wireMap remaps fine-grained domain codes onto the §11.1 wire vocabulary
// where the spec names a canonical code; unlisted codes pass through with
// their stable domain name (the §11.1 envelope example itself uses the
// non-listed "not_enough_coins"). Statuses follow §11.2: 409 for semantic
// conflicts, 400 for malformed payloads, 404 for lookups.
var wireMap = map[string]struct {
	code   string
	status int
}{
	domain.CodeWindowResolved:    {"already_resolved", 409}, // A16 stale window id / closed window
	domain.CodeDecisionMade:      {"already_resolved", 409}, // answered or gone single-seat decision
	domain.CodeVoteExists:        {"already_resolved", 409}, // one active vote per game (A15)
	domain.CodeAlreadyVoted:      {"already_resolved", 409}, // second ballot from the same voter
	domain.CodeVoteResolved:      {"vote_closed", 409},     // no live ballot to cast on (§7/§11.2)
	domain.CodeNoSuchSeat:        {ErrNotFound, 404},       // unknown seat id in a request
	domain.CodeInvalidChoice:     {ErrInvalidInput, 400},   // choice value not in the option set
	domain.CodeMissingClaim:      {ErrInvalidInput, 400},   // payload lacks a required claim
	domain.CodeInvalidClaim:      {ErrInvalidInput, 400},   // payload names a wrong claim character
}

// FromDomain converts an engine error into the wire representation. Domain
// errors carry normative table-voice messages (§4.5) which are shown verbatim
// to humans and fed back to LLM drivers.
func FromDomain(err error) *AppError {
	var le *domain.LegacyError
	if errors.As(err, &le) {
		if w, ok := wireMap[le.Code]; ok {
			return NewAppError(w.code, le.Message, w.status)
		}
		return NewAppError(le.Code, le.Message, 409)
	}
	return NewAppError(ErrServer, fmt.Sprintf("internal error: %v", err), 500)
}

// AsAppError reports whether err is an *AppError and returns it.
func AsAppError(err error) (*AppError, bool) {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}
