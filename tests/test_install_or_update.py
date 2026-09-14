"""Installer control-flow checks using isolated homes and fake system commands.

Run with: python3 -m unittest discover -s tests
"""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "install-or-update.sh"
FAKE_TOOL = r'''#!/usr/bin/env python3
import os
from pathlib import Path
import sys

name = Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["INSTALL_TEST_LOG"], "a") as log:
    log.write(name + " " + " ".join(args) + "\n")
if name == "uname":
    print(os.environ["INSTALL_TEST_OS"])
elif name == "id":
    print("1000")
elif name == "git":
    if args == ["status", "--porcelain"]:
        print(" M file" if os.environ.get("INSTALL_TEST_DIRTY") else "", end="")
    elif args == ["pull", "--ff-only"] and os.environ.get("INSTALL_TEST_PULL_FAIL"):
        sys.exit(1)
elif name == "go":
    if os.environ.get("INSTALL_TEST_BUILD_FAIL"):
        sys.exit(1)
    assert args[:2] == ["build", "-o"] and args[-1] == "./cmd/aiq"
    target = Path(args[2])
    target.write_text(Path(__file__).read_text())
    target.chmod(0o755)
elif name == "systemctl" and os.environ.get("INSTALL_TEST_SERVICE_FAIL"):
    sys.exit(1)
elif name == "aiq" and args == ["daemon", "status"]:
    print("daemon:  not answering" if os.environ.get("INSTALL_TEST_DAEMON_DOWN")
          else "daemon:  running at http://127.0.0.1:7379/")
'''


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "checkout with spaces"
        (self.repo / "cmd/aiq").mkdir(parents=True)
        (self.repo / "go.mod").write_text("module example.test/aiq\n")
        shutil.copy2(SCRIPT, self.repo / SCRIPT.name)
        self.home = self.root / "home"
        self.binary = self.home / ".local/bin/aiq"
        self.binary.parent.mkdir(parents=True)
        self.binary.write_text("old installed binary")
        self.bin = self.root / "tools"
        self.bin.mkdir()
        for name in ("git", "go", "uname", "id", "systemctl", "launchctl", "sleep"):
            tool = self.bin / name
            tool.write_text(FAKE_TOOL)
            tool.chmod(0o755)
        self.log = self.root / "commands.log"
        self.env = dict(os.environ, HOME=str(self.home),
                        PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        INSTALL_TEST_LOG=str(self.log), INSTALL_TEST_OS="Linux")

    def run_installer(self, *args):
        return subprocess.run(["bash", str(self.repo / SCRIPT.name), *args],
                              cwd=self.root, env=self.env,
                              text=True, capture_output=True, timeout=30)

    def commands(self):
        return self.log.read_text().splitlines() if self.log.exists() else []

    def test_ubuntu_update_restarts_after_install(self):
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = self.commands()
        self.assertEqual(commands.count("git pull --ff-only"), 1)
        install = commands.index("aiq daemon install")
        restart = commands.index("systemctl --user restart aiq.service")
        self.assertLess(install, restart)
        self.assertIn("systemctl --user is-active --quiet aiq.service", commands)
        self.assertIn("aiq shim install", commands)
        self.assertFalse(list(self.binary.parent.glob(".aiq-build.*")))

    def test_macos_install_uses_aiq_service_installer(self):
        self.env["INSTALL_TEST_OS"] = "Darwin"
        self.binary.unlink()  # First install also works.
        result = self.run_installer("--no-pull")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(os.access(self.binary, os.X_OK))
        self.assertIn("aiq daemon install", self.commands())
        self.assertIn("aiq daemon status", self.commands())
        self.assertFalse(any(c.startswith("systemctl ") for c in self.commands()))

    def test_no_daemon_and_no_pull(self):
        self.env["INSTALL_TEST_DIRTY"] = "1"
        result = self.run_installer("--no-pull", "--no-daemon")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("aiq shim install", self.commands())
        self.assertFalse(any(c.startswith(("git ", "systemctl ", "aiq daemon"))
                             for c in self.commands()))

    def test_build_failure_preserves_installed_binary(self):
        self.env["INSTALL_TEST_BUILD_FAIL"] = "1"
        result = self.run_installer("--no-pull")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.binary.read_text(), "old installed binary")
        self.assertFalse(list(self.binary.parent.glob(".aiq-build.*")))
        self.assertNotIn("aiq daemon install", self.commands())

    def test_dirty_checkout_refused(self):
        self.env["INSTALL_TEST_DIRTY"] = "1"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("local changes", result.stderr)
        self.assertNotIn("git pull --ff-only", self.commands())
        self.assertEqual(self.binary.read_text(), "old installed binary")

    def test_pull_failure_stops_install(self):
        self.env["INSTALL_TEST_PULL_FAIL"] = "1"
        self.assertNotEqual(self.run_installer().returncode, 0)
        self.assertEqual(self.binary.read_text(), "old installed binary")

    def test_service_manager_failure_stops_before_build(self):
        self.env["INSTALL_TEST_SERVICE_FAIL"] = "1"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("systemd user manager", result.stderr)
        self.assertEqual(self.binary.read_text(), "old installed binary")

    def test_invalid_option(self):
        result = self.run_installer("--unknown")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unknown argument", result.stderr)

    def test_daemon_not_ready_is_an_error(self):
        self.env["INSTALL_TEST_DAEMON_DOWN"] = "1"
        result = self.run_installer("--no-pull")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("daemon did not become ready", result.stderr)
        self.assertNotIn("Installation complete", result.stdout)


if __name__ == "__main__":
    unittest.main()
