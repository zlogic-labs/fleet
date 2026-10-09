#!/usr/bin/env bash
# Build the three Fleet binaries and import them into k3s as scratch images.
#
# There is no docker or buildah on this host and no image worth pulling for a
# Go binary that links nothing: the image is assembled as a docker archive by
# hand, the same way scripts/k3s-engine.sh builds the engine image. The CA
# bundle is the one file the control plane needs beyond the binary, because it
# fetches model repositories over https.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SERVING=${FLEET_SERVING_DIR:-$ROOT/../fleet-serving}
TAG=${TAG:-dev}
SUDO_PASSWORD=${SUDO_PASSWORD:-root}

command -v go >/dev/null 2>&1 || export PATH=/usr/local/go/bin:$PATH
command -v npm >/dev/null 2>&1 || { echo "npm is required to build the console"; exit 1; }

say() { printf '\n== %s\n' "$1"; }

# ------------------------------------------------------------------- console
# The console is embedded in fleet-gateway at compile time, so a rebuild that
# skips this step ships whatever bundle was lying around.
if [ ! -f "$ROOT/web/dist/index.html" ] ||
   [ -n "$(find "$ROOT/web/src" "$ROOT/web/index.html" "$ROOT/web/package.json" \
        -newer "$ROOT/web/dist/index.html" 2>/dev/null)" ]; then
  say "building the console"
  npm --prefix "$ROOT/web" run build
else
  say "console bundle is current"
fi
rm -rf "$ROOT/core/internal/gateway/webui/dist"
mkdir -p "$ROOT/core/internal/gateway/webui/dist"
touch "$ROOT/core/internal/gateway/webui/dist/.gitkeep"
cp -R "$ROOT/web/dist/." "$ROOT/core/internal/gateway/webui/dist/"

# ------------------------------------------------------------------ binaries
say "building the binaries"
STAGE=$(mktemp -d /tmp/fleet-images.XXXXXX)
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/bin"
VERSION=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)

(cd "$ROOT/core" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags "-X main.version=$VERSION" -o "$STAGE/bin/fleet-gateway" ./cmd/fleet-gateway)
(cd "$ROOT/core" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags "-X main.version=$VERSION" -o "$STAGE/bin/fleet-apiserver" ./cmd/fleet-apiserver)

# fleet-serving joins this repository's core through a gitignored go.work. It
# is created here rather than documented as a prerequisite, because a checkout
# that builds against the pinned release instead of the sibling tree compiles
# against a core that no longer matches the controller.
if [ ! -f "$SERVING/go.work" ]; then
  say "writing $SERVING/go.work"
  printf 'go 1.26.4\n\nuse .\n\nreplace github.com/zlogic-labs/fleet/core => %s/core\n' "$ROOT" > "$SERVING/go.work"
fi
(cd "$SERVING" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags "-X main.Version=$VERSION" -o "$STAGE/bin/fleet-operator" ./cmd/manager)

# -------------------------------------------------------------------- images
build_image() {
  local name=$1 entry=$2
  local rootfs="$STAGE/rootfs-$name" archive="$STAGE/archive-$name"
  rm -rf "$rootfs" "$archive"
  mkdir -p "$rootfs/etc/ssl/certs" "$archive"
  cp "$STAGE/bin/$name" "$rootfs/$entry"
  cp /etc/ssl/certs/ca-certificates.crt "$rootfs/etc/ssl/certs/"

  # The diff id is the digest of the uncompressed layer stream, and containerd
  # verifies it on import, so it comes from the tar rather than from a listing.
  tar -C "$rootfs" -cf "$archive/layer.tar" .
  local diff
  diff=$(sha256sum "$archive/layer.tar" | cut -d' ' -f1)
  cat > "$archive/config.json" <<EOF
{
  "architecture": "amd64",
  "os": "linux",
  "config": {
    "Env": ["PATH=/"],
    "Entrypoint": ["/$entry"]
  },
  "rootfs": {"type": "layers", "diff_ids": ["sha256:$diff"]}
}
EOF
  cat > "$archive/manifest.json" <<EOF
[{"Config": "config.json", "RepoTags": ["$name:$TAG"], "Layers": ["layer.tar"]}]
EOF
  tar -C "$archive" -cf "$STAGE/$name.tar" config.json manifest.json layer.tar
}

say "assembling scratch images"
for name in fleet-gateway fleet-apiserver fleet-operator; do
  build_image "$name" "$name"
done

# -------------------------------------------------------------------- import
run_ctr() {
  if [ -w /run/k3s/containerd/containerd.sock ]; then
    k3s ctr "$@"
  elif sudo -n true 2>/dev/null; then
    sudo k3s ctr "$@"
  else
    printf '%s\n' "$SUDO_PASSWORD" | sudo -S -p '' k3s ctr "$@"
  fi
}

say "importing into k3s"
for name in fleet-gateway fleet-apiserver fleet-operator; do
  run_ctr images import "$STAGE/$name.tar"
  printf '  %s:%s\n' "$name" "$TAG"
done
say "done (version $VERSION); redeploy with: kubectl -n fleet rollout restart deploy"
