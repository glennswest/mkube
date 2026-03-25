use anyhow::{bail, Result};
use clap::{Parser, Subcommand};
use iscsi_pvc::iscsi::IscsiInitiator;
use iscsi_pvc::routeros::RouterOsClient;
use std::net::SocketAddr;

#[derive(Parser)]
#[command(name = "iscsi-pvc", about = "iSCSI PVC management tool for RouterOS")]
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

    /// iSCSI portal IP (for initiator connections)
    #[arg(long, env = "ISCSI_PORTAL", default_value = "192.168.200.1")]
    portal: String,

    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Test connectivity to RouterOS
    Ping,

    /// List all file-backed disks
    ListDisks,

    /// Create an iSCSI-backed PVC (file + disk + iSCSI export)
    Create {
        /// PVC name
        name: String,
        /// Size (e.g. "1G", "500M")
        #[arg(long, default_value = "1G")]
        size: String,
        /// Storage pool mount point
        #[arg(long, default_value = "raid1")]
        pool: String,
    },

    /// Delete an iSCSI-backed PVC
    Delete {
        /// PVC name
        name: String,
        /// Storage pool mount point
        #[arg(long, default_value = "raid1")]
        pool: String,
    },

    /// Connect to iSCSI target and read capacity
    Probe {
        /// Target IQN
        iqn: String,
    },

    /// Format an iSCSI target with ext4
    Format {
        /// Target IQN
        iqn: String,
        /// Volume label
        #[arg(long, default_value = "pvc-data")]
        label: String,
    },

    /// Full lifecycle test: create → export → connect → format → verify → cleanup
    Test {
        /// Test PVC name
        #[arg(default_value = "iscsi-pvc-test")]
        name: String,
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
    let ros = RouterOsClient::new(&cli.url, &cli.user, &cli.password)?;

    match cli.command {
        Commands::Ping => {
            let info = ros.system_resource().await?;
            println!("Connected to RouterOS:");
            println!("  Board:   {}", info["board-name"].as_str().unwrap_or("?"));
            println!("  Version: {}", info["version"].as_str().unwrap_or("?"));
            println!("  CPU:     {} x {}", info["cpu-count"].as_str().unwrap_or("?"), info["cpu"].as_str().unwrap_or("?"));
            println!("  Memory:  {} free", info["free-memory"].as_str().unwrap_or("?"));
            println!("  Uptime:  {}", info["uptime"].as_str().unwrap_or("?"));
        }

        Commands::ListDisks => {
            let disks = ros.list_file_disks().await?;
            println!("{:<6} {:<60} {:<12} {:<8} {}", "ID", "FILE PATH", "SIZE", "iSCSI", "IQN");
            println!("{}", "-".repeat(120));
            for d in &disks {
                println!(
                    "{:<6} {:<60} {:<12} {:<8} {}",
                    d.id,
                    d.file_path,
                    d.file_size,
                    if d.is_iscsi_exported() { "yes" } else { "no" },
                    d.iscsi_server_iqn,
                );
            }
            println!("\n{} file-backed disks", disks.len());
        }

        Commands::Create { name, size, pool } => {
            let file_path = format!("{pool}/volumes/pvc/{name}.raw");
            println!("Creating PVC: {name}");
            println!("  File: /{file_path}");
            println!("  Size: {size}");

            let (disk_id, iqn, port) = ros.create_iscsi_target(&file_path, Some(&size)).await?;
            println!("  Disk ID: {disk_id}");
            println!("  IQN: {iqn}");
            println!("  Portal: {}:{port}", cli.portal);
            println!("\nPVC created and exported via iSCSI.");
        }

        Commands::Delete { name, pool } => {
            let file_path = format!("{pool}/volumes/pvc/{name}.raw");
            println!("Deleting PVC: {name}");

            let disk = ros
                .find_disk_by_path(&file_path)
                .await?;

            match disk {
                Some(d) => {
                    ros.delete_iscsi_target(&d.id).await?;
                    println!("  Removed disk {} ({})", d.id, d.slot);
                    println!("PVC deleted.");
                }
                None => {
                    println!("No disk found for path: /{file_path}");
                }
            }
        }

        Commands::Probe { iqn } => {
            let portal: SocketAddr = format!("{}:3260", cli.portal).parse()?;
            println!("Connecting to iSCSI target...");
            println!("  Portal: {portal}");
            println!("  IQN: {iqn}");

            let mut session = IscsiInitiator::connect(
                portal,
                &iqn,
                "iqn.2024-01.io.vkube:iscsi-pvc-tool",
            )
            .await?;

            session.test_unit_ready().await?;
            println!("  Unit ready: OK");

            let cap = session.read_capacity().await?;
            println!("  Capacity: {cap}");

            // Read superblock area: starts at file byte 1024 = LBA 2
            let sb_data = session.read_blocks(2, 2).await?;
            if sb_data.len() >= 58 {
                let has_ext4 = sb_data[56] == 0x53 && sb_data[57] == 0xEF;
                println!("  Filesystem: {}", if has_ext4 { "ext4" } else { "none (unformatted)" });
            }

            session.logout().await?;
        }

        Commands::Format { iqn, label } => {
            let portal: SocketAddr = format!("{}:3260", cli.portal).parse()?;
            println!("Formatting iSCSI target as ext4...");
            println!("  Portal: {portal}");
            println!("  IQN: {iqn}");
            println!("  Label: {label}");

            let mut session = IscsiInitiator::connect(
                portal,
                &iqn,
                "iqn.2024-01.io.vkube:iscsi-pvc-tool",
            )
            .await?;

            let cap = session.read_capacity().await?;
            println!("  Capacity: {cap}");

            // Create an iSCSI block writer adapter
            let uuid = generate_uuid();
            {
                let mut writer = IscsiBlockWriter {
                    session: &mut session,
                    block_size: 4096,
                    sector_size: cap.block_size,
                };
                iscsi_pvc::ext4::format_ext4(&mut writer, cap.total_bytes, uuid, &label).await?;
            }
            println!("  Format complete!");

            // Verify: superblock is at file byte 1024. Read from LBA 2 (byte 1024).
            // Magic at file byte 1080 = buffer offset 56.
            let sb_data = session.read_blocks(2, 8).await?;
            if sb_data.len() >= 58 {
                let magic = u16::from_le_bytes([sb_data[56], sb_data[57]]);
                if magic == 0xEF53 {
                    println!("  Verification: ext4 superblock OK (magic=0xEF53)");
                } else {
                    println!("  Verification: unexpected magic 0x{magic:04x}");
                }
            }

            session.logout().await?;
            println!("\nDone. Target is now ext4-formatted.");
        }

        Commands::Test { name } => {
            println!("=== Full iSCSI PVC Lifecycle Test ===\n");
            let file_path = format!("raid1/volumes/pvc/{name}.raw");

            // Step 1: Create
            println!("Step 1: Creating PVC...");
            let (disk_id, iqn, port) = ros.create_iscsi_target(&file_path, Some("100M")).await?;
            println!("  Disk ID: {disk_id}");
            println!("  IQN: {iqn}");
            println!("  Portal: {}:{port}\n", cli.portal);

            // Step 2: Connect and probe
            println!("Step 2: Connecting iSCSI initiator...");
            let portal: SocketAddr = format!("{}:{port}", cli.portal).parse()?;
            let mut session = IscsiInitiator::connect(
                portal,
                &iqn,
                "iqn.2024-01.io.vkube:iscsi-pvc-tool",
            )
            .await?;

            session.test_unit_ready().await?;
            let cap = session.read_capacity().await?;
            println!("  Capacity: {cap}\n");

            // Step 3: Format
            println!("Step 3: Formatting as ext4...");
            let uuid = generate_uuid();
            {
                let mut writer = IscsiBlockWriter {
                    session: &mut session,
                    block_size: 4096,
                    sector_size: cap.block_size,
                };
                iscsi_pvc::ext4::format_ext4(&mut writer, cap.total_bytes, uuid, "pvc-test").await?;
            }
            println!("  Format complete!\n");

            // Step 4: Verify — superblock at file byte 1024, magic at 1080 = LBA 2 offset 56
            println!("Step 4: Verifying...");
            let sb_data = session.read_blocks(2, 2).await?;
            if sb_data.len() >= 58 {
                let magic = u16::from_le_bytes([sb_data[56], sb_data[57]]);
                if magic == 0xEF53 {
                    println!("  ext4 superblock: OK");
                } else {
                    bail!("superblock verification failed: magic=0x{magic:04x}");
                }
            }

            // Step 5: Logout
            println!("\nStep 5: Disconnecting...");
            session.logout().await?;

            // Step 6: Cleanup
            println!("Step 6: Cleaning up...");
            ros.delete_iscsi_target(&disk_id).await?;
            println!("  PVC deleted.\n");

            println!("=== Test PASSED ===");
        }
    }

    Ok(())
}

/// Adapter to write 4K ext4 blocks via iSCSI SCSI WRITE(10).
struct IscsiBlockWriter<'a> {
    session: &'a mut IscsiInitiator,
    block_size: u32,
    sector_size: u32,
}

#[async_trait::async_trait]
impl iscsi_pvc::ext4::BlockWriter for IscsiBlockWriter<'_> {
    async fn write_block(&mut self, block_num: u64, data: &[u8]) -> Result<()> {
        // Convert ext4 block number to iSCSI sector LBA
        let sectors_per_block = self.block_size / self.sector_size;
        let lba = (block_num * sectors_per_block as u64) as u32;

        // Pad data to exact block size if needed
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
    use std::time::{SystemTime, UNIX_EPOCH};
    let t = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let mut uuid = [0u8; 16];
    let nanos = t.as_nanos();
    uuid[0..8].copy_from_slice(&nanos.to_le_bytes()[..8]);
    // Mix in process ID
    let pid = std::process::id();
    uuid[8..12].copy_from_slice(&pid.to_le_bytes());
    // Version 4 UUID markers
    uuid[6] = (uuid[6] & 0x0F) | 0x40;
    uuid[8] = (uuid[8] & 0x3F) | 0x80;
    uuid
}
