package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHerdrAppHomeUsesExplicitAbsoluteHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERDR_APP_HOME", home)
	got, err := herdrAppHome()
	if err != nil || got != home {
		t.Fatal(got, err)
	}
	t.Setenv("HERDR_APP_HOME", "relative")
	if _, err := herdrAppHome(); err == nil {
		t.Fatal("relative app home accepted")
	}
	t.Setenv("HERDR_APP_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", home)
	got, err = herdrAppHome()
	if err != nil || got != filepath.Join(home, "herdr-app") {
		t.Fatal(got, err)
	}
}

func TestHerdrAppOpenReusesModAndOnlyCreatesAModTab(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	home := t.TempDir()
	t.Setenv("HERDR_APP_HOME", home)
	dir := filepath.Join(home, "mods", "aiq")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte("invented"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("AIQTEST_APP_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stub := filepath.Join(bin, "herdr-app")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AIQTEST_APP_LOG\"\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := appOpen(nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil || string(b) != "do aiq.activate\n" {
		t.Fatal("did not reuse loaded mod", string(b), err)
	}
	os.Remove(log)
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AIQTEST_APP_LOG\"\n[ \"$2\" = aiq.activate ] && exit 1\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := appOpen(nil); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(log)
	if err != nil || !strings.Contains(string(b), `do tab.new {"mod":"aiq","name":"aiq"}`) || strings.Contains(string(b), "terminal.") {
		t.Fatal("did not limit creation to a mod tab", string(b), err)
	}
}
