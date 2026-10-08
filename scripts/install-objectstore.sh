#!/usr/bin/env bash
# Install an S3-compatible object store inside WSL and serve the bucket Fleet
# reads, so the S3 path in core/internal/blobstore can finally be run against a
# real server instead of only blobstore.FS.
#
# MinIO is what the dependency's name suggests, but it can no longer be
# installed: every dl.min.io path answers 410, the GitHub release carries no
# binary assets, and quay.io answers 401 for the image. So this uses SeaweedFS,
# which is a different implementation. That is not a downgrade for this purpose
# -- it is a stronger test, because code that passes against one S3 server and
# fails against another has been leaning on that server's quirks.
set -euo pipefail

BIN=/usr/local/bin/weed
VER=4.48
DATA=${S3_DATA:-/var/lib/fleet-s3}
PORT=${S3_PORT:-9000}

if [ ! -x "$BIN" ]; then
  echo "== downloading seaweedfs $VER"
  tmp=$(mktemp -d)
  curl -fsSL --http1.1 --retry 5 --retry-all-errors -o "$tmp/weed.tgz" \
    "https://github.com/seaweedfs/seaweedfs/releases/download/$VER/linux_amd64.tar.gz"
  tar -xzf "$tmp/weed.tgz" -C "$tmp"
  install -m 0755 "$tmp/weed" "$BIN"
  rm -rf "$tmp"
fi

"$BIN" version
mkdir -p "$DATA"

# S3 credentials live in a file because the S3 gateway reads them at startup
# and there is no flag for them.
cat > "$DATA/s3.json" <<JSON
{
  "identities": [
    {
      "name": "fleetadmin",
      "credentials": [
        { "accessKey": "fleetadmin", "secretKey": "fleet-secret-key-01" }
      ],
      "actions": ["Admin", "Read", "Write", "List", "Tagging"]
    }
  ]
}
JSON

echo "== writing the unit"
# The heredoc is unquoted so $DATA and $PORT expand here, which means ${S3_CONFIG}
# would be expanded here too -- and `set -u` turns that into an unbound-variable
# abort. Escaping it hands systemd the literal text instead.
cat > /etc/systemd/system/fleet-s3.service <<UNIT
[Unit]
Description=S3-compatible object store for Fleet's blobstore backend
After=network.target

[Service]
Environment=S3_CONFIG=$DATA/s3.json
# The volume server defaults to port 8080, which is fleet-gateway. Binding the
# advertised IP to loopback also avoids SeaweedFS defaulting to 10.255.255.254,
# an address that does not exist on this host and leaves it retrying a master it
# can never reach.
ExecStart=$BIN server -dir=$DATA -ip=127.0.0.1 -master.port=19333 -volume.port=18080 -s3 -s3.port=$PORT -s3.config=\${S3_CONFIG}
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable fleet-s3 >/dev/null
systemctl restart fleet-s3

for _ in $(seq 1 60); do
  if ss -ltn 2>/dev/null | grep -q ":$PORT "; then break; fi
  sleep 1
done

# Assert the port is actually bound before claiming success. "systemd says
# active" only means the process has not exited yet, and this process spends a
# while retrying a master it cannot reach -- an earlier version of this script
# printed a success line in that state.
ss -ltn | grep -q ":$PORT " || { journalctl -u fleet-s3 --no-pager -n 20; exit 1; }

echo "== serving S3 on 127.0.0.1:$PORT as fleetadmin/fleet-secret-key-01"