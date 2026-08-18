use anyhow::{bail, Result};
use clap::{Parser, Subcommand};
use nvme_pvc::nvme::NvmeInitiator;
use nvme_pvc::routeros::RouterOsClient;
use std::net::SocketAddr;

/// NQN this tool presents as. Stable across runs so a controller sees one
/// host reconnecting rather than a new host each time.
const HOST_NQN: &str = "nqn.2024-01.io.vkube:mkube-tool";

#[derive(Parser)]
#[command(
    name = "nvme-pvc",
    about = "NVMe/TCP volume tool: format, re-identify, flush and verify stormblock volumes"
)]
struct Cli {
    /// RouterOS REST API base URL (e.g. http://192.168.200.1)
    #[arg(long, env = "ROUTEROS_URL", default_value = "http://192.168.200.1")]
    url: String,

    /// RouterOS REST API username
    #[arg(long, env = "ROUTEROS_USER", default_value = "driveman")]
    user: String,

    /// RouterOS REST API password
    #[arg(long, env = "ROUTEROS_PASSWORD")]
    password: String,

    /// NVMe/TCP target as IP or IP:port. stormblockmk gives every export its
    /// OWN port; 4420 is the shared discovery endpoint and does NOT serve
    /// per-volume subsystems, so pass the export's port explicitly.
    #[arg(long, env = "NVME_TARGET", default_value = "192.168.200.21")]
    target: String,

    #[command(subcommand)]
    command: Commands,
}

/// Read the ext4 superblock area (file bytes 1024..1024+len) regardless of
/// the namespace block size, returning the buffer and the in-buffer offset of
/// byte 1024. Hardcoding "LBA 2" assumes 512-byte sectors and reads the wrong
/// place on 4096-byte devices (stormblockmk volumes).
async fn read_superblock(
    session: &mut NvmeInitiator,
    block_size: u32,
) -> anyhow::Result<(Vec<u8>, usize)> {
    let bs = block_size.max(1) as u64;
    let lba = 1024 / bs;
    let skip = (1024 % bs) as usize;
    let blocks = ((skip as u64 + 4096).div_ceil(bs)) as u16;
    let data = session.read_blocks(lba as u32, blocks).await?;
    Ok((data, skip))
}

fn target_addr(target: &str) -> Result<SocketAddr, std::net::AddrParseError> {
    if target.contains(':') {
        target.parse()
    } else {
        format!("{target}:4420").parse()
    }
}

async fn open(target: &str, nqn: &str) -> Result<NvmeInitiator> {
    let addr = target_addr(target)?;
    Ok(NvmeInitiator::connect(addr, nqn, HOST_NQN).await?)
}

#[derive(Subcommand)]
enum Commands {
    /// Test connectivity to RouterOS
    Ping,

    /// Write a known pattern at a block offset (each block stamped with its
    /// own LBA), then optionally verify it. The point is to answer "are we
    /// actually storing bytes?" without trusting allocation counters.
    Pattern {
        /// Subsystem NQN
        nqn: String,
        /// write | check
        #[arg(long, default_value = "check")]
        mode: String,
        /// First LBA to touch
        #[arg(long, default_value_t = 4096)]
        lba: u32,
        /// How many blocks
        #[arg(long, default_value_t = 64)]
        count: u32,
    },

    /// Tell the controller to commit its cache (NVMe FLUSH)
    Flush {
        /// Subsystem NQN
        nqn: String,
    },

    /// Give a filesystem a fresh UUID (and optionally label) — a CoW clone
    /// inherits its golden's identity byte for byte, and duplicate ext4
    /// UUIDs on one host confuse mount-by-UUID and blkid
    Reid {
        /// Subsystem NQN
        nqn: String,
        /// New filesystem label
        #[arg(long)]
        label: Option<String>,
        /// Also restore the "cleanly unmounted" flag (state=1). Only valid
        /// once writes have quiesced.
        #[arg(long)]
        clean: bool,
    },

    /// Dump ext4 superblock fields (for comparing our format against one
    /// RouterOS wrote itself — signatures and layouts must match)
    Sb {
        /// Subsystem NQN
        nqn: String,
    },

    /// Connect to the subsystem and report namespace geometry
    Probe {
        /// Subsystem NQN
        nqn: String,
    },

    /// Format a namespace with ext4
    Format {
        /// Subsystem NQN
        nqn: String,
        /// Volume label
        #[arg(long, default_value = "pvc-data")]
        label: String,
    },
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let cli = Cli::parse();

    match cli.command {
        Commands::Ping => {
            let ros = RouterOsClient::new(&cli.url, &cli.user, &cli.password)?;
            let info = ros.system_resource().await?;
            println!("Connected to RouterOS:");
            println!("  Board:   {}", info["board-name"].as_str().unwrap_or("?"));
            println!("  Version: {}", info["version"].as_str().unwrap_or("?"));
            println!("  CPU:     {} x {}", info["cpu-count"].as_str().unwrap_or("?"), info["cpu"].as_str().unwrap_or("?"));
            println!("  Memory:  {} free", info["free-memory"].as_str().unwrap_or("?"));
            println!("  Uptime:  {}", info["uptime"].as_str().unwrap_or("?"));
        }

        Commands::Pattern { nqn, mode, lba, count } => {
            let mut session = open(&cli.target, &nqn).await?;
            let cap = session.read_capacity().await?;
            let bs = cap.block_size as usize;
            let stamp = |i: u32| -> Vec<u8> {
                let mut b = vec![0u8; bs];
                let magic: u64 = 0xDA7A_0000_0000_0000 | (i as u64);
                b[..8].copy_from_slice(&magic.to_le_bytes());
                for (j, x) in b[8..].iter_mut().enumerate() {
                    *x = ((i as usize + j) % 251) as u8;
                }
                b
            };
            match mode.as_str() {
                "write" => {
                    for i in 0..count {
                        session.write_blocks(lba + i, &stamp(i)).await?;
                    }
                    session.synchronize_cache().await?;
                    println!("wrote {count} blocks of {bs} bytes at lba {lba} (+flush)");
                }
                _ => {
                    let mut bad = 0u32;
                    let mut zero = 0u32;
                    for i in 0..count {
                        let got = session.read_blocks(lba + i, 1).await?;
                        let want = stamp(i);
                        if got.len() < bs { bail!("short read at lba {}", lba + i); }
                        if got[..bs] == want[..] { continue; }
                        if got[..bs].iter().all(|b| *b == 0) { zero += 1; } else { bad += 1; }
                    }
                    println!("checked {count} blocks at lba {lba}: {} good, {zero} all-zero, {bad} corrupt",
                             count - zero - bad);
                    if zero + bad > 0 { bail!("pattern verification FAILED"); }
                }
            }
            session.logout().await?;
        }

        Commands::Flush { nqn } => {
            let mut session = open(&cli.target, &nqn).await?;
            session.synchronize_cache().await?;
            println!("flush complete");
            session.logout().await?;
        }

        Commands::Reid { nqn, label, clean } => {
            let mut session = open(&cli.target, &nqn).await?;
            let cap = session.read_capacity().await?;
            let (mut sb, off) = read_superblock(&mut session, cap.block_size).await?;
            if sb.len() < off + 264 {
                bail!("short superblock read: {} bytes", sb.len());
            }
            let magic = u16::from_le_bytes([sb[off + 56], sb[off + 57]]);
            if magic != 0xEF53 {
                bail!("no ext4 superblock (magic 0x{magic:04x})");
            }
            let uuid = generate_uuid();
            sb[off + 104..off + 120].copy_from_slice(&uuid);
            if clean {
                sb[off + 58] = 1;
                sb[off + 59] = 0;
            }
            if let Some(l) = &label {
                let mut buf = [0u8; 16];
                let b = l.as_bytes();
                let n = b.len().min(16);
                buf[..n].copy_from_slice(&b[..n]);
                sb[off + 120..off + 136].copy_from_slice(&buf);
            }
            // Write the block that carries file byte 1024 back verbatim.
            let bs = cap.block_size.max(1) as u64;
            let lba = (1024 / bs) as u32;
            session.write_blocks(lba, &sb[..bs as usize]).await?;
            session.synchronize_cache().await?;
            print!("new uuid ");
            for b in uuid { print!("{b:02x}"); }
            println!();
            if let Some(l) = label { println!("new label {l}"); }
            if clean { println!("state set to 1 (cleanly unmounted)"); }
            session.logout().await?;
        }

        Commands::Sb { nqn } => {
            let mut session = open(&cli.target, &nqn).await?;
            let cap = session.read_capacity().await?;
            let (sb, off) = read_superblock(&mut session, cap.block_size).await?;
            if sb.len() < off + 264 {
                bail!("short superblock read: {} bytes", sb.len());
            }
            let s = &sb[off..];
            let le16 = |o: usize| u16::from_le_bytes([s[o], s[o + 1]]);
            let le32 = |o: usize| u32::from_le_bytes([s[o], s[o + 1], s[o + 2], s[o + 3]]);
            let label: String = s[120..136].iter().take_while(|c| **c != 0)
                .map(|c| *c as char).collect();
            let uuid = &s[104..120];
            println!("device_block_size {}", cap.block_size);
            println!("magic             0x{:04x}", le16(56));
            println!("state             {}   (1 = cleanly unmounted)", le16(58));
            println!("log_block_size    {}   (block size {})", le32(24), 1024u32 << le32(24));
            println!("inodes_count      {}", le32(0));
            println!("blocks_count      {}", le32(4));
            println!("first_data_block  {}", le32(20));
            println!("blocks_per_group  {}", le32(32));
            println!("inode_size        {}", le16(88));
            println!("feature_compat    0x{:08x}", le32(92));
            println!("feature_incompat  0x{:08x}", le32(96));
            println!("feature_ro_compat 0x{:08x}", le32(100));
            println!("label             {label:?}");
            print!("uuid              ");
            for b in uuid { print!("{b:02x}"); }
            println!();
            session.logout().await?;
        }

        Commands::Probe { nqn } => {
            println!("Connecting to NVMe/TCP subsystem...");
            println!("  Target: {}", cli.target);
            println!("  NQN:    {nqn}");

            let mut session = open(&cli.target, &nqn).await?;
            session.test_unit_ready().await?;
            println!("  Controller ready: OK");

            let cap = session.read_capacity().await?;
            println!("  Capacity: {cap}");

            let (sb_data, off) = read_superblock(&mut session, cap.block_size).await?;
            if sb_data.len() >= off + 58 {
                let has_ext4 = sb_data[off + 56] == 0x53 && sb_data[off + 57] == 0xEF;
                println!("  Filesystem: {}", if has_ext4 { "ext4" } else { "none (unformatted)" });
            }

            session.logout().await?;
        }

        Commands::Format { nqn, label } => {
            println!("Formatting namespace as ext4...");
            println!("  Target: {}", cli.target);
            println!("  NQN:    {nqn}");
            println!("  Label:  {label}");

            let mut session = open(&cli.target, &nqn).await?;
            let cap = session.read_capacity().await?;
            println!("  Capacity: {cap}");

            let uuid = generate_uuid();
            {
                let mut writer = NvmeBlockWriter {
                    session: &mut session,
                    block_size: 4096,
                    sector_size: cap.block_size,
                };
                nvme_pvc::ext4::format_ext4(&mut writer, cap.total_bytes, uuid, &label).await?;
            }
            // Commit before anyone attaches: an unflushed format is exactly
            // how a volume ends up mounting clean and empty.
            session.synchronize_cache().await?;
            println!("  Format complete!");

            // Verify: superblock is at file byte 1024, magic 56 bytes in.
            let (sb_data, off) = read_superblock(&mut session, cap.block_size).await?;
            if sb_data.len() >= off + 58 {
                let magic = u16::from_le_bytes([sb_data[off + 56], sb_data[off + 57]]);
                if magic == 0xEF53 {
                    println!("  Verification: ext4 superblock OK (magic=0xEF53)");
                } else {
                    bail!("verification failed: unexpected magic 0x{magic:04x}");
                }
            }

            session.logout().await?;
            println!("\nDone. Namespace is now ext4-formatted.");
        }
    }

    Ok(())
}

/// Adapter to write 4K ext4 blocks via NVMe WRITE.
struct NvmeBlockWriter<'a> {
    session: &'a mut NvmeInitiator,
    block_size: u32,
    sector_size: u32,
}

#[async_trait::async_trait]
impl nvme_pvc::ext4::BlockWriter for NvmeBlockWriter<'_> {
    async fn write_block(&mut self, block_num: u64, data: &[u8]) -> Result<()> {
        // Convert an ext4 block number to a namespace LBA.
        let sectors_per_block = (self.block_size / self.sector_size).max(1);
        let lba = (block_num * sectors_per_block as u64) as u32;

        // Pad to exactly one ext4 block if the caller passed less.
        let mut buf = vec![0u8; self.block_size as usize];
        let copy_len = data.len().min(buf.len());
        buf[..copy_len].copy_from_slice(&data[..copy_len]);

        self.session.write_blocks(lba, &buf).await
    }

    fn block_size(&self) -> u32 {
        self.block_size
    }
}

fn generate_uuid() -> [u8; 16] {
    // Random, not time+pid. Every CLONE of a golden inherits its
    // filesystem UUID byte for byte, so weak or partially-zero UUIDs make
    // collisions between simultaneously-mounted clones far more likely —
    // and a duplicate fs UUID is exactly what confuses mount-by-UUID and
    // blkid caching.
    let mut uuid = [0u8; 16];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        use std::io::Read;
        let _ = f.read_exact(&mut uuid);
    }
    if uuid.iter().all(|b| *b == 0) {
        use std::time::{SystemTime, UNIX_EPOCH};
        let nanos = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default().as_nanos();
        uuid[0..16].copy_from_slice(&nanos.to_le_bytes());
    }
    uuid[6] = (uuid[6] & 0x0F) | 0x40; // version 4
    uuid[8] = (uuid[8] & 0x3F) | 0x80; // RFC 4122 variant
    uuid
}
