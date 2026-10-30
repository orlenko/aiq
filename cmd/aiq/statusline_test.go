package main

import (
	"bytes"
	"testing"
)

func TestRunChainedStatuslinePassesInputAndMarksChain(t *testing.T) {
	t.Setenv(statuslineChainEnv, "")
	var out bytes.Buffer
	prev := `printf '%s ' "$(cat)"; printf '%s' "$` + statuslineChainEnv + `"`
	if !runChainedStatusline(prev, []byte("payload"), &out) {
		t.Fatal("chained statusline did not render")
	}
	if got, want := out.String(), "payload 1"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// A chained script that calls aiq back must not run itself again.
func TestRunChainedStatuslineDeclinesInsideChain(t *testing.T) {
	t.Setenv(statuslineChainEnv, "1")
	var out bytes.Buffer
	if runChainedStatusline("printf looped", nil, &out) {
		t.Fatal("ran the chained statusline from inside a chain")
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want none", out.String())
	}
}

func TestRunChainedStatuslineFallsBack(t *testing.T) {
	t.Setenv(statuslineChainEnv, "")
	var out bytes.Buffer
	if runChainedStatusline("", nil, &out) {
		t.Error("empty command reported a render")
	}
	if runChainedStatusline("exit 3", nil, &out) {
		t.Error("failing command reported a render")
	}
}
