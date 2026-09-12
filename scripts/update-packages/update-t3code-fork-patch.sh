#!/usr/bin/env bash
# Regenerates the checked-in T3 Code feature patch from an explicit checkout.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 /path/to/t3code-checkout prerequisite-base-revision" >&2
  exit 2
fi

checkout=$(realpath "$1")
prerequisite_base_input="$2"
package_root=$(realpath "$(dirname "$0")/../..")
destination="$package_root/pkgs/t3code/patches/automatic-thread-titles.patch"
prerequisite_patch="$package_root/pkgs/t3code/patches/upstream-pr-10720.patch"
base_revision=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["revision"])' "$package_root/pkgs/t3code/source.json")

if [[ ! "$base_revision" =~ ^[0-9a-f]{40}$ ]]; then
  echo "unable to read one pinned base revision from pkgs/t3code/source.json" >&2
  exit 1
fi

paths=(
  apps/mobile/src/features/settings/SettingsRouteScreen.logic.test.ts
  apps/mobile/src/features/settings/SettingsRouteScreen.logic.ts
  apps/mobile/src/features/settings/SettingsRouteScreen.tsx
  apps/mobile/src/features/threads/threadPresentation.ts
  apps/mobile/src/lib/projectThreadStartTurn.ts
  apps/server/src/environment/ServerEnvironment.test.ts
  apps/server/src/environment/ServerEnvironment.ts
  apps/server/src/mcp/McpHttpServer.ts
  apps/server/src/mcp/McpInvocationContext.ts
  apps/server/src/mcp/McpSessionRegistry.test.ts
  apps/server/src/mcp/McpSessionRegistry.ts
  apps/server/src/mcp/toolkits/threadTitle/handlers.test.ts
  apps/server/src/mcp/toolkits/threadTitle/handlers.ts
  apps/server/src/mcp/toolkits/threadTitle/tools.ts
  apps/server/src/orchestration/AutomaticThreadTitleRateLimit.test.ts
  apps/server/src/orchestration/AutomaticThreadTitleRateLimit.ts
  apps/server/src/orchestration/Layers/ProjectionPipeline.test.ts
  apps/server/src/orchestration/Layers/ProjectionPipeline.ts
  apps/server/src/orchestration/Layers/ProjectionSnapshotQuery.test.ts
  apps/server/src/orchestration/Layers/ProjectionSnapshotQuery.ts
  apps/server/src/orchestration/Layers/ProviderCommandReactor.test.ts
  apps/server/src/orchestration/Layers/ProviderCommandReactor.ts
  apps/server/src/orchestration/Layers/ProviderRuntimeIngestion.ts
  apps/server/src/orchestration/Services/ProjectionSnapshotQuery.ts
  apps/server/src/orchestration/ThreadTitlePolicy.ts
  apps/server/src/orchestration/decider.titleOwnership.test.ts
  apps/server/src/orchestration/decider.titleRegeneration.test.ts
  apps/server/src/orchestration/decider.ts
  apps/server/src/orchestration/projector.ts
  apps/server/src/persistence/Layers/AutomaticThreadTitleRenameQuery.ts
  apps/server/src/persistence/Layers/ProjectionThreads.ts
  apps/server/src/persistence/Migrations.ts
  apps/server/src/persistence/Migrations/050_ProjectionThreadTitleState.ts
  apps/server/src/persistence/Migrations/ForkProjectionThreadTitleSource.test.ts
  apps/server/src/persistence/Migrations/ForkProjectionThreadTitleSource.ts
  apps/server/src/persistence/Services/AutomaticThreadTitleRenameQuery.ts
  apps/server/src/persistence/Services/ProjectionThreads.ts
  apps/server/src/provider/CodexDeveloperInstructions.ts
  apps/server/src/provider/Layers/AntigravityAdapter.ts
  apps/server/src/provider/Layers/ClaudeAdapter.ts
  apps/server/src/provider/Layers/CodexAdapter.ts
  apps/server/src/provider/Layers/CodexSessionRuntime.test.ts
  apps/server/src/provider/Layers/CodexSessionRuntime.ts
  apps/server/src/provider/Layers/CursorAdapter.ts
  apps/server/src/provider/Layers/GrokAdapter.ts
  apps/server/src/provider/Layers/OpenCodeAdapter.ts
  apps/server/src/provider/Layers/ProviderService.test.ts
  apps/server/src/provider/Layers/ProviderService.ts
  apps/server/src/provider/RuntimeInstructions.test.ts
  apps/server/src/provider/RuntimeInstructions.ts
  apps/server/src/provider/Services/ProviderAdapter.ts
  apps/server/src/server.ts
  apps/server/src/serverRuntimeStartup.ts
  apps/server/src/serverSettings.test.ts
  apps/server/src/ws.ts
  apps/web/src/components/ChatView.tsx
  apps/web/src/components/LegacySidebar.tsx
  apps/web/src/components/Sidebar.logic.test.ts
  apps/web/src/components/Sidebar.logic.ts
  apps/web/src/components/Sidebar.tsx
  apps/web/src/components/chat/ChatHeader.tsx
  apps/web/src/components/settings/SettingsPanels.tsx
  apps/web/src/components/settings/settingsSearch.test.ts
  apps/web/src/components/settings/settingsSearch.ts
  apps/web/src/components/settings/useAvailableSettingsSearchItems.ts
  docs/user/thread-sidebar.md
  packages/client-runtime/src/state/sharedSettings.test.ts
  packages/client-runtime/src/state/sharedSettings.ts
  packages/client-runtime/src/state/threadDetail.ts
  packages/client-runtime/src/state/threadReducer.ts
  packages/contracts/src/environment.ts
  packages/contracts/src/orchestration.ts
  packages/contracts/src/settings.test.ts
  packages/contracts/src/settings.ts
  packages/shared/src/serverSettings.test.ts
  packages/shared/src/serverSettings.ts
)

if ! checkout_root=$(git -C "$checkout" rev-parse --show-toplevel 2>/dev/null); then
  echo "not a Git checkout: $checkout" >&2
  exit 1
fi
checkout=$(realpath "$checkout_root")

if ! git -C "$checkout" cat-file -e "${base_revision}^{commit}"; then
  echo "base revision is unavailable in checkout: $base_revision" >&2
  exit 1
fi

if ! prerequisite_base_revision=$(git -C "$checkout" rev-parse --verify "${prerequisite_base_input}^{commit}"); then
  echo "prerequisite base revision is unavailable in checkout: $prerequisite_base_input" >&2
  exit 1
fi

if ! git -C "$checkout" merge-base --is-ancestor "$base_revision" "$prerequisite_base_revision"; then
  echo "pinned source is not an ancestor of prerequisite base: $prerequisite_base_revision" >&2
  exit 1
fi

if ! git -C "$checkout" merge-base --is-ancestor "$prerequisite_base_revision" HEAD; then
  echo "prerequisite base is not an ancestor of checkout HEAD: $prerequisite_base_revision" >&2
  exit 1
fi

prerequisite_temporary=$(mktemp)
temporary=$(mktemp)
trap 'rm -f "$prerequisite_temporary" "$temporary"' EXIT

git -C "$checkout" diff --binary --no-ext-diff \
  "$base_revision" "$prerequisite_base_revision" -- >"$prerequisite_temporary"
if ! cmp --silent "$prerequisite_patch" "$prerequisite_temporary"; then
  echo "prerequisite base does not match $prerequisite_patch" >&2
  exit 1
fi

declare -A allowed=()
for path in "${paths[@]}"; do
  allowed["$path"]=1
done

mapfile -t changed < <(
  {
    git -C "$checkout" diff --name-only "$prerequisite_base_revision" --
    git -C "$checkout" ls-files --others --exclude-standard
  } | sort -u
)

for path in "${changed[@]}"; do
  if [[ -n "$path" && ! -v "allowed[$path]" ]]; then
    echo "refusing to include unrelated checkout change: $path" >&2
    exit 1
  fi
done

git -C "$checkout" diff --binary --no-ext-diff "$prerequisite_base_revision" -- "${paths[@]}" >"$temporary"
while IFS= read -r path; do
  git -C "$checkout" diff --binary --no-index -- /dev/null "$path" >>"$temporary" || status=$?
  if [[ ${status:-0} -ne 1 ]]; then
    echo "failed to render untracked file: $path" >&2
    exit 1
  fi
  unset status
done < <(git -C "$checkout" ls-files --others --exclude-standard -- "${paths[@]}")

if [[ ! -s "$temporary" ]]; then
  echo "feature patch would be empty" >&2
  exit 1
fi

mv "$temporary" "$destination"
rm -f "$prerequisite_temporary"
trap - EXIT
echo "updated $destination"
