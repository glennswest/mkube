#!/usr/bin/env python3
"""
DHCP probe: send a DISCOVER as unicast to a specific DHCP server IP.
This bypasses broadcast — if the server responds, the DHCP code works
and the problem is broadcast delivery on the bridge.

Usage: python3 dhcp-probe.py <server_ip> [<spoof_mac>]

Must be run from a host that can reach the server IP (same subnet).
"""

import socket
import struct
import random
import sys
import time


def build_discover(mac_bytes, xid):
    packet = bytearray(300)
    packet[0] = 1           # BOOTREQUEST
    packet[1] = 1           # Ethernet
    packet[2] = 6           # MAC len
    packet[3] = 0           # hops
    struct.pack_into('!I', packet, 4, xid)
    struct.pack_into('!H', packet, 8, 0)       # secs
    struct.pack_into('!H', packet, 10, 0x8000) # broadcast flag
    packet[28:34] = mac_bytes

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


def parse_response(data):
    if len(data) < 240:
        return None
    if data[0] != 2:
        return None

    xid = struct.unpack('!I', data[4:8])[0]
    yiaddr = socket.inet_ntoa(data[16:20])
    siaddr = socket.inet_ntoa(data[20:24])

    options = {}
    i = 240
    while i < len(data):
        code = data[i]
        if code == 255: break
        if code == 0: i += 1; continue
        length = data[i+1]
        options[code] = data[i+2:i+2+length]
        i += 2 + length

    msg_type = options.get(53, b'\x00')[0]
    types = {2: 'OFFER', 5: 'ACK', 6: 'NAK'}

    result = {'xid': f'{xid:08x}', 'type': types.get(msg_type, f'?({msg_type})'),
              'offered_ip': yiaddr, 'next_server': siaddr}
    if 1 in options: result['subnet'] = socket.inet_ntoa(options[1])
    if 3 in options: result['router'] = socket.inet_ntoa(options[3][:4])
    if 6 in options: result['dns'] = socket.inet_ntoa(options[6][:4])
    if 15 in options: result['domain'] = options[15].rstrip(b'\x00').decode()
    if 66 in options: result['tftp'] = options[66].rstrip(b'\x00').decode()
    if 67 in options: result['bootfile'] = options[67].rstrip(b'\x00').decode()
    return result


def main():
    server = sys.argv[1] if len(sys.argv) > 1 else '192.168.10.252'
    mac_str = sys.argv[2] if len(sys.argv) > 2 else 'de:ad:be:ef:00:01'
    mac_bytes = bytes(int(b, 16) for b in mac_str.split(':'))
    xid = random.randint(0, 0xFFFFFFFF)

    print(f"DHCP DISCOVER (unicast) -> {server}:67")
    print(f"  MAC: {mac_str}  XID: {xid:08x}")

    pkt = build_discover(mac_bytes, xid)

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    sock.settimeout(5)
    # Bind to port 68 to receive the response
    try:
        sock.bind(('', 68))
    except OSError:
        # port 68 may be in use, bind to any
        sock.bind(('', 0))

    try:
        sock.sendto(pkt, (server, 67))
        print(f"  Sent {len(pkt)} bytes")
        print("  Waiting for response (5s)...")
        data, addr = sock.recvfrom(1500)
        print(f"  Got {len(data)} bytes from {addr}")
        r = parse_response(data)
        if r:
            print("\n  DHCP RESPONSE:")
            for k, v in r.items():
                print(f"    {k}: {v}")
        else:
            print("  Could not parse response")
    except socket.timeout:
        print("  TIMEOUT — no response from DHCP server")
    finally:
        sock.close()


if __name__ == '__main__':
    main()
