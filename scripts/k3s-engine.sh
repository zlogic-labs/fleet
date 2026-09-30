#!/usr/bin/env bash
# Stage everything a k3s deployment of a real engine needs: the llama.cpp
# binary as a container image, and a real quantized model on the node.
#
# Separated from e2e.sh because it is the slow part and the result is
# reusable. Both steps are idempotent and skip when already done.
set -euo pipefail

FLEET_HOME=${FLEET_HOME:-/root/fleet}
BUILD=$FLEET_HOME/build
BIN=$FLEET_HOME/bin
WEIGHTS=${WEIGHTS:-$FLEET_HOME/weights}

LLAMA_TAG=${LLAMA_TAG:-b11146}
IMAGE=${IMAGE:-llama-cpp-local:$LLAMA_TAG}
REPO=${REPO:-Qwen/Qwen2.5-0.5B-Instruct-GGUF}
FILE=${FILE:-qwen2.5-0.5b-instruct-q4_k_m.gguf}
# 491 MB, Q4_K_M. The smallest useful quantization that is still a real
# published model rather than a fixture: 0.5B of parameters, and it answers
# in under a second per token on a CPU.
MODEL_DIR="$WEIGHTS/models/$REPO"

say() { printf '\n== %s\n' "$1"; }

# ---------------------------------------------------------------- engine image
# The listing is captured before it is searched. Piping ctr straight into
# "grep -q" looks equivalent and is not: grep exits on its first match, ctr
# dies of SIGPIPE, and under pipefail the pipeline reports failure — so the
# image is re-imported on every run and the script looks like it worked.
IMAGES=$(k3s ctr images ls 2>/dev/null || true)
if printf '%s' "$IMAGES" | grep -q "llama-cpp-local:$LLAMA_TAG"; then
  say "engine image already imported"
else
  say "downloading llama.cpp $LLAMA_TAG (linux/amd64, CPU)"
  mkdir -p "$BUILD"
  cd "$BUILD"
  if [ ! -d "llama-$LLAMA_TAG" ]; then
    curl -fsSL --retry 3 --retry-all-errors \
      -o ll.tar.gz \
      "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/llama-$LLAMA_TAG-bin-ubuntu-x64.tar.gz"
    tar xzf ll.tar.gz
    rm -f ll.tar.gz
  fi

  say "assembling a scratch image from the binary and its libraries"
  # There is no image builder in this WSL distribution and no registry image
  # worth pulling, so the image is built as a docker archive by hand. Scratch
  # works because the engine needs no shell, no package manager and no
  # configuration: the binary plus the libraries it links is the whole
  # runtime.
  ROOTFS=$BUILD/rootfs
  rm -rf "$ROOTFS" "$BUILD/archive"
  mkdir -p "$ROOTFS" "$BUILD/archive"
  SRC="$BUILD/llama-$LLAMA_TAG"

  cp "$SRC"/llama-server "$SRC"/*.so* "$ROOTFS"/
  for lib in $(ldd "$SRC/llama-server" | grep -oE '/[^ ]+\.so[^ ]*'); do
    mkdir -p "$ROOTFS$(dirname "$lib")"
    cp -L "$lib" "$ROOTFS$lib" 2>/dev/null || true
  done
  mkdir -p "$ROOTFS/lib64"
  cp -L /lib64/ld-linux-x86-64.so.2 "$ROOTFS/lib64/"

  # The diff id is the digest of the uncompressed layer stream, and
  # containerd verifies it on import, so it is computed from the tar rather
  # than from a listing of the files.
  tar -C "$ROOTFS" -cf "$BUILD/archive/layer.tar" .
  DIFF=$(sha256sum "$BUILD/archive/layer.tar" | cut -d' ' -f1)
  cat > "$BUILD/archive/config.json" <<EOF
{
  "architecture": "amd64",
  "os": "linux",
  "config": {
    "Env": ["PATH=/", "LD_LIBRARY_PATH=/:/lib/x86_64-linux-gnu"],
    "Entrypoint": ["/llama-server"]
  },
  "rootfs": {"type": "layers", "diff_ids": ["sha256:$DIFF"]}
}
EOF
  cat > "$BUILD/archive/manifest.json" <<EOF
[{"Config": "config.json", "RepoTags": ["$IMAGE"], "Layers": ["layer.tar"]}]
EOF
  tar -C "$BUILD/archive" -cf "$BUILD/engine-image.tar" config.json manifest.json layer.tar
  k3s ctr images import "$BUILD/engine-image.tar"
fi

# --------------------------------------------------------------------- weights
mkdir -p "$MODEL_DIR"
if [ -f "$MODEL_DIR/$FILE" ]; then
  say "model already staged"
else
  say "downloading $REPO/$FILE"
  # No public URL spelling survives a case-sensitive filesystem, and the
  # repository's filenames are lower case.
  curl -fL --http1.1 --retry 4 --retry-all-errors --retry-delay 2 -C - \
    -o "$MODEL_DIR/$FILE" \
    "https://huggingface.co/$REPO/resolve/main/$FILE"
fi

say "staged"
printf '  image:  %s\n' "$IMAGE"
printf '  model:  %s\n' "$MODEL_DIR/$FILE"
printf '  bytes:  %s\n' "$(stat -c%s "$MODEL_DIR/$FILE")"
