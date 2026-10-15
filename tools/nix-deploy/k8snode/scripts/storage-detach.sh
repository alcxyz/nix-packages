set -euo pipefail
shopt -s nullglob

command -v findmnt >/dev/null
command -v iscsiadm >/dev/null
command -v lsof >/dev/null
command -v lsblk >/dev/null
command -v sed >/dev/null
command -v udevadm >/dev/null

mounts=$(
  findmnt -rn -o TARGET \
    | grep -E '^/var/lib/kubelet/plugins/kubernetes.io/csi/driver\.longhorn\.io/.+/globalmount$' \
    || true
)

if [[ -n "$mounts" ]]; then
  printf 'Residual Longhorn mounts:\n%s\n' "$mounts" >&2
  exit 1
fi

mapfile -t sessions < <(
  iscsiadm -m session 2>/dev/null \
    | awk '$4 ~ /^iqn\.2019-10\.io\.longhorn:/ { print $2, $3, $4 }'
)

if ((${#sessions[@]} == 0)); then
  exit 0
fi

printf 'Found %d detached Longhorn iSCSI session(s); validating host use\n' \
  "${#sessions[@]}" >&2

for session in "${sessions[@]}"; do
  read -r sid portal target <<<"$session"
  sid=${sid#[}
  sid=${sid%]}

  [[ "$sid" =~ ^[0-9]+$ ]] || {
    printf 'Unsafe iSCSI session ID: %s\n' "$sid" >&2
    exit 1
  }
  [[ "$portal" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:3260,1$ ]] || {
    printf 'Unsafe Longhorn iSCSI portal: %s\n' "$portal" >&2
    exit 1
  }
  [[ "$target" =~ ^iqn\.2019-10\.io\.longhorn:pvc-[0-9a-f-]+$ ]] || {
    printf 'Unsafe Longhorn iSCSI target: %s\n' "$target" >&2
    exit 1
  }

  session_path="/sys/class/iscsi_session/session${sid}"
  [[ -r "$session_path/targetname" ]] || {
    printf 'Missing sysfs state for Longhorn iSCSI session %s\n' "$sid" >&2
    exit 1
  }
  [[ "$(<"$session_path/targetname")" == "$target" ]] || {
    printf 'iSCSI session %s target changed while validating\n' "$sid" >&2
    exit 1
  }

  block_paths=("$session_path"/device/target*/*/block/*)
  for block_path in "${block_paths[@]}"; do
    device=${block_path##*/}
    [[ "$device" =~ ^[A-Za-z0-9._-]+$ && -b "/dev/$device" ]] || {
      printf 'Unsafe block device for iSCSI session %s: %s\n' "$sid" "$device" >&2
      exit 1
    }

    while IFS= read -r related_device; do
      [[ "$related_device" =~ ^[A-Za-z0-9._-]+$ ]] || exit 1

      if lsblk -nr -o MOUNTPOINTS "/dev/$related_device" | grep -q '[^[:space:]]'; then
        printf 'Longhorn iSCSI device /dev/%s is still mounted\n' "$related_device" >&2
        exit 1
      fi

      if [[ -d "/sys/class/block/$related_device/holders" ]] \
        && find "/sys/class/block/$related_device/holders" -mindepth 1 -maxdepth 1 -print -quit \
          | grep -q .; then
        printf 'Longhorn iSCSI device /dev/%s still has block holders\n' "$related_device" >&2
        exit 1
      fi

      if lsof -t -- "/dev/$related_device" 2>/dev/null | grep -q .; then
        printf 'Longhorn iSCSI device /dev/%s is still open\n' "$related_device" >&2
        exit 1
      fi
    done < <(lsblk -nr -o NAME "/dev/$device")
  done
done

if [[ "$cleanup_sessions" != true ]]; then
  printf 'Detached Longhorn iSCSI sessions are safe to reconcile during maintenance\n' >&2
  exit 0
fi

printf 'Reconciling %d detached Longhorn iSCSI session(s)\n' "${#sessions[@]}" >&2
for session in "${sessions[@]}"; do
  read -r sid portal target <<<"$session"
  sid=${sid#[}
  sid=${sid%]}
  portal_address=${portal%,1}
  record_portal=${portal/:3260,1/,3260,1}
  record="/etc/iscsi/nodes/$target/$record_portal/default"

  # Open-iSCSI 2.1.12 rejects records created by older releases when they
  # contain this removed key. Migrate only the exact validated record before
  # asking iscsiadm to log out and remove it.
  if [[ -f "$record" ]]; then
    sed -i '/^node\.session\.conn_reopen_log_freq[[:space:]]*=/d' "$record"
  fi

  iscsiadm -m node -T "$target" -p "$portal_address" --logout
  iscsiadm -m node -T "$target" -p "$portal_address" --op=delete
done

udevadm settle

remaining=$(iscsiadm -m session 2>/dev/null | grep -F 'iqn.2019-10.io.longhorn:' || true)
if [[ -n "$remaining" ]]; then
  printf 'Longhorn iSCSI sessions remain after reconciliation:\n%s\n' "$remaining" >&2
  exit 1
fi
