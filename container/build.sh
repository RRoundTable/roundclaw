#!/usr/bin/env sh
# Build an agent image and promote it to TAG only if it actually runs.
#
# This is what "redeploy an agent image" is: agent turns are a fresh
# `docker run <tag>` each time, so retagging an image is the whole deployment —
# no compose restart. The gate matters because the admin agent runs on an agent
# image itself: promoting a broken one over a tag in use would brick its own
# next turn (and every agent on that tag), with recovery needing a host shell.
#
# So the build lands on a throwaway candidate tag first, is smoke-tested
# (`claude --version` — proves the binary is present and runnable), and only a
# passing candidate is retagged onto TAG. A failed build leaves TAG untouched.
#
# Usage:
#   container/build.sh [TAG] [CONTEXT]
#     TAG      image tag to promote to        (default: roundclaw/claude:latest)
#     CONTEXT  build context / Dockerfile dir (default: this script's directory)
#
# Build a per-agent variant instead of the fleet default — the safe path, since
# it never touches the image the admin or the rest of the fleet runs on:
#   container/build.sh roundclaw/claude-dev:latest container
set -eu

TAG="${1:-roundclaw/claude:latest}"
CONTEXT="${2:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)}"
CANDIDATE="${TAG%%:*}:candidate-$$"

cleanup() { docker image rm -f "$CANDIDATE" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "building $CANDIDATE from $CONTEXT"
docker build -t "$CANDIDATE" "$CONTEXT"

echo "smoke test: claude --version"
# --entrypoint clears the image's ENTRYPOINT so the version check is the whole
# command, with no credential and no network needed.
if ! docker run --rm --entrypoint claude "$CANDIDATE" --version; then
	echo "smoke test failed; $TAG left untouched" >&2
	exit 1
fi

echo "promoting $CANDIDATE -> $TAG"
docker tag "$CANDIDATE" "$TAG"
echo "done. next turn of any agent on $TAG uses the new image."
