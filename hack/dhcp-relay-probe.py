#!/usr/bin/env python3
"""
DHCP relay probe: send a DISCOVER with giaddr set, simulating what a relay
would send. This tests the full DHCP processing path (bypassing the giaddr=0
filter).

Usage: python3 dhcp-relay-probe.py [server_ip] [relay_ip] [spoof_mac]

The response goes back to the relay IP on port 67, so this script binds to
a high port and won't see the response directly. Check the server logs
for "DHCP Discover" and "offering" messages.
"""

import socket
import struct
import random
import sys
import time


def build_relayed_discover(mac_bytes, xid, giaddr_bytes):
    """Build a DHCP DISCOVER with giaddr set (as a relay would send)."""
    packet = bytearray(300)
    packet[0] = 1           # BOOTREQUEST
    packet[1] = 1           # Ethernet
    packet[2] = 6           # MAC len
    packet[3] = 1           # hops = 1 (relayed)
    struct.pack_into('!I', packet, 4, xid)
    struct.pack_into('!H', packet, 8, 0)       # secs
    struct.pack_into('!H', packet, 10, 0)      # flags (no broadcast - relay handles it)
    # ciaddr = 0 (offset 12-15)
    # yiaddr = 0 (offset 16-19)
    # siaddr = 0 (offset 20-23)
    packet[24:28] = giaddr_bytes               # giaddr = relay IP
    packet[28:34] = mac_bytes                  # chaddr

    # Magic cookie
    off = 236
    packet[off:off+4] = b'\x63\x82\x53\x63'
    off += 4

    # Option 53: DHCP Discover
    packet[off] = 53; packet[off+1] = 1; packet[off+2] = 1; off += 3

    # Option 55: Parameter request
    params = [1, 3, 6, 15, 66, 67]
    packet[off] = 55; packet[off+1] = len(params); off += 2
    for p in params:
        packet[off] = p; off += 1

    # End
    packet[off] = 255
    return bytes(packet[:off+1])


def main():
    server = sys.argv[1] if len(sys.argv) > 1 else '192.168.10.252'
    relay = sys.argv[2] if len(sys.argv) > 2 else '192.168.10.1'
    mac_str = sys.argv[3] if len(sys.argv) > 3 else 'ac:1f:6b:8a:a7:9c'  # server1

    mac_bytes = bytes(int(b, 16) for b in mac_str.split(':'))
    giaddr_bytes = socket.inet_aton(relay)
    xid = random.randint(0, 0xFFFFFFFF)

    print(f"DHCP DISCOVER (relay-simulated) -> {server}:67")
    print(f"  MAC: {mac_str}  XID: {xid:08x}")
    print(f"  giaddr: {relay}  hops: 1")

    pkt = build_relayed_discover(mac_bytes, xid, giaddr_bytes)

    # Send from source port 67 to look like a real relay
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(5)
    try:
        sock.bind(('', 67))
        print(f"  Bound to port 67 (like a real relay)")
    except OSError:
        sock.bind(('', 0))
        port = sock.getsockname()[1]
        print(f"  Could not bind port 67, using port {port}")

    try:
        sock.sendto(pkt, (server, 67))
        print(f"  Sent {len(pkt)} bytes")
        print(f"  Response goes to {relay}:67 (relay), not to us")
        print("  Waiting 3s for any stray response...")
        try:
            data, addr = sock.recvfrom(1500)
            print(f"  Got {len(data)} bytes from {addr}")
        except socket.timeout:
            print("  No direct response (expected — response goes to relay)")
    finally:
        sock.close()

    print(f"\nCheck server logs for 'DHCP Discover from {mac_str}'")


if __name__ == '__main__':
    main()
