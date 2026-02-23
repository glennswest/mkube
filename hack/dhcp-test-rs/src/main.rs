use socket2::{Domain, Protocol, Socket, Type};
use std::net::{Ipv4Addr, SocketAddr, SocketAddrV4, UdpSocket};
use std::time::Duration;

fn build_discover(mac: &[u8; 6], xid: u32) -> Vec<u8> {
    let mut pkt = vec![0u8; 300];
    pkt[0] = 1; // BOOTREQUEST
    pkt[1] = 1; // Ethernet
    pkt[2] = 6; // MAC len
    pkt[3] = 0; // hops
    pkt[4..8].copy_from_slice(&xid.to_be_bytes());
    // secs = 0
    pkt[10] = 0x80; pkt[11] = 0x00; // broadcast flag
    // ciaddr, yiaddr, siaddr, giaddr = 0
    pkt[28..34].copy_from_slice(mac);

    // Magic cookie at 236
    pkt[236] = 99; pkt[237] = 130; pkt[238] = 83; pkt[239] = 99;

    let mut off = 240;
    // Option 53: DHCP Discover
    pkt[off] = 53; pkt[off+1] = 1; pkt[off+2] = 1; off += 3;
    // Option 55: Parameter request
    let params = [1u8, 3, 6, 15, 66, 67];
    pkt[off] = 55; pkt[off+1] = params.len() as u8; off += 2;
    for p in &params { pkt[off] = *p; off += 1; }
    // End
    pkt[off] = 255;
    pkt.truncate(off + 1);
    // Pad to 300
    while pkt.len() < 300 { pkt.push(0); }
    pkt
}

fn parse_response(data: &[u8]) -> Option<String> {
    if data.len() < 240 || data[0] != 2 { return None; }
    let xid = u32::from_be_bytes([data[4], data[5], data[6], data[7]]);
    let yiaddr = Ipv4Addr::new(data[16], data[17], data[18], data[19]);
    let siaddr = Ipv4Addr::new(data[20], data[21], data[22], data[23]);
    let giaddr = Ipv4Addr::new(data[24], data[25], data[26], data[27]);

    let mut info = format!(
        "xid={xid:08x} yiaddr={yiaddr} siaddr={siaddr} giaddr={giaddr}"
    );

    // Parse options
    if data.len() > 240 && data[236..240] == [99, 130, 83, 99] {
        let mut i = 240;
        while i < data.len() {
            let code = data[i];
            if code == 255 { break; }
            if code == 0 { i += 1; continue; }
            if i + 1 >= data.len() { break; }
            let len = data[i+1] as usize;
            i += 2;
            if i + len > data.len() { break; }
            match code {
                53 => {
                    let t = match data[i] {
                        2 => "OFFER", 5 => "ACK", 6 => "NAK", _ => "?"
                    };
                    info = format!("type={t} {info}");
                }
                1 if len == 4 => info += &format!(" subnet={}.{}.{}.{}", data[i], data[i+1], data[i+2], data[i+3]),
                3 if len >= 4 => info += &format!(" router={}.{}.{}.{}", data[i], data[i+1], data[i+2], data[i+3]),
                6 if len >= 4 => info += &format!(" dns={}.{}.{}.{}", data[i], data[i+1], data[i+2], data[i+3]),
                54 if len == 4 => info += &format!(" server_id={}.{}.{}.{}", data[i], data[i+1], data[i+2], data[i+3]),
                _ => {}
            }
            i += len;
        }
    }
    Some(info)
}

fn main() {
    let server: Ipv4Addr = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "192.168.10.252".into())
        .parse()
        .expect("invalid IP");

    let mac = [0xde, 0xad, 0xbe, 0xef, 0x00, 0x01u8];
    let xid = 0x44484350;

    println!("DHCP DISCOVER -> {server}:67 (unicast)");
    println!("  MAC: de:ad:be:ef:00:01  XID: {xid:08x}");

    // Create socket with SO_REUSEADDR so we can bind to 68
    let sock = Socket::new(Domain::IPV4, Type::DGRAM, Some(Protocol::UDP))
        .expect("socket");
    sock.set_reuse_address(true).ok();
    sock.set_broadcast(true).ok();
    sock.set_read_timeout(Some(Duration::from_secs(5))).ok();

    // Bind to port 68 (DHCP client port) - need to receive responses
    let bind_addr = SocketAddrV4::new(Ipv4Addr::UNSPECIFIED, 68);
    if let Err(e) = sock.bind(&bind_addr.into()) {
        eprintln!("  Cannot bind to port 68 ({e}), trying random port...");
        let bind_any = SocketAddrV4::new(Ipv4Addr::UNSPECIFIED, 0);
        sock.bind(&bind_any.into()).expect("bind");
        eprintln!("  NOTE: Response will go to port 68 broadcast, not here.");
        eprintln!("  Run with: sudo cargo run  to bind to port 68");
    }

    let udp: UdpSocket = sock.into();
    let pkt = build_discover(&mac, xid);
    let dest = SocketAddr::new(server.into(), 67);

    udp.send_to(&pkt, dest).expect("send");
    println!("  Sent {} bytes", pkt.len());
    println!("  Waiting for response (5s)...");

    let mut buf = [0u8; 1500];
    match udp.recv_from(&mut buf) {
        Ok((len, from)) => {
            println!("  Got {len} bytes from {from}");
            match parse_response(&buf[..len]) {
                Some(info) => println!("  RESPONSE: {info}"),
                None => println!("  Could not parse response"),
            }
        }
        Err(e) => println!("  TIMEOUT/ERROR: {e}"),
    }
}
