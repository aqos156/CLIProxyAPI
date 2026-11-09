#!/usr/bin/env bash
set -euo pipefail

image=${1:?expected a local Hermes image tag}
[[ "$image" =~ ^hermes-ai/cliproxyapi:[a-zA-Z0-9._-]+$ ]]
cd /opt/hermes-ai-gateway
exec 9>/run/lock/hermes-ai-deploy.lock
flock -n 9
docker image inspect "$image" >/dev/null

backup=$(mktemp -d /root/hermes-ai-rollout.XXXXXX)
chmod 700 "$backup"
cp -p compose.yaml "$backup/compose.yaml"
trial="hermes-ai-trial-${backup##*.}"
stage=$(mktemp -d)
trap 'docker rm -f "$trial" >/dev/null 2>&1 || true; rm -rf -- "$stage"' EXIT

python3 - "$stage/config.yaml" <<'PY'
import json
from pathlib import Path
import secrets
import sys
Path(sys.argv[1]).write_text(json.dumps({
    "config-version": 8, "server": {"port": 8317},
    "routing": {"strategy": "soonest-reset", "session-affinity": True},
    "access": {"api-keys": [secrets.token_urlsafe(32)]},
}))
PY
docker run --detach --name "$trial" --memory 384m --cpus 1 --pids-limit 128 \
  --security-opt no-new-privileges:true --publish 127.0.0.1::8317 \
  --mount "type=bind,src=$stage/config.yaml,dst=/CLIProxyAPI/config.yaml" "$image" >/dev/null
trial_port=$(docker port "$trial" 8317/tcp | sed 's/.*://')
python3 - "$trial_port" "$stage/config.yaml" <<'PY'
import http.client
import json
from pathlib import Path
import sys
import time
key = json.loads(Path(sys.argv[2]).read_text())["access"]["api-keys"][0]
for _ in range(20):
    try:
        conn = http.client.HTTPConnection("127.0.0.1", int(sys.argv[1]), timeout=2)
        conn.request("GET", "/v1/models", headers={"Authorization": "Bearer "+key})
        response = conn.getresponse()
        if response.status == 200 and isinstance(json.loads(response.read()).get("data"), list):
            break
    except (OSError, ValueError):
        pass
    time.sleep(1)
else:
    raise SystemExit("Candidate did not pass the isolated API test")
conn = http.client.HTTPConnection("127.0.0.1", int(sys.argv[1]), timeout=2)
conn.request("GET", "/v1/models")
assert conn.getresponse().status == 401
PY
docker rm -f "$trial" >/dev/null

python3 - "$image" <<'PY'
import re
from pathlib import Path
import sys
p = Path("compose.yaml")
content, count = re.subn(r"(?m)(^  proxy:\n(?:    [^\n]+\n)*?    image: )[^\n]+", lambda match: match[1]+sys.argv[1], p.read_text())
assert count == 1, "Proxy image entry was not found exactly once"
p.write_text(content)
PY

rollback() {
  cp -p "$backup/compose.yaml" compose.yaml
  docker compose up -d --no-deps proxy </dev/null
}

if ! docker compose config --quiet </dev/null ||
   ! docker compose up -d --no-deps proxy </dev/null ||
   ! python3 - <<'PY'
from pathlib import Path
import base64
import http.client
import json
import os
import time

management = next(line.partition("=")[2] for line in Path("keeper.env").read_text().splitlines() if line.startswith("CPA_MANAGEMENT_KEY="))
auth = {"Authorization": "Bearer "+management, "Content-Type": "application/json"}
def request(port, path, headers=None, method="GET", data=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=4)
    try:
        conn.request(method, path, body=data, headers=headers or {})
        response = conn.getresponse()
        return response.status, response.read() if response.status != 101 else b""
    finally:
        conn.close()

for _ in range(20):
    try:
        status, raw = request(18317, "/v8/management/config/access/api-keys", auth)
        if status == 200:
            keys = json.loads(raw)
            if isinstance(keys, list) and keys and isinstance(keys[0], str):
                break
    except (OSError, ValueError):
        pass
    time.sleep(1)
else:
    raise SystemExit("Live management API did not recover")

for port in (18317, 18319):
    assert request(port, "/usage/")[0] == 200
    assert request(port, "/v1/models")[0] == 401
    device = {"Authorization": "Bearer "+keys[0]}
    assert request(port, "/v1/models", device)[0] == 200
    websocket = {**device, "Upgrade": "websocket", "Connection": "Upgrade",
                 "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": base64.b64encode(os.urandom(16)).decode()}
    assert request(port, "/v1/responses", websocket)[0] == 101
assert request(18319, "/management.html")[0] == 404
assert request(18317, "/management.html")[0] == 200
status, raw = request(18317, "/v0/management/routing/strategy", auth)
assert status == 200
if json.loads(raw).get("strategy") != "soonest-reset":
    assert request(18317, "/v8/management/config/routing/strategy", auth, "PUT", b'"soonest-reset"')[0] == 200
    status, raw = request(18317, "/v0/management/routing/strategy", auth)
    assert status == 200 and json.loads(raw).get("strategy") == "soonest-reset"
print("Private and public routes, API authentication, WebSockets and routing configuration passed.")
PY
then
  rollback
  exit 1
fi
printf 'Deployed %s. Previous Compose file is in %s.\n' "$image" "$backup"
