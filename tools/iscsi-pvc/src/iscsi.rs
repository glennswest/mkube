//! Pure-Rust iSCSI initiator — enough to login, read capacity, read/write blocks.
//!
//! Implements the iSCSI protocol (RFC 7143) at the minimum level needed to:
//! 1. Connect and login to a target
//! 2. Read capacity (SCSI READ CAPACITY)
//! 3. Read/write blocks (SCSI READ(10)/WRITE(10))
//! 4. Logout and disconnect

use anyhow::{bail, Context, Result};
use byteorder::{BigEndian, ByteOrder};
use std::net::SocketAddr;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

// iSCSI opcodes (initiator → target)
const ISCSI_OP_LOGIN_REQ: u8 = 0x03;
const ISCSI_OP_LOGOUT_REQ: u8 = 0x06;
const ISCSI_OP_SCSI_CMD: u8 = 0x01;
const ISCSI_OP_NOP_OUT: u8 = 0x00;

// iSCSI opcodes (target → initiator)
const ISCSI_OP_LOGIN_RESP: u8 = 0x23;
const ISCSI_OP_LOGOUT_RESP: u8 = 0x26;
const ISCSI_OP_SCSI_RESP: u8 = 0x21;
const ISCSI_OP_DATA_IN: u8 = 0x25;
const ISCSI_OP_NOP_IN: u8 = 0x20;
const ISCSI_OP_R2T: u8 = 0x31;
const ISCSI_OP_REJECT: u8 = 0x3F;

// SCSI opcodes
const SCSI_TEST_UNIT_READY: u8 = 0x00;
const SCSI_READ_CAPACITY_10: u8 = 0x25;
const SCSI_READ_10: u8 = 0x28;
const SCSI_WRITE_10: u8 = 0x2A;

// iSCSI BHS (Basic Header Segment) is always 48 bytes
const BHS_SIZE: usize = 48;

/// iSCSI session state.
pub struct IscsiInitiator {
    stream: TcpStream,
    target_iqn: String,
    initiator_iqn: String,
    cmd_sn: u32,
    exp_stat_sn: u32,
    itt_counter: u32,
    isid: [u8; 6],
    /// Target-assigned session tag
    tsih: u16,
    /// Negotiated max recv data segment length
    max_recv_data_segment: u32,
    /// Negotiated first burst length
    first_burst_length: u32,
}

/// Result of READ CAPACITY(10).
#[derive(Debug, Clone)]
pub struct DiskCapacity {
    /// Number of logical blocks.
    pub block_count: u64,
    /// Bytes per block (typically 512).
    pub block_size: u32,
    /// Total size in bytes.
    pub total_bytes: u64,
}

impl IscsiInitiator {
    /// Connect to an iSCSI target portal and log in.
    pub async fn connect(
        portal: SocketAddr,
        target_iqn: &str,
        initiator_iqn: &str,
    ) -> Result<Self> {
        tracing::info!("connecting to iSCSI portal {portal}");
        let stream = TcpStream::connect(portal)
            .await
            .with_context(|| format!("connecting to {portal}"))?;

        // Disable Nagle for better latency
        stream.set_nodelay(true).ok();

        let mut session = Self {
            stream,
            target_iqn: target_iqn.to_string(),
            initiator_iqn: initiator_iqn.to_string(),
            cmd_sn: 1,
            exp_stat_sn: 0,
            itt_counter: 1,
            isid: [0x00, 0x02, 0x3D, 0x00, 0x00, 0x01],
            tsih: 0,
            max_recv_data_segment: 65536,
            first_burst_length: 65536,
        };

        session.login().await?;
        tracing::info!("iSCSI login successful to {target_iqn}");
        Ok(session)
    }

    fn next_itt(&mut self) -> u32 {
        let itt = self.itt_counter;
        self.itt_counter += 1;
        itt
    }

    /// Build and send a login PDU, return response BHS + data.
    async fn send_login_pdu(
        &mut self,
        csg: u8,
        nsg: u8,
        transit: bool,
        kvs: &str,
        itt: u32,
    ) -> Result<([u8; BHS_SIZE], Vec<u8>)> {
        let data_segment = kvs.as_bytes();

        let mut bhs = [0u8; BHS_SIZE];
        bhs[0] = ISCSI_OP_LOGIN_REQ | 0x40; // immediate

        // Byte 1: T | C | reserved | CSG(2) | NSG(2)
        let mut flags: u8 = 0;
        if transit {
            flags |= 0x80; // T bit
        }
        flags |= (csg & 0x03) << 2;
        flags |= nsg & 0x03;
        bhs[1] = flags;

        // Version
        bhs[2] = 0x00; // version-max
        bhs[3] = 0x00; // version-min

        // Data segment length
        let ds_len = data_segment.len() as u32;
        bhs[5] = ((ds_len >> 16) & 0xFF) as u8;
        bhs[6] = ((ds_len >> 8) & 0xFF) as u8;
        bhs[7] = (ds_len & 0xFF) as u8;

        // ISID + TSIH
        bhs[8..14].copy_from_slice(&self.isid);
        BigEndian::write_u16(&mut bhs[14..16], self.tsih);

        // ITT
        BigEndian::write_u32(&mut bhs[16..20], itt);
        // CID (connection ID) at bytes 20-21
        BigEndian::write_u16(&mut bhs[20..22], 0);
        // CmdSN
        BigEndian::write_u32(&mut bhs[24..28], self.cmd_sn);
        // ExpStatSN
        BigEndian::write_u32(&mut bhs[28..32], self.exp_stat_sn);

        // Send
        self.stream.write_all(&bhs).await?;
        self.stream.write_all(data_segment).await?;
        let pad = (4 - (data_segment.len() % 4)) % 4;
        if pad > 0 {
            self.stream.write_all(&vec![0u8; pad]).await?;
        }
        self.stream.flush().await?;

        // Read response
        let mut resp_bhs = [0u8; BHS_SIZE];
        self.stream.read_exact(&mut resp_bhs).await?;

        let opcode = resp_bhs[0] & 0x3F;
        if opcode != ISCSI_OP_LOGIN_RESP {
            bail!("expected Login Response (0x23), got 0x{opcode:02x}");
        }

        // Read data segment
        let resp_ds_len = ((resp_bhs[5] as usize) << 16)
            | ((resp_bhs[6] as usize) << 8)
            | (resp_bhs[7] as usize);
        let mut resp_data = Vec::new();
        if resp_ds_len > 0 {
            let padded = resp_ds_len + ((4 - (resp_ds_len % 4)) % 4);
            resp_data.resize(padded, 0);
            self.stream.read_exact(&mut resp_data).await?;
            resp_data.truncate(resp_ds_len);
        }

        Ok((resp_bhs, resp_data))
    }

    /// Perform iSCSI login with proper multi-phase negotiation.
    async fn login(&mut self) -> Result<()> {
        let itt = self.next_itt();

        // Phase 1: Security Negotiation (CSG=0) → Login Operational (NSG=1)
        // Send initiator/target names and request AuthMethod=None
        let phase1_kvs = format!(
            "InitiatorName={}\0\
             TargetName={}\0\
             SessionType=Normal\0\
             AuthMethod=None\0",
            self.initiator_iqn, self.target_iqn,
        );

        let (resp_bhs, resp_data) = self
            .send_login_pdu(0, 1, true, &phase1_kvs, itt)
            .await?;

        // Check status
        let status_class = resp_bhs[36];
        let status_detail = resp_bhs[37];
        if status_class != 0 {
            let msg = String::from_utf8_lossy(&resp_data);
            bail!("login phase 1 failed: class={status_class} detail={status_detail} data={msg}");
        }

        // Extract TSIH (may be 0 until final response)
        self.tsih = BigEndian::read_u16(&resp_bhs[14..16]);
        // Track StatSN from phase 1 (will be overwritten by phase 2)
        let p1_stat_sn = BigEndian::read_u32(&resp_bhs[24..28]);
        self.exp_stat_sn = p1_stat_sn + 1;

        // Check if target already transited to full feature phase
        let resp_flags = resp_bhs[1];
        let resp_transit = (resp_flags & 0x80) != 0;
        let resp_csg = (resp_flags >> 2) & 0x03;
        let resp_nsg = resp_flags & 0x03;

        tracing::debug!(
            "login phase 1 response: T={resp_transit} CSG={resp_csg} NSG={resp_nsg} TSIH={}",
            self.tsih
        );

        // Parse target's key-value responses
        if !resp_data.is_empty() {
            let text = String::from_utf8_lossy(&resp_data);
            tracing::debug!("login phase 1 target params: {text}");
        }

        // If target transited directly to FFP (NSG=3, T=1), we're done
        if resp_transit && resp_nsg == 3 {
            self.cmd_sn += 1;
            return Ok(());
        }

        // Phase 2: Login Operational Negotiation (CSG=1) → Full Feature Phase (NSG=3)
        let phase2_kvs = format!(
            "HeaderDigest=None\0\
             DataDigest=None\0\
             MaxRecvDataSegmentLength=262144\0\
             MaxBurstLength=262144\0\
             FirstBurstLength=262144\0\
             DefaultTime2Wait=2\0\
             DefaultTime2Retain=0\0\
             MaxOutstandingR2T=1\0\
             ImmediateData=Yes\0\
             InitialR2T=Yes\0\
             MaxConnections=1\0\
             DataPDUInOrder=Yes\0\
             DataSequenceInOrder=Yes\0\
             ErrorRecoveryLevel=0\0"
        );

        let (resp_bhs, resp_data) = self
            .send_login_pdu(1, 3, true, &phase2_kvs, itt)
            .await?;

        let status_class = resp_bhs[36];
        let status_detail = resp_bhs[37];

        let p2_flags = resp_bhs[1];
        let p2_transit = (p2_flags & 0x80) != 0;
        let p2_csg = (p2_flags >> 2) & 0x03;
        let p2_nsg = p2_flags & 0x03;
        tracing::debug!(
            "login phase 2 response: T={p2_transit} CSG={p2_csg} NSG={p2_nsg} status_class={status_class} TSIH={}",
            BigEndian::read_u16(&resp_bhs[14..16])
        );

        if status_class != 0 {
            let msg = String::from_utf8_lossy(&resp_data);
            bail!("login phase 2 failed: class={status_class} detail={status_detail} data={msg}");
        }

        // Extract TSIH from final login response
        self.tsih = BigEndian::read_u16(&resp_bhs[14..16]);

        // Update StatSN from phase 2 response
        let stat_sn = BigEndian::read_u32(&resp_bhs[24..28]);
        let exp_cmd_sn = BigEndian::read_u32(&resp_bhs[28..32]);
        tracing::debug!(
            "login phase 2: StatSN={stat_sn} ExpCmdSN={exp_cmd_sn}"
        );
        self.exp_stat_sn = stat_sn + 1;
        // RFC 7143: CmdSN is NOT advanced after login. First SCSI command
        // uses the CmdSN that the target's ExpCmdSN points to.
        self.cmd_sn = exp_cmd_sn;

        // Parse negotiated parameters
        if !resp_data.is_empty() {
            let text = String::from_utf8_lossy(&resp_data);
            tracing::debug!("login phase 2 target params: {text}");
            for kv in text.split('\0') {
                if let Some((key, val)) = kv.split_once('=') {
                    match key {
                        "MaxRecvDataSegmentLength" => {
                            if let Ok(v) = val.parse::<u32>() {
                                self.max_recv_data_segment = v;
                            }
                        }
                        "FirstBurstLength" => {
                            if let Ok(v) = val.parse::<u32>() {
                                self.first_burst_length = v;
                            }
                        }
                        _ => {}
                    }
                }
            }
        }

        tracing::debug!(
            "negotiated: MaxRecvDataSegment={} FirstBurst={}",
            self.max_recv_data_segment,
            self.first_burst_length,
        );

        Ok(())
    }

    /// Read a response PDU (BHS + data segment).
    async fn read_pdu(&mut self) -> Result<(u8, [u8; BHS_SIZE], Vec<u8>)> {
        let mut resp_bhs = [0u8; BHS_SIZE];
        self.stream.read_exact(&mut resp_bhs).await?;

        let opcode = resp_bhs[0] & 0x3F;
        tracing::debug!(
            "PDU: opcode=0x{:02x} raw[0..8]={:02x?}",
            opcode,
            &resp_bhs[..8]
        );
        let resp_ds_len = ((resp_bhs[5] as usize) << 16)
            | ((resp_bhs[6] as usize) << 8)
            | (resp_bhs[7] as usize);

        let mut data_buf = Vec::new();
        if resp_ds_len > 0 {
            let padded = resp_ds_len + ((4 - (resp_ds_len % 4)) % 4);
            data_buf.resize(padded, 0);
            self.stream.read_exact(&mut data_buf).await?;
            data_buf.truncate(resp_ds_len);
        }

        Ok((opcode, resp_bhs, data_buf))
    }

    /// Handle a NOP-In by sending NOP-Out response if needed.
    async fn handle_nop_in(&mut self, bhs: &[u8; BHS_SIZE]) -> Result<()> {
        let ttt = BigEndian::read_u32(&bhs[20..24]);
        if ttt != 0xFFFFFFFF {
            // Target expects a NOP-Out response
            let mut nop_out = [0u8; BHS_SIZE];
            nop_out[0] = ISCSI_OP_NOP_OUT | 0x40; // immediate
            nop_out[1] = 0x80; // F=1
            BigEndian::write_u32(&mut nop_out[16..20], 0xFFFFFFFF); // ITT=reserved
            BigEndian::write_u32(&mut nop_out[20..24], ttt); // TTT
            BigEndian::write_u32(&mut nop_out[24..28], self.cmd_sn);
            BigEndian::write_u32(&mut nop_out[28..32], self.exp_stat_sn);
            self.stream.write_all(&nop_out).await?;
            self.stream.flush().await?;
        }
        Ok(())
    }

    /// Send a SCSI command and receive data-in response.
    async fn scsi_command(
        &mut self,
        cdb: &[u8],
        expected_data_len: u32,
        write_data: Option<&[u8]>,
    ) -> Result<Vec<u8>> {
        let itt = self.next_itt();
        let is_read = write_data.is_none() && expected_data_len > 0;
        let is_write = write_data.is_some();

        // Build SCSI Command BHS
        let mut bhs = [0u8; BHS_SIZE];
        bhs[0] = ISCSI_OP_SCSI_CMD; // NOT immediate — let target sequence normally

        // Byte 1: F=1 (final), R/W flags, ATTR=0 (untagged)
        let mut flags: u8 = 0x80; // F=1 (final PDU)
        if is_read {
            flags |= 0x40; // R=1
        }
        if is_write {
            flags |= 0x20; // W=1
        }
        bhs[1] = flags;

        // Data segment length (immediate data for writes)
        if let Some(data) = write_data {
            let imm_len = data.len().min(self.first_burst_length as usize);
            let len = imm_len as u32;
            bhs[5] = ((len >> 16) & 0xFF) as u8;
            bhs[6] = ((len >> 8) & 0xFF) as u8;
            bhs[7] = (len & 0xFF) as u8;
        }

        // ITT
        BigEndian::write_u32(&mut bhs[16..20], itt);
        // Expected Data Transfer Length
        if is_write {
            BigEndian::write_u32(&mut bhs[20..24], write_data.unwrap().len() as u32);
        } else {
            BigEndian::write_u32(&mut bhs[20..24], expected_data_len);
        }
        // CmdSN
        BigEndian::write_u32(&mut bhs[24..28], self.cmd_sn);
        // ExpStatSN
        BigEndian::write_u32(&mut bhs[28..32], self.exp_stat_sn);

        // CDB (up to 16 bytes)
        let cdb_len = cdb.len().min(16);
        bhs[32..32 + cdb_len].copy_from_slice(&cdb[..cdb_len]);

        // Send BHS
        self.stream.write_all(&bhs).await?;

        // Send immediate data for writes
        if let Some(data) = write_data {
            let imm_len = data.len().min(self.first_burst_length as usize);
            self.stream.write_all(&data[..imm_len]).await?;
            let pad = (4 - (imm_len % 4)) % 4;
            if pad > 0 {
                self.stream.write_all(&vec![0u8; pad]).await?;
            }
        }
        self.stream.flush().await?;

        self.cmd_sn += 1;

        // Receive response(s)
        let mut result_data = Vec::new();

        loop {
            let (opcode, resp_bhs, data_buf) = self.read_pdu().await?;

            match opcode {
                ISCSI_OP_DATA_IN => {
                    let s_bit = resp_bhs[1] & 0x01;
                    let f_bit = resp_bhs[1] & 0x80;
                    result_data.extend_from_slice(&data_buf);

                    if s_bit != 0 {
                        self.exp_stat_sn = BigEndian::read_u32(&resp_bhs[24..28]) + 1;
                        let status = resp_bhs[3];
                        if status != 0 {
                            bail!("SCSI command failed: status=0x{status:02x}");
                        }
                    }

                    if f_bit != 0 && s_bit != 0 {
                        break;
                    }
                    if f_bit != 0 && s_bit == 0 {
                        // Final data-in without status; SCSI Response follows
                        continue;
                    }
                    // More data-in PDUs coming
                }
                ISCSI_OP_SCSI_RESP => {
                    self.exp_stat_sn = BigEndian::read_u32(&resp_bhs[24..28]) + 1;
                    let status = resp_bhs[3];
                    if status != 0 {
                        // Check for sense data
                        if !data_buf.is_empty() {
                            tracing::debug!("SCSI sense data: {:02x?}", &data_buf);
                        }
                        bail!("SCSI response status=0x{status:02x}");
                    }
                    break;
                }
                ISCSI_OP_R2T => {
                    // Ready to Transfer — target wants more data
                    if let Some(data) = write_data {
                        let r2t_offset = BigEndian::read_u32(&resp_bhs[40..44]) as usize;
                        let r2t_length = BigEndian::read_u32(&resp_bhs[44..48]) as usize;
                        let ttt = BigEndian::read_u32(&resp_bhs[20..24]);

                        tracing::debug!("R2T: offset={r2t_offset} length={r2t_length} ttt=0x{ttt:08x}");

                        // Send Data-Out PDU
                        let end = (r2t_offset + r2t_length).min(data.len());
                        let chunk = &data[r2t_offset..end];

                        let mut data_out = [0u8; BHS_SIZE];
                        data_out[0] = 0x05; // SCSI Data-Out
                        data_out[1] = 0x80; // F=1 (final)

                        let len = chunk.len() as u32;
                        data_out[5] = ((len >> 16) & 0xFF) as u8;
                        data_out[6] = ((len >> 8) & 0xFF) as u8;
                        data_out[7] = (len & 0xFF) as u8;

                        BigEndian::write_u32(&mut data_out[16..20], itt); // ITT
                        BigEndian::write_u32(&mut data_out[20..24], ttt); // TTT
                        BigEndian::write_u32(&mut data_out[28..32], self.exp_stat_sn);
                        BigEndian::write_u32(&mut data_out[36..40], 0); // DataSN
                        BigEndian::write_u32(&mut data_out[40..44], r2t_offset as u32); // Buffer Offset

                        self.stream.write_all(&data_out).await?;
                        self.stream.write_all(chunk).await?;
                        let pad = (4 - (chunk.len() % 4)) % 4;
                        if pad > 0 {
                            self.stream.write_all(&vec![0u8; pad]).await?;
                        }
                        self.stream.flush().await?;
                    }
                }
                ISCSI_OP_NOP_IN => {
                    self.handle_nop_in(&resp_bhs).await?;
                }
                ISCSI_OP_REJECT => {
                    let reason = resp_bhs[2];
                    let reason_str = match reason {
                        0x04 => "Protocol Error",
                        0x05 => "Command Not Supported",
                        0x09 => "Invalid PDU Field",
                        0x0c => "Waiting for Logout",
                        _ => "Unknown",
                    };
                    tracing::error!(
                        "iSCSI REJECT: reason=0x{:02x} ({}) StatSN={} ExpCmdSN={}",
                        reason,
                        reason_str,
                        BigEndian::read_u32(&resp_bhs[24..28]),
                        BigEndian::read_u32(&resp_bhs[28..32]),
                    );
                    if data_buf.len() >= BHS_SIZE {
                        tracing::error!(
                            "rejected BHS: {:02x?}",
                            &data_buf[..BHS_SIZE]
                        );
                    }
                    bail!("iSCSI target rejected PDU: {reason_str} (0x{reason:02x})");
                }
                _ => {
                    tracing::warn!("unexpected opcode 0x{opcode:02x} during SCSI cmd, skipping");
                }
            }
        }

        Ok(result_data)
    }

    /// SCSI TEST UNIT READY — verify the target is accessible.
    pub async fn test_unit_ready(&mut self) -> Result<()> {
        let cdb = [SCSI_TEST_UNIT_READY, 0, 0, 0, 0, 0];
        self.scsi_command(&cdb, 0, None).await?;
        Ok(())
    }

    /// SCSI READ CAPACITY(10) — get disk size and block size.
    pub async fn read_capacity(&mut self) -> Result<DiskCapacity> {
        let cdb = [SCSI_READ_CAPACITY_10, 0, 0, 0, 0, 0, 0, 0, 0, 0];
        let data = self.scsi_command(&cdb, 8, None).await?;

        if data.len() < 8 {
            bail!("READ CAPACITY returned {} bytes, expected 8", data.len());
        }

        let last_lba = BigEndian::read_u32(&data[0..4]);
        let block_size = BigEndian::read_u32(&data[4..8]);
        let block_count = last_lba as u64 + 1;
        let total_bytes = block_count * block_size as u64;

        Ok(DiskCapacity {
            block_count,
            block_size,
            total_bytes,
        })
    }

    /// SCSI READ(10) — read blocks from the disk.
    pub async fn read_blocks(&mut self, lba: u32, count: u16) -> Result<Vec<u8>> {
        let mut cdb = [0u8; 10];
        cdb[0] = SCSI_READ_10;
        BigEndian::write_u32(&mut cdb[2..6], lba);
        BigEndian::write_u16(&mut cdb[7..9], count);

        // Assume 512-byte sectors
        let expected = count as u32 * 512;
        self.scsi_command(&cdb, expected, None).await
    }

    /// SCSI WRITE(10) — write blocks to the disk.
    pub async fn write_blocks(&mut self, lba: u32, data: &[u8]) -> Result<()> {
        let block_count = (data.len() / 512) as u16;
        let mut cdb = [0u8; 10];
        cdb[0] = SCSI_WRITE_10;
        BigEndian::write_u32(&mut cdb[2..6], lba);
        BigEndian::write_u16(&mut cdb[7..9], block_count);

        self.scsi_command(&cdb, 0, Some(data)).await?;
        Ok(())
    }

    /// Logout and disconnect.
    pub async fn logout(mut self) -> Result<()> {
        let itt = self.itt_counter;
        let mut bhs = [0u8; BHS_SIZE];
        bhs[0] = ISCSI_OP_LOGOUT_REQ | 0x40; // immediate
        bhs[1] = 0x80; // F=1, reason=close session (0)
        BigEndian::write_u32(&mut bhs[16..20], itt); // ITT
        BigEndian::write_u32(&mut bhs[24..28], self.cmd_sn); // CmdSN
        BigEndian::write_u32(&mut bhs[28..32], self.exp_stat_sn); // ExpStatSN

        self.stream.write_all(&bhs).await?;
        self.stream.flush().await?;

        // Read logout response
        let mut resp = [0u8; BHS_SIZE];
        match tokio::time::timeout(
            std::time::Duration::from_secs(5),
            self.stream.read_exact(&mut resp),
        )
        .await
        {
            Ok(Ok(_)) => {
                let opcode = resp[0] & 0x3F;
                if opcode != ISCSI_OP_LOGOUT_RESP {
                    tracing::warn!("expected Logout Response, got 0x{opcode:02x}");
                }
            }
            _ => {
                tracing::debug!("logout response timeout, closing connection");
            }
        }

        tracing::info!("iSCSI logout complete");
        Ok(())
    }
}

impl std::fmt::Display for DiskCapacity {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let gb = self.total_bytes as f64 / (1024.0 * 1024.0 * 1024.0);
        write!(
            f,
            "{} blocks x {} bytes = {:.2} GB ({} bytes)",
            self.block_count, self.block_size, gb, self.total_bytes
        )
    }
}
