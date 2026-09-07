package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// Install registers the daemon as a user service (launchd on macOS,
// systemd --user on Linux) and starts it. The service inherits the current
// PATH so it can find aiquota and the provider CLIs.
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
`, launchdLabel, xmlEscape(aiqPath), xmlEscape(os.Getenv("PATH")), xmlEscape(home), xmlEscape(LogPath()), xmlEscape(LogPath()))
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
`, aiqPath, os.Getenv("PATH"))
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
