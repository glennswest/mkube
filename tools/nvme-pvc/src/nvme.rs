//! A minimal NVMe/TCP initiator (NVMe-oF TP 8000).
//!
//! This replaces the iSCSI initiator outright. It exposes the same small
//! block API the rest of the tool was already written against —
//! `connect / read_capacity / read_blocks / write_blocks / synchronize_cache`
//! — so formatting, re-identifying, flushing and pattern verification all
//! work over NVMe with no change at the call sites.
//!
//! Scope is deliberately narrow: one admin queue and one I/O queue, queue
//! depth one, no digests, namespace 1. Everything here is synchronous
//! request/response, which is all a format-and-verify tool needs and which
//! keeps the state machine small enough to reason about.
//!
//! Wire notes worth keeping, because they are the parts that bite:
//!
//!   * NVMe/TCP uses ONE TCP CONNECTION PER QUEUE. The admin queue cannot
//!     carry I/O commands, so a second connection is opened and joined to
//!     the controller returned by the admin Connect.
//!   * A fabrics controller must be explicitly enabled: set CC.EN and poll
//!     CSTS.RDY via Property Set/Get before any I/O queue will attach.
//!   * Writes go out as H2CData PDUs in response to an R2T from the target,
//!     and a read's final C2HData PDU may carry a SUCCESS flag that stands
//!     in for the completion, in which case no response capsule follows.

use anyhow::{bail, Context, Result};
use std::net::SocketAddr;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

/// Geometry of the attached namespace.
#[derive(Debug, Clone)]
pub struct DiskCapacity {
    /// Number of logical blocks.
    pub block_count: u64,
    /// Bytes per block. stormblockmk volumes are 4096.
    pub block_size: u32,
    /// Total size in bytes.
    pub total_bytes: u64,
}

impl std::fmt::Display for DiskCapacity {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{} blocks x {} bytes = {:.2} GiB",
            self.block_count,
            self.block_size,
            self.total_bytes as f64 / (1024.0 * 1024.0 * 1024.0)
        )
    }
}

// ─── PDU types ──────────────────────────────────────────────────────────────

const PDU_ICREQ: u8 = 0x00;
const PDU_ICRESP: u8 = 0x01;
const PDU_H2C_TERM: u8 = 0x02;
const PDU_C2H_TERM: u8 = 0x03;
const PDU_CAPSULE_CMD: u8 = 0x04;
const PDU_CAPSULE_RESP: u8 = 0x05;
const PDU_H2C_DATA: u8 = 0x06;
const PDU_C2H_DATA: u8 = 0x07;
const PDU_R2T: u8 = 0x09;

/// C2HData flags: bit2 LAST_PDU, bit3 SUCCESS (completion is implied).
const C2H_FLAG_SUCCESS: u8 = 0x08;
/// H2CData flags: bit2 marks the last PDU of the transfer.
const H2C_FLAG_LAST: u8 = 0x04;

// ─── NVMe opcodes ───────────────────────────────────────────────────────────

const NVME_OP_FLUSH: u8 = 0x00;
const NVME_OP_WRITE: u8 = 0x01;
const NVME_OP_READ: u8 = 0x02;
const NVME_ADMIN_IDENTIFY: u8 = 0x06;
const NVME_ADMIN_SET_FEATURES: u8 = 0x09;
const NVME_OP_FABRICS: u8 = 0x7F;

const FCTYPE_PROPERTY_SET: u8 = 0x00;
const FCTYPE_CONNECT: u8 = 0x01;
const FCTYPE_PROPERTY_GET: u8 = 0x04;

/// SGL descriptor: transport data block, transferred out of capsule.
const SGL_TRANSPORT_DATA: u8 = 0x55;
/// Command flags: SGL used for the data pointer.
const CMD_FLAG_SGL: u8 = 0x40;

// Controller properties.
const PROP_CAP: u32 = 0x00;
const PROP_CC: u32 = 0x14;
const PROP_CSTS: u32 = 0x1C;

const SQE_LEN: usize = 64;
const CONNECT_DATA_LEN: usize = 1024;
const NQN_FIELD_LEN: usize = 256;

/// One PDU as received: the common header, the rest of its header, and any
/// data that followed.
struct Pdu {
    pdu_type: u8,
    flags: u8,
    header: Vec<u8>,
    data: Vec<u8>,
}

/// A single NVMe/TCP queue — one TCP connection, one command in flight.
struct Queue {
    stream: TcpStream,
    /// Controller PDU data alignment, in 4-byte units, from ICResp.
    cpda: u8,
    /// Largest H2CData payload the controller will accept.
    maxh2cdata: u32,
    cmd_id: u16,
}

impl Queue {
    async fn connect_tcp(addr: SocketAddr) -> Result<Self> {
        let stream = TcpStream::connect(addr)
            .await
            .with_context(|| format!("connecting to {addr}"))?;
        stream.set_nodelay(true).ok();
        let mut q = Queue {
            stream,
            cpda: 0,
            maxh2cdata: 128 * 1024,
            cmd_id: 1,
        };
        q.initialize().await?;
        Ok(q)
    }

    /// ICReq/ICResp — the connection-level handshake that precedes any
    /// command. Digests are declined; they buy nothing here and every PDU
    /// would need a CRC32C.
    async fn initialize(&mut self) -> Result<()> {
        let mut pdu = vec![0u8; 128];
        pdu[0] = PDU_ICREQ;
        pdu[2] = 128; // hlen
        pdu[4..8].copy_from_slice(&128u32.to_le_bytes()); // plen
        // pfv = 0, hpda = 0, digest = 0 (none), maxr2t = 0 (one at a time)
        self.stream.write_all(&pdu).await?;
        self.stream.flush().await?;

        let resp = self.recv_pdu().await?;
        if resp.pdu_type != PDU_ICRESP {
            bail!(
                "expected ICResp from the controller, got PDU type 0x{:02x}",
                resp.pdu_type
            );
        }
        if resp.header.len() < 16 {
            bail!("ICResp header truncated ({} bytes)", resp.header.len());
        }
        let digest = resp.header[11];
        if digest != 0 {
            bail!("controller insists on digests (0x{digest:02x}); this initiator does not implement them");
        }
        self.cpda = resp.header[10];
        let maxh2cdata = u32::from_le_bytes(resp.header[12..16].try_into().unwrap());
        if maxh2cdata >= 4096 {
            self.maxh2cdata = maxh2cdata;
        }
        Ok(())
    }

    fn next_cmd_id(&mut self) -> u16 {
        let id = self.cmd_id;
        // 0xFFFF is reserved for AEN-style traffic; keep clear of it.
        self.cmd_id = if self.cmd_id >= 0xFFFE { 1 } else { self.cmd_id + 1 };
        id
    }

    /// Bytes of padding between a PDU header and its data, honoring the
    /// alignment the controller asked for in ICResp.
    fn data_pad(&self, hlen: usize) -> usize {
        let align = (self.cpda as usize + 1) * 4;
        if align <= 1 {
            return 0;
        }
        (align - (hlen % align)) % align
    }

    async fn recv_exact(&mut self, n: usize) -> Result<Vec<u8>> {
        let mut buf = vec![0u8; n];
        self.stream
            .read_exact(&mut buf)
            .await
            .with_context(|| format!("reading {n} bytes from the controller"))?;
        Ok(buf)
    }

    async fn recv_pdu(&mut self) -> Result<Pdu> {
        let ch = self.recv_exact(8).await?;
        let pdu_type = ch[0];
        let flags = ch[1];
        let hlen = ch[2] as usize;
        let pdo = ch[3] as usize;
        let plen = u32::from_le_bytes(ch[4..8].try_into().unwrap()) as usize;

        if hlen < 8 || plen < hlen {
            bail!("malformed PDU header (type 0x{pdu_type:02x} hlen={hlen} plen={plen})");
        }
        let mut header = ch;
        if hlen > 8 {
            header.extend_from_slice(&self.recv_exact(hlen - 8).await?);
        }

        // Data begins at pdo when the controller specifies one.
        let data_start = if pdo > 0 { pdo } else { hlen };
        if data_start > hlen {
            let _ = self.recv_exact(data_start - hlen).await?;
        }
        let data_len = plen.saturating_sub(data_start.max(hlen));
        let data = if data_len > 0 {
            self.recv_exact(data_len).await?
        } else {
            Vec::new()
        };

        if pdu_type == PDU_C2H_TERM || pdu_type == PDU_H2C_TERM {
            bail!("controller terminated the connection (PDU type 0x{pdu_type:02x})");
        }
        Ok(Pdu {
            pdu_type,
            flags,
            header,
            data,
        })
    }

    /// Send a command capsule, optionally with in-capsule data.
    async fn send_capsule(&mut self, sqe: &[u8; SQE_LEN], data: Option<&[u8]>) -> Result<()> {
        let hlen = 8 + SQE_LEN; // 72
        let pad = if data.is_some() { self.data_pad(hlen) } else { 0 };
        let dlen = data.map(|d| d.len()).unwrap_or(0);
        let plen = hlen + pad + dlen;

        let mut pdu = Vec::with_capacity(plen);
        pdu.push(PDU_CAPSULE_CMD);
        pdu.push(0); // no digests
        pdu.push(hlen as u8);
        pdu.push(if data.is_some() { (hlen + pad) as u8 } else { 0 });
        pdu.extend_from_slice(&(plen as u32).to_le_bytes());
        pdu.extend_from_slice(sqe);
        if let Some(d) = data {
            pdu.extend(std::iter::repeat(0u8).take(pad));
            pdu.extend_from_slice(d);
        }
        self.stream.write_all(&pdu).await?;
        self.stream.flush().await?;
        Ok(())
    }

    async fn send_h2c_data(&mut self, cmd_id: u16, ttag: u16, offset: u32, data: &[u8]) -> Result<()> {
        let hlen = 24usize;
        let pad = self.data_pad(hlen);
        let plen = hlen + pad + data.len();

        let mut pdu = Vec::with_capacity(plen);
        pdu.push(PDU_H2C_DATA);
        pdu.push(H2C_FLAG_LAST);
        pdu.push(hlen as u8);
        pdu.push((hlen + pad) as u8);
        pdu.extend_from_slice(&(plen as u32).to_le_bytes());
        pdu.extend_from_slice(&cmd_id.to_le_bytes());
        pdu.extend_from_slice(&ttag.to_le_bytes());
        pdu.extend_from_slice(&offset.to_le_bytes());
        pdu.extend_from_slice(&(data.len() as u32).to_le_bytes());
        pdu.extend_from_slice(&[0u8; 4]);
        pdu.extend(std::iter::repeat(0u8).take(pad));
        pdu.extend_from_slice(data);

        self.stream.write_all(&pdu).await?;
        self.stream.flush().await?;
        Ok(())
    }

    /// Run one command to completion, servicing R2T and C2HData along the
    /// way. Returns (completion result dword pair, any data read back).
    async fn execute(
        &mut self,
        sqe: &[u8; SQE_LEN],
        write_data: Option<&[u8]>,
        expect_read: usize,
    ) -> Result<(u64, Vec<u8>)> {
        let cmd_id = u16::from_le_bytes([sqe[2], sqe[3]]);
        self.send_capsule(sqe, None).await?;

        let mut read_buf = Vec::with_capacity(expect_read);
        loop {
            let pdu = self.recv_pdu().await?;
            match pdu.pdu_type {
                PDU_R2T => {
                    let data = write_data
                        .ok_or_else(|| anyhow::anyhow!("controller sent R2T for a command with no data"))?;
                    let ttag = u16::from_le_bytes(pdu.header[10..12].try_into().unwrap());
                    let offset = u32::from_le_bytes(pdu.header[12..16].try_into().unwrap());
                    let length = u32::from_le_bytes(pdu.header[16..20].try_into().unwrap());

                    let start = offset as usize;
                    let end = (start + length as usize).min(data.len());
                    if start >= data.len() {
                        bail!("controller asked for data past the end of the buffer (offset {start})");
                    }
                    // Respect the controller's per-PDU ceiling.
                    let mut sent = start;
                    while sent < end {
                        let chunk_end = (sent + self.maxh2cdata as usize).min(end);
                        self.send_h2c_data(cmd_id, ttag, sent as u32, &data[sent..chunk_end])
                            .await?;
                        sent = chunk_end;
                    }
                }
                PDU_C2H_DATA => {
                    read_buf.extend_from_slice(&pdu.data);
                    if pdu.flags & C2H_FLAG_SUCCESS != 0 {
                        // The controller folded the completion into this PDU.
                        return Ok((0, read_buf));
                    }
                }
                PDU_CAPSULE_RESP => {
                    if pdu.header.len() < 24 {
                        bail!("response capsule truncated ({} bytes)", pdu.header.len());
                    }
                    let cqe = &pdu.header[8..24];
                    let result = u64::from_le_bytes(cqe[0..8].try_into().unwrap());
                    let status = u16::from_le_bytes(cqe[14..16].try_into().unwrap()) >> 1;
                    if status != 0 {
                        let sct = (status >> 8) & 0x7;
                        let sc = status & 0xFF;
                        bail!("NVMe command failed: status type {sct} code 0x{sc:02x} (raw 0x{status:04x})");
                    }
                    return Ok((result, read_buf));
                }
                other => bail!("unexpected PDU type 0x{other:02x} while awaiting completion"),
            }
        }
    }
}

/// Build a bare SQE with the common fields filled in.
fn sqe(opcode: u8, cmd_id: u16, nsid: u32) -> [u8; SQE_LEN] {
    let mut s = [0u8; SQE_LEN];
    s[0] = opcode;
    s[1] = CMD_FLAG_SGL;
    s[2..4].copy_from_slice(&cmd_id.to_le_bytes());
    s[4..8].copy_from_slice(&nsid.to_le_bytes());
    s
}

/// Point a command's data pointer at a host transfer of `len` bytes.
fn set_sgl(s: &mut [u8; SQE_LEN], len: u32) {
    s[24..32].copy_from_slice(&0u64.to_le_bytes()); // address
    s[32..36].copy_from_slice(&len.to_le_bytes());
    s[36..39].copy_from_slice(&[0, 0, 0]);
    s[39] = SGL_TRANSPORT_DATA;
}

fn set_cdw(s: &mut [u8; SQE_LEN], index: usize, value: u32) {
    let off = 40 + (index - 10) * 4;
    s[off..off + 4].copy_from_slice(&value.to_le_bytes());
}

/// An NVMe/TCP session against one namespace.
pub struct NvmeInitiator {
    admin: Queue,
    io: Queue,
    nsid: u32,
    block_size: u32,
    block_count: u64,
}

impl NvmeInitiator {
    /// Connect to a subsystem and bring up an I/O queue against namespace 1.
    ///
    /// `addr` must carry the export's OWN port. stormblockmk serves each
    /// volume on a dedicated port and the shared 4420 is discovery only —
    /// connecting there yields a controller that answers the handshake and
    /// then errors every I/O.
    pub async fn connect(addr: SocketAddr, subnqn: &str, hostnqn: &str) -> Result<Self> {
        tracing::info!("connecting to NVMe/TCP subsystem {subnqn} at {addr}");

        // --- admin queue -------------------------------------------------
        let mut admin = Queue::connect_tcp(addr).await?;
        let hostid = host_id(hostnqn);
        let cntlid = fabrics_connect(&mut admin, 0, 31, 0xFFFF, &hostid, subnqn, hostnqn).await?;
        tracing::debug!("admin queue joined, controller id {cntlid}");

        // A fabrics controller starts disabled; enable it and wait for ready.
        let cap = property_get(&mut admin, PROP_CAP, true).await?;
        let mqes = (cap & 0xFFFF) as u16; // zero-based max queue entries
        let to_ms = ((cap >> 24) & 0xFF) as u64 * 500; // CAP.TO, 500ms units

        // CC: EN=1, IOSQES=6 (64B), IOCQES=4 (16B).
        let cc: u32 = 1 | (6 << 16) | (4 << 20);
        property_set(&mut admin, PROP_CC, cc as u64).await?;

        let deadline = std::time::Instant::now()
            + std::time::Duration::from_millis(if to_ms == 0 { 5_000 } else { to_ms.max(1_000) });
        loop {
            let csts = property_get(&mut admin, PROP_CSTS, false).await?;
            if csts & 0x1 != 0 {
                break;
            }
            if csts & 0x2 != 0 {
                bail!("controller reported a fatal status while enabling (CSTS=0x{csts:x})");
            }
            if std::time::Instant::now() > deadline {
                bail!("controller never became ready (CSTS=0x{csts:x})");
            }
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        }
        tracing::debug!("controller enabled and ready");

        // Ask for one I/O queue pair. Values are zero-based.
        let mut s = sqe(NVME_ADMIN_SET_FEATURES, admin.next_cmd_id(), 0);
        set_cdw(&mut s, 10, 0x07); // Number of Queues
        set_cdw(&mut s, 11, 0x0000_0000); // 1 submission, 1 completion
        admin.execute(&s, None, 0).await?;

        // --- namespace geometry ------------------------------------------
        let nsid = 1u32;
        let mut s = sqe(NVME_ADMIN_IDENTIFY, admin.next_cmd_id(), nsid);
        set_sgl(&mut s, 4096);
        set_cdw(&mut s, 10, 0x00); // CNS 0 = Identify Namespace
        let (_, ident) = admin.execute(&s, None, 4096).await?;
        if ident.len() < 132 {
            bail!("Identify Namespace returned {} bytes", ident.len());
        }
        let block_count = u64::from_le_bytes(ident[0..8].try_into().unwrap());
        let flbas = (ident[26] & 0x0F) as usize;
        let lbaf_off = 128 + flbas * 4;
        if ident.len() < lbaf_off + 4 {
            bail!("Identify Namespace has no LBA format {flbas}");
        }
        let lbads = ident[lbaf_off + 2];
        if !(9..=22).contains(&lbads) {
            bail!("namespace reports an implausible block size exponent {lbads}");
        }
        let block_size = 1u32 << lbads;
        tracing::info!("namespace {nsid}: {block_count} blocks of {block_size} bytes");

        // --- I/O queue (its own TCP connection) ---------------------------
        let sqsize = if mqes == 0 { 31 } else { (mqes as u32).min(31) as u16 };
        let mut io = Queue::connect_tcp(addr).await?;
        fabrics_connect(&mut io, 1, sqsize, cntlid, &hostid, subnqn, hostnqn).await?;
        tracing::info!("NVMe/TCP session established to {subnqn}");

        Ok(Self {
            admin,
            io,
            nsid,
            block_size,
            block_count,
        })
    }

    /// Present for parity with the old initiator; an established fabrics
    /// session has already proven the controller is answering.
    pub async fn test_unit_ready(&mut self) -> Result<()> {
        Ok(())
    }

    /// Geometry, from Identify Namespace rather than READ CAPACITY.
    pub async fn read_capacity(&mut self) -> Result<DiskCapacity> {
        Ok(DiskCapacity {
            block_count: self.block_count,
            block_size: self.block_size,
            total_bytes: self.block_count * self.block_size as u64,
        })
    }

    pub async fn read_blocks(&mut self, lba: u32, count: u16) -> Result<Vec<u8>> {
        if count == 0 {
            return Ok(Vec::new());
        }
        let len = count as u32 * self.block_size;
        let cmd_id = self.io.next_cmd_id();
        let mut s = sqe(NVME_OP_READ, cmd_id, self.nsid);
        set_sgl(&mut s, len);
        set_cdw(&mut s, 10, lba);
        set_cdw(&mut s, 11, 0);
        set_cdw(&mut s, 12, (count - 1) as u32); // zero-based block count
        let (_, data) = self.io.execute(&s, None, len as usize).await?;
        if data.len() < len as usize {
            bail!(
                "short read at lba {lba}: wanted {len} bytes, got {}",
                data.len()
            );
        }
        data.get(..len as usize)
            .map(|d| d.to_vec())
            .ok_or_else(|| anyhow::anyhow!("short read at lba {lba}"))
    }

    pub async fn write_blocks(&mut self, lba: u32, data: &[u8]) -> Result<()> {
        let bs = self.block_size as usize;
        if data.is_empty() {
            return Ok(());
        }
        if data.len() % bs != 0 {
            bail!(
                "write of {} bytes is not a multiple of the namespace block size {}",
                data.len(),
                bs
            );
        }
        let count = (data.len() / bs) as u16;
        let cmd_id = self.io.next_cmd_id();
        let mut s = sqe(NVME_OP_WRITE, cmd_id, self.nsid);
        set_sgl(&mut s, data.len() as u32);
        set_cdw(&mut s, 10, lba);
        set_cdw(&mut s, 11, 0);
        set_cdw(&mut s, 12, (count - 1) as u32);
        self.io.execute(&s, Some(data), 0).await?;
        Ok(())
    }

    /// NVMe FLUSH — the counterpart of SCSI SYNCHRONIZE CACHE. stormblock
    /// implements it as device.flush(), which is what makes a golden durable
    /// before it is sealed.
    pub async fn synchronize_cache(&mut self) -> Result<()> {
        let cmd_id = self.io.next_cmd_id();
        let s = sqe(NVME_OP_FLUSH, cmd_id, self.nsid);
        self.io.execute(&s, None, 0).await?;
        Ok(())
    }

    pub async fn logout(mut self) -> Result<()> {
        // No graceful teardown is required for a fabrics session; closing
        // both queues releases the controller's resources.
        let _ = self.io.stream.shutdown().await;
        let _ = self.admin.stream.shutdown().await;
        Ok(())
    }
}

/// Fabrics Connect. Returns the controller id the target assigned.
async fn fabrics_connect(
    q: &mut Queue,
    qid: u16,
    sqsize: u16,
    cntlid: u16,
    hostid: &[u8; 16],
    subnqn: &str,
    hostnqn: &str,
) -> Result<u16> {
    let mut data = vec![0u8; CONNECT_DATA_LEN];
    data[0..16].copy_from_slice(hostid);
    data[16..18].copy_from_slice(&cntlid.to_le_bytes());
    write_nqn(&mut data[256..256 + NQN_FIELD_LEN], subnqn)?;
    write_nqn(&mut data[512..512 + NQN_FIELD_LEN], hostnqn)?;

    let cmd_id = q.next_cmd_id();
    let mut s = sqe(NVME_OP_FABRICS, cmd_id, 0);
    s[4] = FCTYPE_CONNECT; // fabrics reuses the nsid field for fctype
    s[5..8].copy_from_slice(&[0, 0, 0]);
    set_sgl(&mut s, CONNECT_DATA_LEN as u32);
    s[40..42].copy_from_slice(&0u16.to_le_bytes()); // recfmt
    s[42..44].copy_from_slice(&qid.to_le_bytes());
    s[44..46].copy_from_slice(&sqsize.to_le_bytes());
    s[46] = 0; // cattr
    s[48..52].copy_from_slice(&0u32.to_le_bytes()); // kato

    let (result, _) = q.execute(&s, Some(&data), 0).await.with_context(|| {
        format!("fabrics Connect for queue {qid} to {subnqn}")
    })?;
    Ok((result & 0xFFFF) as u16)
}

async fn property_get(q: &mut Queue, offset: u32, wide: bool) -> Result<u64> {
    let cmd_id = q.next_cmd_id();
    let mut s = sqe(NVME_OP_FABRICS, cmd_id, 0);
    s[4] = FCTYPE_PROPERTY_GET;
    s[40] = if wide { 1 } else { 0 }; // attrib: 1 = 64-bit property
    s[44..48].copy_from_slice(&offset.to_le_bytes());
    let (result, _) = q.execute(&s, None, 0).await?;
    Ok(if wide { result } else { result & 0xFFFF_FFFF })
}

async fn property_set(q: &mut Queue, offset: u32, value: u64) -> Result<()> {
    let cmd_id = q.next_cmd_id();
    let mut s = sqe(NVME_OP_FABRICS, cmd_id, 0);
    s[4] = FCTYPE_PROPERTY_SET;
    s[40] = 0;
    s[44..48].copy_from_slice(&offset.to_le_bytes());
    s[48..56].copy_from_slice(&value.to_le_bytes());
    q.execute(&s, None, 0).await?;
    Ok(())
}

fn write_nqn(field: &mut [u8], nqn: &str) -> Result<()> {
    let bytes = nqn.as_bytes();
    if bytes.len() >= field.len() {
        bail!("NQN is too long ({} bytes, max {})", bytes.len(), field.len() - 1);
    }
    field[..bytes.len()].copy_from_slice(bytes);
    Ok(())
}

/// A stable host id derived from the host NQN, so reconnects from the same
/// tool present the same identity rather than a fresh one each run.
fn host_id(hostnqn: &str) -> [u8; 16] {
    let mut id = [0u8; 16];
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in hostnqn.as_bytes() {
        h ^= *b as u64;
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    id[0..8].copy_from_slice(&h.to_le_bytes());
    id[8..16].copy_from_slice(&h.rotate_left(32).to_le_bytes());
    // Stamp RFC 4122 version 4 / variant bits so the value is a well-formed UUID.
    id[6] = (id[6] & 0x0F) | 0x40;
    id[8] = (id[8] & 0x3F) | 0x80;
    id
}
