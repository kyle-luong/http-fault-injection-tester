// Package fixture implements the stateful application under test.
//
// The fixture is the only service the MVP targets. It binds a loopback address
// on an ephemeral port, keeps state per test case, and exposes that state so an
// oracle can decide whether a logical operation was applied once or duplicated.
// The two modes differ only in retry semantics:
//
//	unsafe-counter: applies every successful POST /operation
//	safe-counter:   applies at most one POST /operation per idempotency key
//
// Nothing here is meant for production use; /__test/* endpoints exist so a test
// harness can reset and observe state, and they carry no sensitive data.
package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// MaxRequestBodyBytes bounds every request body the fixture will read. The PRD
// requires non-streaming bounded bodies so that attempts stay replayable.
const MaxRequestBodyBytes = 64 << 10

// IdempotencyKeyHeader carries the stable logical operation identity. See
// docs/decisions/ADR-001-harness-control-headers.md.
const IdempotencyKeyHeader = "Idempotency-Key"

// Mode selects the fixture's retry semantics.
type Mode string

// Supported fixture modes.
const (
	ModeUnsafe Mode = "unsafe-counter"
	ModeSafe   Mode = "safe-counter"
)

// ParseMode validates a mode name from a scenario file or CLI flag.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeUnsafe:
		return ModeUnsafe, nil
	case ModeSafe:
		return ModeSafe, nil
	default:
		return "", fmt.Errorf("unknown fixture mode %q (want %q or %q)", s, ModeUnsafe, ModeSafe)
	}
}

// State is the oracle-visible projection of one case's state. Operations lists
// the idempotency key of every applied operation, in application order, so a
// duplicate is observable as a repeated entry and not only as a counter value.
type State struct {
	CaseID     string   `json:"case_id"`
	Counter    int      `json:"counter"`
	Operations []string `json:"operations"`
}

// OperationRequest is the body of POST /operation.
type OperationRequest struct {
	CaseID string `json:"case_id"`
	Delta  int    `json:"delta"`
}

// OperationResponse is the body returned by a successful POST /operation.
// Deduplicated reports whether the fixture recognised a repeated idempotency
// key; it is informational, and the oracle never relies on it.
type OperationResponse struct {
	CaseID       string `json:"case_id"`
	OperationID  string `json:"operation_id"`
	Counter      int    `json:"counter"`
	Deduplicated bool   `json:"deduplicated"`
}

type resetRequest struct {
	CaseID string `json:"case_id"`
}

type caseState struct {
	counter    int
	operations []string
	applied    map[string]OperationResponse
	received   int
}

// Server is a running fixture instance.
type Server struct {
	mode    Mode
	srv     *http.Server
	baseURL string

	mu    sync.Mutex
	cases map[string]*caseState

	operationRequests atomic.Int64
}

// Start binds 127.0.0.1 on an ephemeral port and serves the fixture contract.
// The caller must call Close.
func Start(mode Mode) (*Server, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind fixture listener: %w", err)
	}

	s := &Server{
		mode:    mode,
		cases:   make(map[string]*caseState),
		baseURL: "http://" + ln.Addr().String(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /__test/reset", s.handleReset)
	mux.HandleFunc("POST /operation", s.handleOperation)
	mux.HandleFunc("GET /__test/state", s.handleState)

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		// Serve always returns a non-nil error; ErrServerClosed is the normal
		// shutdown path and the goroutine must exit so leak checks pass.
		_ = s.srv.Serve(ln)
	}()

	return s, nil
}

// BaseURL is the fixture's loopback origin, e.g. http://127.0.0.1:54321.
func (s *Server) BaseURL() string { return s.baseURL }

// Mode reports the configured retry semantics.
func (s *Server) Mode() Mode { return s.mode }

// Close shuts the listener down and waits briefly for in-flight handlers.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		_ = s.srv.Close()
		return fmt.Errorf("shutdown fixture: %w", err)
	}
	return nil
}

// OperationRequests reports how many POST /operation requests reached the
// fixture in total, including rejected ones. Tests use it to prove that a
// before_forward fault really kept the backend out of the interaction.
func (s *Server) OperationRequests() int64 { return s.operationRequests.Load() }

// CaseOperationRequests reports how many POST /operation requests reached the
// fixture for one case after that case was reset.
func (s *Server) CaseOperationRequests(caseID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.cases[caseID]
	if !ok {
		return 0
	}
	return cs.received
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := validateCaseID(req.CaseID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	s.cases[req.CaseID] = &caseState{applied: make(map[string]OperationResponse)}
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	s.operationRequests.Add(1)

	key := r.Header.Get(IdempotencyKeyHeader)
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing "+IdempotencyKeyHeader+" header")
		return
	}
	if len(key) > 256 {
		writeError(w, http.StatusBadRequest, IdempotencyKeyHeader+" exceeds 256 bytes")
		return
	}

	var req OperationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := validateCaseID(req.CaseID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Delta < 1 || req.Delta > 1000 {
		writeError(w, http.StatusBadRequest, "delta must be between 1 and 1000")
		return
	}

	s.mu.Lock()
	cs, ok := s.cases[req.CaseID]
	if !ok {
		s.mu.Unlock()
		// A missing case means the harness skipped reset. That is a harness
		// defect, not an application-under-test bug, so it must be loud.
		writeError(w, http.StatusConflict, "unknown case_id "+req.CaseID+"; POST /__test/reset first")
		return
	}
	cs.received++

	if s.mode == ModeSafe {
		if prior, dup := cs.applied[key]; dup {
			// Replay the original result verbatim; a repeat must not change state.
			prior.Deduplicated = true
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, prior)
			return
		}
	}

	cs.counter += req.Delta
	cs.operations = append(cs.operations, key)
	resp := OperationResponse{
		CaseID:      req.CaseID,
		OperationID: key,
		Counter:     cs.counter,
	}
	if s.mode == ModeSafe {
		cs.applied[key] = resp
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	caseID := r.URL.Query().Get("case_id")
	if err := validateCaseID(caseID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.mu.Lock()
	cs, ok := s.cases[caseID]
	var st State
	if ok {
		st = State{
			CaseID:     caseID,
			Counter:    cs.counter,
			Operations: append([]string(nil), cs.operations...),
		}
	}
	s.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "unknown case_id "+caseID)
		return
	}
	if st.Operations == nil {
		st.Operations = []string{}
	}
	writeJSON(w, http.StatusOK, st)
}

func validateCaseID(id string) error {
	if id == "" {
		return errors.New("case_id is required")
	}
	if len(id) > 128 {
		return errors.New("case_id exceeds 128 bytes")
	}
	for _, c := range id {
		isAllowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !isAllowed {
			return fmt.Errorf("case_id %q contains disallowed characters", id)
		}
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing content after JSON body")
	}
	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds %d bytes", MaxRequestBodyBytes))
		return
	}
	writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
