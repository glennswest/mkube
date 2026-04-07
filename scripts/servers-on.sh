#!/bin/bash
# Power on server1-3 with correct boot configs
# server1 = build job runner (CoreOS agent)
# server2, server3 = local boot

MKUBE_API="http://192.168.200.2:8082/api/v1/namespaces/default/baremetalhosts"

echo "Powering on server1 (build runner - fcos-cloudid / coreos-agent)..."
curl -s -X PATCH "$MKUBE_API/server1" \
  -H "Content-Type: application/json" \
  -d '{"spec":{"online":true,"image":"fcos-cloudid","bootConfigRef":"coreos-agent","template":"fcos/agent-runner"}}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  image={d[\"spec\"][\"image\"]} bootConfig={d[\"spec\"][\"bootConfigRef\"]} online={d[\"spec\"][\"online\"]}')"

echo "Powering on server2 (local boot)..."
curl -s -X PATCH "$MKUBE_API/server2" \
  -H "Content-Type: application/json" \
  -d '{"spec":{"online":true,"image":"localboot","bootConfigRef":"local"}}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  image={d[\"spec\"][\"image\"]} bootConfig={d[\"spec\"][\"bootConfigRef\"]} online={d[\"spec\"][\"online\"]}')"

echo "Powering on server3 (local boot)..."
curl -s -X PATCH "$MKUBE_API/server3" \
  -H "Content-Type: application/json" \
  -d '{"spec":{"online":true,"image":"localboot","bootConfigRef":"local"}}' \
  | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  image={d[\"spec\"][\"image\"]} bootConfig={d[\"spec\"][\"bootConfigRef\"]} online={d[\"spec\"][\"online\"]}')"

echo "All servers powered on."
