package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const launchdLabel = "dev.aiq.daemon"

func launchdPlist() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func systemdUnit() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", "aiq.service")
}

// ServicePATH is the PATH baked into the service: the current one, then
// any entries only the user's interactive login shell adds. A takeover
// respawns `aiq run` with the daemon's environment, so a PATH captured from
// a bare ssh or cron shell (no nvm, no ~/.local/bin additions from .zshrc)
// leaves every successor with "claude not found on PATH".
func ServicePATH() string {
	return mergePATH(os.Getenv("PATH"), loginShellPATH())
}

func mergePATH(first, extra string) string {
	seen := map[string]bool{}
	var out []string
	for _, list := range []string{first, extra} {
		for _, d := range filepath.SplitList(list) {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// loginShellPATH asks $SHELL, as an interactive login shell, for its PATH.
// Markers fence the value off from whatever the rc files print.
func loginShellPATH() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-lic", `printf '\n__AIQ_PATH__%s__AIQ_PATH__\n' "$PATH"`)
	cmd.Stdin = nil
	out, _ := cmd.Output()
	_, rest, ok := strings.Cut(string(out), "__AIQ_PATH__")
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(rest, "__AIQ_PATH__")
	if !ok {
		return ""
	}
	return value
}

// Install registers the daemon as a user service (launchd on macOS,
// systemd --user on Linux) and starts it. The service gets ServicePATH so
// it, and every session it respawns, can find the provider CLIs.
func Install(aiqPath string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(aiqPath)
	case "linux":
		return installSystemd(aiqPath)
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

func Uninstall() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		plist := launchdPlist()
		exec.Command("launchctl", "unload", plist).Run()
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return plist, nil
	case "linux":
		unit := systemdUnit()
		exec.Command("systemctl", "--user", "disable", "--now", "aiq.service").Run()
		if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		return unit, nil
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

// Installed reports whether the service definition exists.
func Installed() (path string, ok bool) {
	switch runtime.GOOS {
	case "darwin":
		path = launchdPlist()
	case "linux":
		path = systemdUnit()
	default:
		return "", false
	}
	_, err := os.Stat(path)
	return path, err == nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func installLaunchd(aiqPath string) (string, error) {
	plist := launchdPlist()
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
    <string>run</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
    <key>HOME</key><string>%s</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, xmlEscape(aiqPath), xmlEscape(ServicePATH()), xmlEscape(home), xmlEscape(LogPath()), xmlEscape(LogPath()))
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return "", err
	}
	exec.Command("launchctl", "unload", plist).Run()
	if out, err := exec.Command("launchctl", "load", plist).CombinedOutput(); err != nil {
		return plist, fmt.Errorf("launchctl load: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return plist, nil
}

func installSystemd(aiqPath string) (string, error) {
	unit := systemdUnit()
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return "", err
	}
	content := fmt.Sprintf(`[Unit]
Description=aiq quota-aware account router
After=network-online.target

[Service]
ExecStart=%s daemon run
Environment=PATH=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, aiqPath, ServicePATH())
	if err := os.WriteFile(unit, []byte(content), 0o644); err != nil {
		return "", err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return unit, fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "aiq.service").CombinedOutput(); err != nil {
		return unit, fmt.Errorf("systemctl enable: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return unit, nil
}
