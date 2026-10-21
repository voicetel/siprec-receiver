#!/usr/bin/env bash
# Live interop test: real FreeSWITCH + mod_siprec calls recorded by
# siprec-receiver. This is the end-to-end proof the unit tests cannot
# give — an actual sofia SRC INVITEing the SRS under test.
#
# Shape: FreeSWITCH runs in the mod-siprec-load container with host
# networking (so its SIPREC INVITEs and forked RTP reach the host);
# siprec-receiver runs on the host from a fresh build. check.py then
# asserts on the receiver's SQLite DB and WAV files.
#
# Prerequisites:
#   - the mod-siprec-load image (build once, ~30-45 min):
#       (cd ../mod_siprec && tests/load/run.sh)   # builds + load-gates
#   - docker reachable (set DOCKER="sudo docker" if your user is not
#     in the docker group)
#   - Go toolchain, python3
#
# Usage: tests/interop/run.sh
# Env: TAG (image, default mod-siprec-load), OUT (artifacts dir)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
tag=${TAG:-mod-siprec-load}
docker=${DOCKER:-docker}
out=${OUT:-$(mktemp -d)}
mkdir -p "$out/recordings"
ip=$(hostname -I | awk '{print $1}')

if ! $docker image inspect "$tag" >/dev/null 2>&1; then
    echo "image '$tag' not found — build it first:" >&2
    echo "  (cd ../mod_siprec && tests/load/run.sh)" >&2
    exit 1
fi

echo "==> building siprec-receiver"
(cd "$repo" && go build -o "$out/siprec-receiver" ./cmd/siprec-receiver)

echo "==> starting siprec-receiver (SIP ${ip}:5070, HTTP 127.0.0.1:18082)"
SIPREC_LISTEN=0.0.0.0:5070 SIPREC_NETWORKS=udp SIPREC_LOCAL_IP="$ip" \
RTP_PORT_MIN=47000 RTP_PORT_MAX=47100 \
RECORDING_DIR="$out/recordings" DB_PATH="$out/interop.db" \
HTTP_LISTEN=127.0.0.1:18082 PUBLIC_BASE_URL=http://127.0.0.1:18082 \
ADMIN_EMAIL=admin@example.com SESSION_SECRET=interop-session \
NOTIFY_SECRET=interop-secret \
    "$out/siprec-receiver" > "$out/receiver.log" 2>&1 &
recv=$!
trap 'kill "$recv" 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
    curl -sf http://127.0.0.1:18082/healthz >/dev/null 2>&1 && break
    sleep 0.5
done

echo "==> running FreeSWITCH + mod_siprec scenarios in container"
$docker run --rm --network host --cap-add=SYS_NICE \
    -v "$here:/interop:ro" -v "$out:/tmp/interop" \
    "$tag" bash /interop/interop.sh

echo "==> asserting on recordings"
python3 "$here/check.py" "$out/interop.db" http://127.0.0.1:18082 interop-secret
rc=$?
echo "artifacts: $out (receiver.log, fs.log, interop.db, recordings/)"
exit $rc
