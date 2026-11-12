// Package report writes and reads the structured result file.
//
// The result is a run's durable record: enough to summarise a finished run
// without executing it again. It deliberately carries no request bodies and no
// request headers, which costs nothing to guarantee here because nothing
// upstream of this package records them in the first place.
package report

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/runner"
)

// SchemaVersion is the only result version this build writes or accepts.
const SchemaVersion = 1

// MaxFileBytes bounds a result file being read back, so a wrong path cannot
// exhaust memory. A 10,000-case run is far smaller than this.
const MaxFileBytes = 64 << 20

// Result is the structured result file.
type Result struct {
	SchemaVersion int            `json:"schema_version"`
	RunID         string         `json:"run_id"`
	StartedAt     time.Time      `json:"started_at"`
	Summary       runner.Summary `json:"summary"`
}

// New stamps a summary with the identity a saved result needs.
func New(runID string, startedAt time.Time, summary runner.Summary) Result {
	return Result{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		StartedAt:     startedAt,
		Summary:       summary,
	}
}

// NewRunID returns an identifier for one run.
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A result file must still be writable; the clock is unique enough to
		// tell two runs apart when the random source is unavailable.
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}

// Write saves the result as JSON, creating the parent directory if needed.
//
// The encode goes to a temporary file in the same directory which is then
// moved into place, so a failure part way through cannot leave a truncated
// result behind, and two writers cannot interleave into one file. Without
// overwrite the move is a hard link, which fails if the destination exists:
// that refusal is atomic, so two runs sharing a path cannot both believe they
// won.
func Write(path string, overwrite bool, r Result) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create result directory %s: %w", dir, err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".result-*.json")
	if err != nil {
		return fmt.Errorf("create temporary result in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// A no-op once the file has been moved into place.
	defer func() { _ = os.Remove(tmpName) }()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write result %s: %w", path, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set result permissions %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary result for %s: %w", path, err)
	}

	if overwrite {
		if err := os.Rename(tmpName, path); err != nil {
			return fmt.Errorf("write result %s: %w", path, err)
		}
		return nil
	}
	if err := os.Link(tmpName, path); err != nil {
		return fmt.Errorf("write result %s: %w", path, err)
	}
	return nil
}

// Exists reports whether a result file is already present.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// CheckWritable reports whether a result could be written to path, so the CLI
// can refuse before a run instead of after it: discovering an unwritable path
// once 10,000 cases have executed throws the whole run away.
//
// It cannot promise the later write will succeed, which is why Write still
// enforces both the refusal and the atomicity itself.
func CheckWritable(path string, overwrite bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create result directory %s: %w", dir, err)
	}
	if !overwrite && Exists(path) {
		return fmt.Errorf("%s already exists; pass --overwrite to replace it", path)
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("cannot write results to %s: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// Load reads a result file, rejecting anything this build cannot interpret.
// Parsing is strict for the same reason scenario parsing is: a silently
// ignored key is a result that means something other than it appears to.
func Load(path string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("open result %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read result %s: %w", path, err)
	}
	if len(data) > MaxFileBytes {
		return Result{}, fmt.Errorf("result %s exceeds %d bytes", path, MaxFileBytes)
	}

	r, err := Parse(data)
	if err != nil {
		return Result{}, fmt.Errorf("result %s: %w", path, err)
	}
	return r, nil
}

// Parse strictly decodes result JSON.
func Parse(data []byte) (Result, error) {
	var r Result
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Result{}, fmt.Errorf("invalid result JSON: %w", err)
	}
	if dec.More() {
		return Result{}, errors.New("unexpected trailing content after result JSON")
	}
	if r.SchemaVersion != SchemaVersion {
		return Result{}, fmt.Errorf("unsupported schema_version %d (want %d)", r.SchemaVersion, SchemaVersion)
	}
	if r.RunID == "" {
		return Result{}, errors.New("run_id is required")
	}
	if err := validateSummary(r.Summary); err != nil {
		return Result{}, err
	}
	return r, nil
}

// validateSummary rejects a result whose own numbers disagree.
//
// This is not pedantry about shape. `report` turns a saved result into an exit
// code, so a file that establishes nothing must not be readable as a clean
// run: without these checks `{"schema_version":1,"run_id":"x"}` reports zero
// failures and exits 0, and a CI gate would read that as a pass.
func validateSummary(s runner.Summary) error {
	if s.Scenario == "" {
		return errors.New("summary.scenario is required")
	}
	if s.Runs < 1 {
		return fmt.Errorf("summary.runs must be at least 1, got %d", s.Runs)
	}
	if s.Executed+s.NotRun != s.Runs {
		return fmt.Errorf("summary accounts for %d of %d cases (executed %d, not run %d)",
			s.Executed+s.NotRun, s.Runs, s.Executed, s.NotRun)
	}
	if classified := s.Passed + s.Failed + s.Inconclusive + s.HarnessErrors; classified != s.Executed {
		return fmt.Errorf("summary classifies %d outcomes but reports %d executed cases",
			classified, s.Executed)
	}
	if len(s.Outcomes) != s.Executed {
		return fmt.Errorf("summary carries %d outcomes but reports %d executed cases",
			len(s.Outcomes), s.Executed)
	}
	return nil
}
