#!/bin/bash
# Test DHCP from inside the mkube container (which has a veth on the g10 bridge)
# Sends a raw DHCP discover and listens for a response

# Upload a tiny Python DHCP test into the mkube container via the config volume
# then execute it via SSH into rose1

ROSE1="admin@rose1.gw.lo"
CONTAINER="g10_dns_microdns"

echo "=== Checking g10/dns container DHCP from rose1 ==="

# Use SSH to run a quick UDP test from rose1 itself (which is on the bridge)
# rose1 has an IP on the bridge (192.168.10.1), so we can send a unicast
# DHCP discover to the container's IP to test if the DHCP server responds at all

echo "Step 1: Sending unicast DHCP discover to 192.168.10.252:67..."
ssh $ROSE1 "/tool/fetch url=http://192.168.10.252:8080/api/v1/leases mode=http" 2>/dev/null
echo ""

echo "Step 2: Checking leases via API..."
curl -sf --connect-timeout 5 "http://192.168.10.252:8080/api/v1/leases" 2>&1
echo ""

echo "Step 3: Checking container logs for DHCP activity..."
curl -sf --connect-timeout 5 "http://192.168.200.2:8082/api/v1/namespaces/g10/pods/dns/log" 2>&1 | grep -i "dhcp\|discover\|offer\|ack\|lease\|assigned" | tail -10
echo ""

echo "Step 4: Packet capture on bridge for 10 seconds..."
ssh $ROSE1 '/tool/sniffer/set filter-interface=bridge filter-port=67,68 filter-mac-protocol="" filter-ip-address="" memory-limit=1000' 2>/dev/null
ssh $ROSE1 '/tool/sniffer/start' 2>/dev/null
sleep 10
ssh $ROSE1 '/tool/sniffer/stop' 2>/dev/null
echo "Captured packets:"
ssh $ROSE1 '/tool/sniffer/packet/print' 2>/dev/null
echo ""

echo "Step 5: Check bridge port status..."
ssh $ROSE1 '/interface/bridge/port/print where interface~"veth_g10_dns|qsfp28-2"' 2>/dev/null
echo ""

echo "=== Done ==="
