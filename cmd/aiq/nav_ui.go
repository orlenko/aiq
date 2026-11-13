package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/navclient"
	"github.com/orlenko/aiq/internal/navhealth"
	"github.com/orlenko/aiq/internal/navlaunch"
	"github.com/orlenko/aiq/internal/navtoken"
	"github.com/orlenko/aiq/internal/paths"
)

const uiUsage = "usage: aiq ui [directory] | --info | --revoke | --help"
const uiHelpText = uiUsage + `

Open the local Sessions and Asks navigator for this directory.
Requires an already running daemon with [ui] navigator = true.
Login is per tab and expires after 30 minutes idle or eight hours.
--info prints authenticated directory and host information.
--revoke invalidates every browser login and pending link.

Browser handoffs are private files under the aiq data directory. Later launches
prune known files older than ten minutes; without another launch they can remain
after their one-use links expire. Browser dispatch does not confirm arrival.
macOS and Linux only; this command never starts or restarts a daemon.
`

var (
	errUIPlatform    = errors.New("navigator unlock is supported on macOS and Linux only; Windows validation is pending")
	errUIConfig      = errors.New("navigator configuration could not be read")
	errUIDirectory   = errors.New("navigator requires an absolute data directory without dot segments; restart with that configuration")
	errUIUnavailable = errors.New("navigator could not be authenticated; check the daemon and run aiq ui again")
	errUIOpen        = errors.New("browser opening could not be confirmed; run aiq ui again if needed")
)

type uiMode uint8

const (
	uiLaunch uiMode = iota
	uiHelp
	uiInfo
	uiRevoke
)

type uiOptions struct {
	mode      uiMode
	directory string
}

func parseUIArgs(args []string) (uiOptions, error) {
	if len(args) > 1 {
		return uiOptions{}, errors.New(uiUsage)
	}
	if len(args) == 0 {
		return uiOptions{}, nil
	}
	a := args[0]
	if !utf8.ValidString(a) || strings.IndexFunc(a, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return uiOptions{}, errors.New(uiUsage)
	}
	switch a {
	case "--help", "-h":
		return uiOptions{mode: uiHelp}, nil
	case "--info":
		return uiOptions{mode: uiInfo}, nil
	case "--revoke":
		return uiOptions{mode: uiRevoke}, nil
	}
	if a == "" || strings.HasPrefix(a, "-") {
		return uiOptions{}, errors.New(uiUsage)
	}
	return uiOptions{directory: a}, nil
}

type uiDependencies struct {
	load     func() (*config.Config, error)
	data     func() string
	cwd      func() (string, error)
	identity func(context.Context) (string, error)
	health   func(context.Context, string, navtoken.Record, string) error
	open     func(context.Context, string) (bool, error)
}

func cmdUI(args []string) error {
	return runUI(context.Background(), args, goos, os.Stdout, uiDependencies{})
}
func canonicalUIData(data string) bool {
	return utf8.ValidString(data) && filepath.IsAbs(data) && filepath.Clean(data) == data && !strings.ContainsRune(data, 0)
}

func runUI(parent context.Context, args []string, platform string, out io.Writer, d uiDependencies) error {
	o, err := parseUIArgs(args)
	if err != nil {
		return err
	}
	if o.mode == uiHelp {
		_, err = io.WriteString(out, uiHelpText)
		return err
	}
	if platform != "linux" && platform != "darwin" {
		return errUIPlatform
	}
	if parent == nil {
		return errUIUnavailable
	}
	if d.load == nil {
		d.load = config.Load
	}
	if d.data == nil {
		d.data = paths.DataDir
	}
	if d.cwd == nil {
		d.cwd = os.Getwd
	}
	if d.identity == nil {
		d.identity = daemon.IdentityContext
	}
	if d.health == nil {
		d.health = navhealth.Check
	}
	if d.open == nil {
		d.open = func(ctx context.Context, p string) (bool, error) { return openUIFile(ctx, platform, p) }
	}
	data := d.data()
	if !canonicalUIData(data) {
		return errUIDirectory
	}
	cfg, err := d.load()
	if err != nil || cfg == nil {
		return errUIConfig
	}
	if !cfg.UI.Navigator {
		return errors.New("navigator is off; enable [ui] navigator and restart the daemon")
	}
	if daemon.CheckListen(cfg.Daemon.Listen) != nil {
		return errUIUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return errUIUnavailable
	}
	record, err := navtoken.Read(filepath.Join(data, "ui-token"))
	if err != nil {
		return errUIUnavailable
	}
	switch record.Status() {
	case navtoken.Off:
		return errors.New("navigator is off in the running daemon; restart it after enabling [ui] navigator")
	case navtoken.Refused:
		return errors.New("navigator listener was refused; check the daemon configuration and restart it")
	case navtoken.Failed:
		return errors.New("navigator could not start; check the daemon and restart it")
	case navtoken.On:
	default:
		return errUIUnavailable
	}
	identity, err := d.identity(ctx)
	if err != nil || ctx.Err() != nil {
		return errUIUnavailable
	}
	if d.health(ctx, cfg.Daemon.Listen, record, identity) != nil || ctx.Err() != nil {
		return errUIUnavailable
	}
	client, err := navclient.New(record, daemon.NavigatorBuild())
	if err != nil {
		return errUIUnavailable
	}
	info, err := client.Info(ctx)
	if err != nil || ctx.Err() != nil {
		return errUIUnavailable
	}
	if !canonicalUIData(info.DataDir) {
		return errUIDirectory
	}
	if info.DataDir != data {
		return errUIUnavailable
	}
	if o.mode == uiInfo {
		_, err = fmt.Fprintf(out, "root: %s\ndata_dir: %s\nhostname: %s\n", strconv.Quote(info.Root), strconv.Quote(info.DataDir), strconv.Quote(info.Hostname))
		return err
	}
	if o.mode == uiRevoke {
		if client.Revoke(ctx) != nil || ctx.Err() != nil {
			return errUIUnavailable
		}
		_, err = io.WriteString(out, "All navigator browser logins and pending links revoked. Browser storage is cleared on its next authenticated request.\n")
		return err
	}
	directory := o.directory
	if directory == "" {
		directory, err = d.cwd()
	}
	if err == nil {
		directory, err = filepath.Abs(directory)
	}
	if err != nil {
		return navlaunch.ErrUnavailable
	}
	port, _ := record.Port()
	// Canonical zero bytes give the longest possible code shape without issuing.
	if _, err = navlaunch.Document(port, directory, strings.Repeat("A", 43)); err != nil {
		return navlaunch.ErrUnavailable
	}
	file, err := navlaunch.Prepare(ctx, data)
	if err != nil {
		return navlaunch.ErrUnavailable
	}
	retain := false
	defer func() { file.Finish(retain) }()
	code, err := client.Issue(ctx)
	if err != nil || ctx.Err() != nil {
		return errUIUnavailable
	}
	value, ok := code.Value()
	if !ok {
		return errUIUnavailable
	}
	body, err := navlaunch.Document(port, directory, value)
	if err != nil {
		return navlaunch.ErrUnavailable
	}
	name, err := file.Publish(ctx, body)
	if err != nil {
		return navlaunch.ErrUnavailable
	}
	retain, err = d.open(ctx, name)
	if err != nil {
		return errUIOpen
	}
	_, err = io.WriteString(out, "Navigator browser handoff dispatched.\n")
	return err
}

// A helper can exec the browser. Cancellation bounds observation only and must
// never signal that process. The sole waiter captures no credential or client.
func openUIFile(ctx context.Context, platform, name string) (started bool, err error) {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(name) {
		return false, errUIOpen
	}
	command := ""
	switch platform {
	case "darwin":
		command = "open"
	case "linux":
		command = "xdg-open"
	default:
		return false, errUIPlatform
	}
	cmd := exec.Command(command, name)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BROWSER=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	null, e := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if e != nil {
		return false, errUIOpen
	}
	defer null.Close()
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	if ctx.Err() != nil || cmd.Start() != nil {
		return false, errUIOpen
	}
	done := make(chan error, 1)
	go func(c *exec.Cmd) { done <- c.Wait() }(cmd)
	select {
	case <-ctx.Done():
		return true, errUIOpen
	case e := <-done:
		if e != nil || ctx.Err() != nil {
			return true, errUIOpen
		}
		return true, nil
	}
}
