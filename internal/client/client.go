// Package client drives one logical operation against the fixture through the
// proxy, applying a scenario's retry policy.
//
// Retry policy lives here and nowhere else: the proxy only injects faults. Each
// attempt sends a byte-identical body, and when the policy reuses the
// idempotency key every attempt also carries the same logical operation
// identity -- which is precisely the condition under which a correct server may
// deduplicate and an incorrect one duplicates.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/proxy"
)

// MaxAttemptsLimit bounds how many attempts a scenario may request.
const MaxAttemptsLimit = 10

// Policy is the per-case client behaviour taken from a scenario.
type Policy struct {
	Method                string
	Path                  string
	Delta                 int
	MaxAttempts           int
	RetryOnTransportError bool
	ReuseIdempotencyKey   bool
	// AttemptTimeout bounds a single attempt. Zero means only the caller's
	// context bounds it.
	AttemptTimeout time.Duration
}

// Validate reports whether the policy is implementable.
func (p Policy) Validate() error {
	if p.Method != http.MethodPost {
		return fmt.Errorf("client method %q is not supported; the v1 fixture contract is POST only", p.Method)
	}
	if p.Path == "" || p.Path[0] != '/' {
		return fmt.Errorf("client path %q must be absolute", p.Path)
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > MaxAttemptsLimit {
		return fmt.Errorf("client max_attempts must be between 1 and %d, got %d", MaxAttemptsLimit, p.MaxAttempts)
	}
	if p.Delta < 1 || p.Delta > 1000 {
		return fmt.Errorf("client delta must be between 1 and 1000, got %d", p.Delta)
	}
	return nil
}

// Attempt records what the client observed for one attempt. TransportError is
// the error text, kept because "no response at all" is the signal that matters
// for a lost response; request bodies and headers are never recorded.
type Attempt struct {
	Number         int                        `json:"number"`
	IdempotencyKey string                     `json:"idempotency_key"`
	StatusCode     int                        `json:"status_code,omitempty"`
	TransportError string                     `json:"transport_error,omitempty"`
	Duration       time.Duration              `json:"duration_ns"`
	Response       *fixture.OperationResponse `json:"response,omitempty"`
}

// Failed reports whether the attempt produced no usable response.
func (a Attempt) Failed() bool {
	return a.TransportError != "" || a.StatusCode < 200 || a.StatusCode > 299
}

// Result is the outcome of one case's attempt sequence.
type Result struct {
	CaseID   string    `json:"case_id"`
	Attempts []Attempt `json:"attempts"`
}

// Succeeded reports whether any attempt received a 2xx response.
func (r Result) Succeeded() bool {
	for _, a := range r.Attempts {
		if a.TransportError == "" && a.StatusCode >= 200 && a.StatusCode <= 299 {
			return true
		}
	}
	return false
}

// LastAttempt returns the final attempt, or false when none ran.
func (r Result) LastAttempt() (Attempt, bool) {
	if len(r.Attempts) == 0 {
		return Attempt{}, false
	}
	return r.Attempts[len(r.Attempts)-1], true
}

// NewHTTPClient builds a bounded HTTP/1.1 client for harness traffic. The
// caller should call CloseIdleConnections on the returned transport when done;
// use CloseIdleConnections below.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 64,
			MaxConnsPerHost:     128,
			IdleConnTimeout:     20 * time.Second,
			DisableCompression:  true,
			ForceAttemptHTTP2:   false,
		},
		// Redirects would hide which endpoint actually ran; the fixture never
		// issues any.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not expected from the fixture")
		},
	}
}

// CloseIdleConnections releases pooled connections held by a harness client.
func CloseIdleConnections(hc *http.Client) {
	if tr, ok := hc.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// Execute runs the attempt sequence for one case against baseURL, which is
// normally the proxy. It returns an error only for harness-level problems; a
// lost or refused response is recorded in the Result, because that is the
// behaviour under test rather than a failure of the harness.
func Execute(ctx context.Context, hc *http.Client, baseURL, caseID string, p Policy) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}

	endpoint, err := url.JoinPath(baseURL, p.Path)
	if err != nil {
		return Result{}, fmt.Errorf("build request URL: %w", err)
	}
	body, err := json.Marshal(fixture.OperationRequest{CaseID: caseID, Delta: p.Delta})
	if err != nil {
		return Result{}, fmt.Errorf("encode operation body: %w", err)
	}

	result := Result{CaseID: caseID}

	for number := 1; number <= p.MaxAttempts; number++ {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("case %s cancelled before attempt %d: %w", caseID, number, err)
		}

		attempt := Attempt{Number: number, IdempotencyKey: idempotencyKey(caseID, number, p.ReuseIdempotencyKey)}
		started := time.Now()
		status, decoded, err := p.sendOnce(ctx, hc, endpoint, caseID, attempt, body)
		attempt.Duration = time.Since(started)
		attempt.StatusCode = status
		attempt.Response = decoded
		if err != nil {
			attempt.TransportError = err.Error()
		}
		result.Attempts = append(result.Attempts, attempt)

		switch {
		case err != nil:
			// A transport error is the ambiguous case: the operation may or may
			// not have been applied. Only the policy decides whether to retry.
			if !p.RetryOnTransportError {
				return result, nil
			}
		case status >= 200 && status <= 299:
			return result, nil
		default:
			// A complete HTTP error response is unambiguous, so v1 does not
			// retry it. Retry-on-status is a LATER scenario option.
			return result, nil
		}
	}

	return result, nil
}

func (p Policy) sendOnce(
	ctx context.Context,
	hc *http.Client,
	endpoint, caseID string,
	attempt Attempt,
	body []byte,
) (int, *fixture.OperationResponse, error) {
	attemptCtx := ctx
	if p.AttemptTimeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, p.AttemptTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(attemptCtx, p.Method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(fixture.IdempotencyKeyHeader, attempt.IdempotencyKey)
	req.Header.Set(proxy.HeaderCaseID, caseID)
	req.Header.Set(proxy.HeaderAttempt, strconv.Itoa(attempt.Number))
	req.ContentLength = int64(len(body))

	// Exactly one wire request per logical attempt.
	//
	// http.Transport silently replays a request when a *reused* connection
	// fails, and Request.isReplayable treats any request carrying an
	// Idempotency-Key header as safe to replay. This harness always sends that
	// header, so the stdlib would re-send an attempt whose response the proxy
	// destroyed on purpose: the side effect would be applied twice, several
	// wire requests would share one attempt number (breaking deterministic
	// fault selection), and a lost response could even be reported here as a
	// success. Clearing GetBody makes the request non-replayable, so the retry
	// decision belongs to this package alone.
	//
	// A transport-level retry where nothing was written is unaffected and
	// remains safe, because such a request never reached the server.
	req.GetBody = nil

	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, fixture.MaxResponseBodyBytes))
		_ = resp.Body.Close()
	}()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, fixture.MaxResponseBodyBytes+1))
	if readErr != nil {
		// Headers arrived but the body did not: still a transport failure for
		// this attempt, and still ambiguous.
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", readErr)
	}
	if len(raw) > fixture.MaxResponseBodyBytes {
		return resp.StatusCode, nil, fmt.Errorf("response body exceeds %d bytes", fixture.MaxResponseBodyBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, nil, nil
	}

	var decoded fixture.OperationResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("decode operation response: %w", err)
	}
	return resp.StatusCode, &decoded, nil
}

// idempotencyKey derives the attempt's logical operation identity. Reusing the
// key is what a correct client does when retrying an ambiguous request; a fresh
// key per attempt models a client that does not.
func idempotencyKey(caseID string, attempt int, reuse bool) string {
	if reuse {
		return caseID + "-key"
	}
	return fmt.Sprintf("%s-key-%d", caseID, attempt)
}
