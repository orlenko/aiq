#!/usr/bin/env bash
# Run from an aiq checkout on macOS or Linux. No sudo required.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: install-or-update.sh [--no-pull] [--no-daemon]

Fast-forward this checkout, build aiq into ~/.local/bin, refresh its shims,
and install/restart the current user's daemon (launchd or systemd).

  --no-pull    Build the current checkout, including local changes.
  --no-daemon  Install the binary and shims without changing the user service.
  -h, --help   Show this help.

Requires Git, Go (see go.mod), and launchctl or systemctl for the daemon.
Run as your regular login user, not with sudo.
EOF
}

fail() { printf 'aiq install: %s\n' "$*" >&2; exit 1; }

pull=true
daemon=true
for arg in "$@"; do
  case "$arg" in
    --no-pull) pull=false ;;
    --no-daemon) daemon=false ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; fail "unknown argument: $arg" ;;
  esac
done

case "$(uname -s)" in
  Darwin) service_command=launchctl ;;
  Linux) service_command=systemctl ;;
  *) fail 'supported platforms are macOS and Linux with systemd' ;;
esac
[[ $(id -u) != 0 ]] || fail 'run as your regular login user, not root'
for command in git go; do
  command -v "$command" >/dev/null || fail "$command is required; install it and retry"
done
if "$daemon"; then
  command -v "$service_command" >/dev/null || fail "$service_command is required (or use --no-daemon)"
  if [[ $service_command == systemctl ]]; then
    systemctl --user show-environment >/dev/null || fail 'cannot reach the systemd user manager; run in a user login session or use --no-daemon'
  fi
fi

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
cd -- "$repo_dir"
[[ -f go.mod && -d cmd/aiq ]] || fail 'run this script from an aiq checkout'

if "$pull"; then
  [[ -z $(git status --porcelain) ]] || fail 'checkout has local changes; commit/stash them or use --no-pull'
  git symbolic-ref --quiet HEAD >/dev/null || fail 'checkout is detached; switch to a tracking branch or use --no-pull'
  git rev-parse --verify '@{upstream}' >/dev/null 2>&1 || fail 'branch has no upstream; configure it or use --no-pull'
  printf 'Updating %s\n' "$repo_dir"
  git pull --ff-only
  # Pull may update this script too. Re-exec it before installing anything.
  exec bash "$repo_dir/install-or-update.sh" --no-pull "$@"
fi

install_dir="$HOME/.local/bin"
mkdir -p -- "$install_dir"
build_dir=$(mktemp -d "$install_dir/.aiq-build.XXXXXX")
trap 'rm -rf -- "$build_dir"' EXIT
printf 'Building aiq in %s\n' "$repo_dir"
go build -o "$build_dir/aiq" ./cmd/aiq
chmod 755 "$build_dir/aiq"
# Rename on the same filesystem: a running Linux binary cannot be overwritten.
mv -f -- "$build_dir/aiq" "$install_dir/aiq"
printf 'Installed %s\n' "$install_dir/aiq"

"$install_dir/aiq" shim install
if "$daemon"; then
  "$install_dir/aiq" daemon install
  if [[ $service_command == systemctl ]]; then
    # enable --now starts a new service but does not restart an existing one.
    systemctl --user restart aiq.service
    systemctl --user is-active --quiet aiq.service
  fi
  # Service registration returns before the daemon's HTTP listener is ready.
  ready=false
  for ((attempt = 0; attempt < 15; attempt++)); do
    daemon_status=$("$install_dir/aiq" daemon status)
    if [[ $daemon_status == *"daemon:  running at "* ]]; then
      ready=true
      break
    fi
    sleep 1
  done
  printf '%s\n' "$daemon_status"
  "$ready" || fail 'daemon did not become ready; check the log shown above'
fi
printf '\nInstallation complete. Keep ~/.local/bin and the shim directory on PATH.\n'
