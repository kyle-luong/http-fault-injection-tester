package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionAndHelpSucceed(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--help"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitPass {
			t.Errorf("run(%v) = %d, want %d; stderr: %s", args, code, exitPass, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Errorf("run(%v) wrote nothing to stdout", args)
		}
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}, {"replay"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestVersionIsReported(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"version"}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), version) {
		t.Errorf("version output %q does not contain %q", stdout.String(), version)
	}
}
