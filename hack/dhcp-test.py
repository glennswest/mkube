#!/usr/bin/env python3
"""Send a DHCP DISCOVER to a specific server and print the response."""

import socket
import struct
import random
import sys
import time

def build_discover(mac_bytes, xid):
    """Build a DHCP DISCOVER packet."""
    packet = bytearray(300)

    packet[0] = 1          # op: BOOTREQUEST
    packet[1] = 1          # htype: Ethernet
    packet[2] = 6          # hlen: MAC length
    packet[3] = 0          # hops
    struct.pack_into('!I', packet, 4, xid)
    struct.pack_into('!H', packet, 8, 0)      # secs
    struct.pack_into('!H', packet, 10, 0x8000) # flags: broadcast
    # ciaddr, yiaddr, siaddr, giaddr = 0
    packet[28:34] = mac_bytes  # chaddr

    # Magic cookie
    offset = 236
    packet[offset:offset+4] = b'\x63\x82\x53\x63'
    offset += 4

    # Option 53: DHCP Message Type = Discover (1)
    packet[offset] = 53; packet[offset+1] = 1; packet[offset+2] = 1
    offset += 3

    # Option 55: Parameter Request List
    params = [1, 3, 6, 15, 66, 67]  # subnet, router, dns, domain, tftp, bootfile
    packet[offset] = 55; packet[offset+1] = len(params)
    offset += 2
    for p in params:
        packet[offset] = p; offset += 1

    # End
    packet[offset] = 255

    return bytes(packet[:offset+1])


def parse_response(data):
    """Parse a DHCP response packet."""
    if len(data) < 240:
        return None

    op = data[0]
    if op != 2:
        return None

    xid = struct.unpack('!I', data[4:8])[0]
    yiaddr = socket.inet_ntoa(data[16:20])
    siaddr = socket.inet_ntoa(data[20:24])

    # Parse options after magic cookie
    options = {}
    i = 240
    while i < len(data):
        code = data[i]
        if code == 255:
            break
        if code == 0:
            i += 1
            continue
        length = data[i+1]
        value = data[i+2:i+2+length]
        options[code] = value
        i += 2 + length

    msg_type = options.get(53, b'\x00')[0]
    types = {1: 'DISCOVER', 2: 'OFFER', 3: 'REQUEST', 4: 'DECLINE',
             5: 'ACK', 6: 'NAK', 7: 'RELEASE'}

    result = {
        'xid': f'{xid:08x}',
        'type': types.get(msg_type, f'unknown({msg_type})'),
        'offered_ip': yiaddr,
        'server_ip': siaddr,
    }

    if 1 in options:
        result['subnet'] = socket.inet_ntoa(options[1])
    if 3 in options:
        result['router'] = socket.inet_ntoa(options[3][:4])
    if 6 in options:
        result['dns'] = socket.inet_ntoa(options[6][:4])
    if 15 in options:
        result['domain'] = options[15].decode('ascii', errors='replace')
    if 54 in options:
        result['dhcp_server'] = socket.inet_ntoa(options[54])
    if 66 in options:
        result['tftp_server'] = options[66].rstrip(b'\x00').decode('ascii', errors='replace')
    if 67 in options:
        result['bootfile'] = options[67].rstrip(b'\x00').decode('ascii', errors='replace')

    return result


def main():
    target = sys.argv[1] if len(sys.argv) > 1 else '192.168.10.252'
    mac_str = sys.argv[2] if len(sys.argv) > 2 else 'f4:52:14:84:b7:e0'

    mac_bytes = bytes(int(b, 16) for b in mac_str.split(':'))
    xid = random.randint(0, 0xFFFFFFFF)

    print(f"Sending DHCP DISCOVER to {target}:67")
    print(f"  MAC: {mac_str}")
    print(f"  XID: {xid:08x}")

    packet = build_discover(mac_bytes, xid)

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    sock.settimeout(5)

    try:
        sock.sendto(packet, (target, 67))
        print(f"  Sent {len(packet)} bytes")

        print("Waiting for response (5s timeout)...")
        data, addr = sock.recvfrom(1500)
        print(f"  Received {len(data)} bytes from {addr}")

        result = parse_response(data)
        if result:
            print("\nDHCP Response:")
            for k, v in result.items():
                print(f"  {k}: {v}")
        else:
            print("  Could not parse response")

    except socket.timeout:
        print("  TIMEOUT - no response received")
    finally:
        sock.close()


if __name__ == '__main__':
    main()
