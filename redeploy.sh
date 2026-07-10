#!/usr/bin/env bash
# Rebuild the agent binary + all worker images (full + variants), pin the
# new full-image digest in serve.yaml, and restart the user service.
# Intended for a local bumblebee-style single-host deployment. Run from
# the agent repo root.
#
# Overridable env vars:
#   AGENT_CONFIG       path to serve.yaml
#                      (default ${XDG_CONFIG_HOME:-~/.config}/contextmatrix-agent/serve.yaml)
#   AGENT_WORKER_IMAGE image ref used for `docker inspect` (default contextmatrix-agent-worker:dev)
#   AGENT_SERVICE      systemd user unit name (default contextmatrix-agent)
set -euo pipefail

CONFIG="${AGENT_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/contextmatrix-agent/serve.yaml}"
WORKER_IMAGE="${AGENT_WORKER_IMAGE:-contextmatrix-agent-worker:dev}"
SERVICE="${AGENT_SERVICE:-contextmatrix-agent}"

# Repo portion of the image ref (strip the trailing :tag) — used to match
# the RepoDigest emitted by `docker image inspect`.
WORKER_REPO="${WORKER_IMAGE%:*}"

[ -f "$CONFIG" ] || {
  echo "ERROR: $CONFIG not found" >&2
  exit 1
}
[ -w "$CONFIG" ] || {
  echo "ERROR: $CONFIG not writable" >&2
  exit 1
}
grep -q '^base_image:' "$CONFIG" || {
  echo "ERROR: no active 'base_image:' line in $CONFIG to pin" >&2
  echo "       add 'base_image: ${WORKER_IMAGE}' (any value) before the first redeploy" >&2
  exit 1
}
command -v docker >/dev/null || {
  echo "ERROR: docker not in PATH" >&2
  exit 1
}
command -v systemctl >/dev/null || {
  echo "ERROR: systemctl not in PATH" >&2
  exit 1
}

echo "==> make build"
make build

echo "==> make docker-worker"
make docker-worker

# A ContextMatrix project can pin a variant tag via its worker_image
# override, which bypasses the digest-pinned base_image below — a stale
# variant would then run an old worker binary. Rebuild them all.
echo "==> make docker-worker-variants"
make docker-worker-variants

echo "==> capturing RepoDigest for ${WORKER_IMAGE}"
digest=$(docker image inspect "$WORKER_IMAGE" \
  --format '{{range .RepoDigests}}{{println .}}{{end}}' \
  | grep "^${WORKER_REPO}@sha256:" | head -n 1)
if [ -z "$digest" ]; then
  echo "ERROR: no ${WORKER_REPO}@sha256 RepoDigest on ${WORKER_IMAGE}" >&2
  echo "       rebuild produced an image without a digest — push to a registry or retag" >&2
  exit 1
fi
echo "    ${digest}"

echo "==> pinning base_image in ${CONFIG}"
# Replace the whole base_image value line. Uses | as the sed delimiter so
# the / in the image path does not need escaping. Whole-line replace works
# for both quoted and unquoted styles and only matches the active line
# (a '#'-commented line does not start with base_image:).
sed -i -E "s|^(base_image:[[:space:]]*).*|\\1${digest}|" "$CONFIG"
grep -E '^base_image:' "$CONFIG"

echo "==> systemctl --user restart ${SERVICE}"
systemctl --user restart "$SERVICE"
