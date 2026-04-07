#!/bin/bash
# Power off server1-3

MKUBE_API="http://192.168.200.2:8082/api/v1/namespaces/default/baremetalhosts"

for server in server1 server2 server3; do
  echo "Powering off $server..."
  curl -s -X PATCH "$MKUBE_API/$server" \
    -H "Content-Type: application/json" \
    -d '{"spec":{"online":false}}' \
    | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  online={d[\"spec\"][\"online\"]}')"
done

echo "All servers powered off."
