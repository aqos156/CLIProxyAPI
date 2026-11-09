#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
: "${HERMES_SSH_DEST:?set HERMES_SSH_DEST}"
: "${HERMES_SSH_KEY:?set HERMES_SSH_KEY}"
: "${HERMES_SSH_KNOWN_HOSTS:?set HERMES_SSH_KNOWN_HOSTS}"
ssh_hermes=(ssh -S none -o BatchMode=yes -o IdentitiesOnly=yes -i "$HERMES_SSH_KEY"
  -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$HERMES_SSH_KNOWN_HOSTS"
  -o ConnectTimeout=8 "$HERMES_SSH_DEST")
branch=refs/heads/codex/hermes-ai
lock="$(git rev-parse --absolute-git-dir)/hermes-update.lock"
mkdir "$lock" || { echo "Another Hermes update holds $lock." >&2; exit 1; }
temp=$(mktemp -d)
cleanup() {
  if [ -e "$temp/source/.git" ]; then
    git -C "$temp/source" rebase --abort >/dev/null 2>&1 || true
    git worktree remove "$temp/source"
  fi
  if [ -f "$temp/compile-check" ]; then unlink "$temp/compile-check"; fi
  rmdir "$temp" "$lock"
}
trap cleanup EXIT

"${ssh_hermes[@]}" 'sudo env DEBIAN_FRONTEND=noninteractive bash -ec "apt-get update -qq && unattended-upgrade"'
stable=$(gh api repos/router-for-me/CLIProxyAPI/releases/latest --jq .tag_name)
[[ "$stable" =~ ^v8\.[0-9]+\.[0-9]+$ ]] || { echo "Unexpected stable tag: $stable" >&2; exit 1; }
active=$("${ssh_hermes[@]}" 'image=$(sudo docker compose --project-directory /opt/hermes-ai-gateway images -q proxy | head -1) && test -n "$image" && sudo docker image inspect "$image" --format "{{index .Config.Labels \"ai.hermes.upstream\"}}"')
if [ "$stable" = "$active" ]; then
  printf 'Hermes proxy already uses stable %s. Debian security updates checked.\n' "$stable"
  exit 0
fi
[[ "$active" =~ ^v8\.[0-9]+\.[0-9]+$ ]] || { echo "The live image is missing its upstream release label." >&2; exit 1; }

git -c credential.helper= -c 'credential.helper=!gh auth git-credential' fetch --no-tags origin "$branch"
published=$(git rev-parse FETCH_HEAD)
git fetch --no-tags upstream "refs/tags/$stable:refs/tags/$stable" "refs/tags/$active:refs/tags/$active"
git worktree add --detach "$temp/source" "$published"
(
  cd "$temp/source"
  if ! git merge-base --is-ancestor "$stable" HEAD; then
    base=$(git describe --tags --abbrev=0 --match 'v8.[0-9]*' HEAD)
    git merge-base --is-ancestor "$base" "$stable"
    git -c commit.gpgsign=false rebase --onto "$stable" "$base"
  fi
  go test ./...
  go build -o "$temp/compile-check" ./cmd/server
  commit=$(git rev-parse HEAD)
  short=$(git rev-parse --short=12 HEAD)
  image="hermes-ai/cliproxyapi:$stable-$short"
  git archive HEAD | docker buildx build --platform linux/amd64 --load \
    --build-arg "VERSION=$stable-hermes" --build-arg "COMMIT=$commit" \
    --label "ai.hermes.upstream=$stable" --label "org.opencontainers.image.revision=$commit" -t "$image" -
  git -c credential.helper= -c 'credential.helper=!gh auth git-credential' push \
    --force-with-lease="$branch:$published" origin "$commit:$branch"
  docker save "$image" | gzip -1 | "${ssh_hermes[@]}" 'sudo docker load'
  "${ssh_hermes[@]}" "sudo bash -s -- $image" < ops/hermes/deploy.sh
)
