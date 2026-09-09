#!/usr/bin/env bash
set -euo pipefail

KEEP_GENERATIONS=10
HIGH_GENERATIONS=20
MIN_FREE_PERCENT=15
MAX_FREED_BYTES=10737418240
PROFILE_ROOT="${NIX_GC_PROFILE_ROOT:-}"
SYSTEM_PROFILE="${NIX_GC_SYSTEM_PROFILE:-/nix/var/nix/profiles/system}"
STORE_PATH="${NIX_GC_STORE_PATH:-/nix}"
NIX_ENV_BIN="${NIX_ENV_BIN:-nix-env}"
NIX_STORE_BIN="${NIX_STORE_BIN:-nix-store}"
SUDO_BIN="${SUDO_BIN:-sudo}"
SSH_BIN="${SSH_BIN:-ssh}"
DF_BIN="${DF_BIN:-df}"
FIND_BIN="${FIND_BIN:-find}"
ID_BIN="${ID_BIN:-id}"
SCRIPT_SOURCE="${NIX_GC_SCRIPT_SOURCE:-${BASH_SOURCE[0]:-$0}}"
AUTOMATIC_RETENTION=false
CHECK_ONLY=false
MANAGED_USER=""
MANAGED_HOME=""

usage() {
  cat <<'EOF'
Usage: nix-gc-maintenance [host]
       nix-gc-maintenance --automatic-retention --user <user> --home <path> [--check-only]

Manual mode retains 10 generations in the current user's Home Manager/user
profiles and the system profile, then garbage-collects up to 10 GiB.

With a host argument, the manual command is streamed to that host over SSH.
The managed SSH host must log in as a non-root user with sudo access.

Automatic retention mode is intended for the root-owned NixOS and Darwin
maintenance services. It prunes profiles from more than 20 generations to 10,
or prunes back to 10 when the Nix filesystem has less than 15% free. It validates
every current profile closure before changing any generation roots. --check-only
reports the decision without deleting generations.
EOF
}

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

resolve_common_commands() {
  need_command "$NIX_ENV_BIN"
  need_command "$NIX_STORE_BIN"
  need_command "$DF_BIN"
  need_command "$FIND_BIN"
  need_command "$ID_BIN"
  NIX_ENV_BIN=$(command -v "$NIX_ENV_BIN")
  NIX_STORE_BIN=$(command -v "$NIX_STORE_BIN")
  DF_BIN=$(command -v "$DF_BIN")
  FIND_BIN=$(command -v "$FIND_BIN")
  ID_BIN=$(command -v "$ID_BIN")
}

profile_exists() {
  [[ -e "$1" || -L "$1" ]]
}

resolve_user_profile_root() {
  if [[ -n "$PROFILE_ROOT" ]]; then
    return 0
  fi
  if [[ -n "${XDG_STATE_HOME:-}" ]]; then
    PROFILE_ROOT="$XDG_STATE_HOME/nix/profiles"
  elif [[ -n "${HOME:-}" ]]; then
    PROFILE_ROOT="$HOME/.local/state/nix/profiles"
  else
    die "could not determine the current user's profile directory"
  fi
}

profile_generation_count() {
  local profile="$1"
  local profile_dir profile_name

  profile_dir=$(dirname "$profile")
  profile_name=$(basename "$profile")
  "$FIND_BIN" "$profile_dir" -maxdepth 1 -type l \
    -name "${profile_name}-*-link" | wc -l | tr -d '[:space:]'
}

validate_profile() {
  local profile="$1"

  [[ -e "$profile" ]] || die "current profile is dangling: $profile"
  "$NIX_STORE_BIN" -qR "$profile" >/dev/null ||
    die "current profile closure is not valid: $profile"
}

free_percent() {
  local percent

  percent=$(
    "$DF_BIN" -Pk "$STORE_PATH" |
      awk 'NR == 2 && $2 > 0 { print int(($4 * 100) / $2) }'
  )
  [[ "$percent" =~ ^[0-9]+$ ]] ||
    die "could not determine filesystem capacity for $STORE_PATH"
  printf '%s\n' "$percent"
}

delete_user_profile_generations() {
  local profile="$1"

  if [[ "$AUTOMATIC_RETENTION" == true ]]; then
    "$SUDO_BIN" -H -u "$MANAGED_USER" "$NIX_ENV_BIN" \
      --profile "$profile" \
      --delete-generations "+${KEEP_GENERATIONS}"
  else
    "$NIX_ENV_BIN" \
      --profile "$profile" \
      --delete-generations "+${KEEP_GENERATIONS}"
  fi
}

run_privileged() {
  "$SUDO_BIN" "$@"
}

run_remote() {
  local host="$1"

  [[ "$host" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] ||
    die "invalid SSH host: $host"
  [[ -r "$SCRIPT_SOURCE" ]] || die "maintenance script is not readable: $SCRIPT_SOURCE"

  need_command "$SSH_BIN"
  SSH_BIN=$(command -v "$SSH_BIN")
  log "running Nix GC maintenance on $host"
  exec "$SSH_BIN" -tt "$host" bash -s <"$SCRIPT_SOURCE"
}

manual_maintenance() {
  local name profile

  [[ "$($ID_BIN -u)" -ne 0 ]] ||
    die "run as the managed user; this command requests sudo for system maintenance"
  resolve_user_profile_root

  need_command "$SUDO_BIN"
  SUDO_BIN=$(command -v "$SUDO_BIN")

  for name in home-manager profile; do
    profile="$PROFILE_ROOT/$name"
    if profile_exists "$profile"; then
      validate_profile "$profile"
      log "retaining the latest ${KEEP_GENERATIONS} generations in $profile"
      delete_user_profile_generations "$profile"
    fi
  done

  if profile_exists "$SYSTEM_PROFILE"; then
    validate_profile "$SYSTEM_PROFILE"
    log "retaining the latest ${KEEP_GENERATIONS} generations in $SYSTEM_PROFILE"
    run_privileged "$NIX_ENV_BIN" \
      --profile "$SYSTEM_PROFILE" \
      --delete-generations "+${KEEP_GENERATIONS}"
  fi

  log "garbage-collecting up to ${MAX_FREED_BYTES} bytes"
  run_privileged "$NIX_STORE_BIN" --gc --max-freed "$MAX_FREED_BYTES"
}

automatic_retention() {
  local name profile count free trigger=false action=false
  local -a existing_profiles=()
  local -a user_profiles=()

  [[ "$MANAGED_USER" =~ ^[A-Za-z_][A-Za-z0-9_-]*$ ]] ||
    die "invalid managed user: $MANAGED_USER"
  [[ "$MANAGED_HOME" == /* ]] || die "managed home must be an absolute path"
  PROFILE_ROOT="${NIX_GC_PROFILE_ROOT:-$MANAGED_HOME/.local/state/nix/profiles}"

  if [[ "$CHECK_ONLY" != true && "$($ID_BIN -u)" -ne 0 ]]; then
    die "automatic retention must run as root"
  fi
  need_command "$SUDO_BIN"
  SUDO_BIN=$(command -v "$SUDO_BIN")

  free=$(free_percent)
  log "Nix filesystem free space: ${free}% (minimum ${MIN_FREE_PERCENT}%)"
  if [[ "$free" -lt "$MIN_FREE_PERCENT" ]]; then
    trigger=true
    log "retention trigger: free space is below ${MIN_FREE_PERCENT}%"
  fi

  for name in home-manager profile; do
    profile="$PROFILE_ROOT/$name"
    if profile_exists "$profile"; then
      count=$(profile_generation_count "$profile")
      log "$profile has $count generations"
      existing_profiles+=("$profile")
      user_profiles+=("$profile")
      if [[ "$count" -gt "$HIGH_GENERATIONS" ]]; then
        trigger=true
        log "retention trigger: $profile exceeds ${HIGH_GENERATIONS} generations"
      fi
    fi
  done

  if profile_exists "$SYSTEM_PROFILE"; then
    count=$(profile_generation_count "$SYSTEM_PROFILE")
    log "$SYSTEM_PROFILE has $count generations"
    existing_profiles+=("$SYSTEM_PROFILE")
    if [[ "$count" -gt "$HIGH_GENERATIONS" ]]; then
      trigger=true
      log "retention trigger: $SYSTEM_PROFILE exceeds ${HIGH_GENERATIONS} generations"
    fi
  fi

  if [[ "$trigger" != true ]]; then
    log "retention not required"
    return 0
  fi

  for profile in "${existing_profiles[@]}"; do
    validate_profile "$profile"
  done

  for profile in "${user_profiles[@]}"; do
    count=$(profile_generation_count "$profile")
    if [[ "$count" -gt "$KEEP_GENERATIONS" ]]; then
      action=true
      if [[ "$CHECK_ONLY" == true ]]; then
        log "check-only: would retain ${KEEP_GENERATIONS} generations in $profile"
      else
        log "retaining the latest ${KEEP_GENERATIONS} generations in $profile"
        delete_user_profile_generations "$profile"
      fi
    fi
  done

  if profile_exists "$SYSTEM_PROFILE"; then
    count=$(profile_generation_count "$SYSTEM_PROFILE")
    if [[ "$count" -gt "$KEEP_GENERATIONS" ]]; then
      action=true
      if [[ "$CHECK_ONLY" == true ]]; then
        log "check-only: would retain ${KEEP_GENERATIONS} generations in $SYSTEM_PROFILE"
      else
        log "retaining the latest ${KEEP_GENERATIONS} generations in $SYSTEM_PROFILE"
        "$NIX_ENV_BIN" \
          --profile "$SYSTEM_PROFILE" \
          --delete-generations "+${KEEP_GENERATIONS}"
      fi
    fi
  fi

  if [[ "$action" != true ]]; then
    log "retention triggered, but all profiles already retain ${KEEP_GENERATIONS} generations or fewer"
  fi
}

parse_args() {
  if [[ $# -eq 1 && "$1" != -* ]]; then
    run_remote "$1"
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --automatic-retention)
        AUTOMATIC_RETENTION=true
        shift
        ;;
      --check-only)
        CHECK_ONLY=true
        shift
        ;;
      --user)
        [[ $# -ge 2 ]] || die "--user requires a value"
        MANAGED_USER="$2"
        shift 2
        ;;
      --home)
        [[ $# -ge 2 ]] || die "--home requires a value"
        MANAGED_HOME="$2"
        shift 2
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      *)
        usage >&2
        die "unknown argument: $1"
        ;;
    esac
  done
}

main() {
  parse_args "$@"
  resolve_common_commands

  if [[ "$AUTOMATIC_RETENTION" == true ]]; then
    [[ -n "$MANAGED_USER" ]] || die "--automatic-retention requires --user"
    [[ -n "$MANAGED_HOME" ]] || die "--automatic-retention requires --home"
    automatic_retention
  else
    [[ "$CHECK_ONLY" != true ]] || die "--check-only requires --automatic-retention"
    manual_maintenance
  fi
}

main "$@"
