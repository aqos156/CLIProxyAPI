#!/usr/bin/env bash
set -euo pipefail
cd /opt/hermes-ai-gateway
exec 9>/run/lock/hermes-ai-deploy.lock
flock -n 9
backup=$(mktemp -d /root/hermes-ai-hub-edge.XXXXXX)
cp -p nginx.conf "$backup/nginx.conf"
cat > "$backup/public-hub.conf"

python3 - "$backup/public-hub.conf" <<'PY'
from pathlib import Path
from textwrap import indent
import sys

nginx = Path("nginx.conf")
config = nginx.read_text()
start = "  # BEGIN Hermes public hub limits\n"
end = "  # END Hermes public hub limits\n"
if start in config:
    before, _, rest = config.partition(start)
    _, marker, after = rest.partition(end)
    assert marker, "Incomplete public hub markers"
    config = before + after
zone = "  limit_req_zone $server_name zone=public_hub_reads:64k rate=60r/m;\n"
config = config.replace(zone, "")
public = "  server {\n    listen 8082;\n    server_name hermes-ai-public;\n"
assert config.count(public) == 1
routes = Path(sys.argv[1]).read_text()
assert "proxy_pass http://$hub_backend/hermes-hub/auth-files;" in routes
assert "proxy_pass http://$hub_backend/hermes-hub/api-call;" in routes
nginx.write_text(config.replace(public, zone + public + start + indent(routes, "    ") + end))
PY
if ! docker compose exec -T edge nginx -t </dev/null ||
   ! docker compose exec -T edge nginx -s reload </dev/null; then
  cat "$backup/nginx.conf" > nginx.conf
  docker compose exec -T edge nginx -s reload </dev/null
  exit 1
fi
echo "Public hub route backup: $backup/nginx.conf"
