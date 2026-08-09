package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// MaxResponseBodyBytes bounds every fixture response a harness client reads.
const MaxResponseBodyBytes = 64 << 10

// The helpers below are the client half of the fixture contract described in
// docs/PRD.md section 2. They are kept here, next to the server, so that a
// future external fixture can be validated against exactly one definition of
// the wire format.

// Reset clears a case's state, creating it if necessary.
func Reset(ctx context.Context, hc *http.Client, baseURL, caseID string) error {
	body, err := json.Marshal(resetRequest{CaseID: caseID})
	if err != nil {
		return fmt.Errorf("encode reset body: %w", err)
	}
	endpoint, err := url.JoinPath(baseURL, "/__test/reset")
	if err != nil {
		return fmt.Errorf("build reset URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build reset request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("reset case %s: %w", caseID, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reset case %s: unexpected status %d", caseID, resp.StatusCode)
	}
	return nil
}

// FetchState reads the oracle-visible state for one case.
func FetchState(ctx context.Context, hc *http.Client, baseURL, caseID string) (State, error) {
	endpoint, err := url.JoinPath(baseURL, "/__test/state")
	if err != nil {
		return State{}, fmt.Errorf("build state URL: %w", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return State{}, fmt.Errorf("parse state URL: %w", err)
	}
	parsed.RawQuery = url.Values{"case_id": []string{caseID}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return State{}, fmt.Errorf("build state request: %w", err)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return State{}, fmt.Errorf("read state for case %s: %w", caseID, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return State{}, fmt.Errorf("read state for case %s: unexpected status %d", caseID, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyBytes+1))
	if err != nil {
		return State{}, fmt.Errorf("read state body for case %s: %w", caseID, err)
	}
	if len(raw) > MaxResponseBodyBytes {
		return State{}, fmt.Errorf("state body for case %s exceeds %d bytes", caseID, MaxResponseBodyBytes)
	}

	var st State
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return State{}, fmt.Errorf("decode state for case %s: %w", caseID, err)
	}
	return st, nil
}

// drainAndClose discards the remaining body so the connection can be reused,
// bounded so a misbehaving peer cannot make the harness read forever.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, MaxResponseBodyBytes))
	_ = body.Close()
}
