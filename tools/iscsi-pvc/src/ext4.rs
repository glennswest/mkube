//! Minimal ext4 filesystem formatter.
//!
//! Writes enough ext4 structures to create a mountable filesystem
//! directly over iSCSI block writes. No mkfs.ext4 dependency.
//!
//! Supports multiple block groups (volumes > 128 MiB) with proper
//! sparse_super backup superblock placement.

use anyhow::Result;
use byteorder::{LittleEndian, ByteOrder};

const EXT4_SUPER_MAGIC: u16 = 0xEF53;
const EXT4_BLOCK_SIZE: u32 = 4096; // 4KB blocks
const EXT4_INODE_SIZE: u16 = 256;
const EXT4_LOG_BLOCK_SIZE: u32 = 2; // log2(4096/1024) = 2

// Feature flags
const EXT4_FEATURE_COMPAT_EXT_ATTR: u32 = 0x0008;
const EXT4_FEATURE_INCOMPAT_FILETYPE: u32 = 0x0002;
const EXT4_FEATURE_INCOMPAT_EXTENTS: u32 = 0x0040;
const EXT4_FEATURE_RO_COMPAT_SPARSE_SUPER: u32 = 0x0001;
const EXT4_FEATURE_RO_COMPAT_LARGE_FILE: u32 = 0x0002;
const EXT4_FEATURE_RO_COMPAT_EXTRA_ISIZE: u32 = 0x0040;

/// A block writer trait — abstracts over iSCSI or local file.
#[async_trait::async_trait]
pub trait BlockWriter: Send {
    /// Write a full block at the given block number.
    async fn write_block(&mut self, block_num: u64, data: &[u8]) -> Result<()>;
    /// Block size in bytes.
    fn block_size(&self) -> u32 {
        EXT4_BLOCK_SIZE
    }
}

/// Returns true if a block group has a backup superblock (sparse_super feature).
/// Groups 0, 1, and powers of 3, 5, 7 have backup superblocks.
fn has_backup_super(group: u32) -> bool {
    if group <= 1 {
        return true;
    }
    for base in [3u32, 5, 7] {
        let mut p = base;
        while p < group {
            p *= base;
        }
        if p == group {
            return true;
        }
    }
    false
}

/// Per-group metadata layout.
struct GroupLayout {
    /// First block of this group
    group_start: u64,
    /// Block number of the block bitmap
    block_bitmap: u64,
    /// Block number of the inode bitmap
    inode_bitmap: u64,
    /// Block number of the inode table start
    inode_table: u64,
    /// Total metadata overhead blocks in this group
    overhead: u64,
    /// Usable blocks in this group
    total_blocks: u64,
    /// Free data blocks in this group
    free_blocks: u64,
}

/// Generate an ext4 filesystem on a block device.
/// `total_bytes` is the device size.
pub async fn format_ext4(writer: &mut dyn BlockWriter, total_bytes: u64, uuid: [u8; 16], label: &str) -> Result<()> {
    let block_size = writer.block_size();
    let total_blocks = total_bytes / block_size as u64;

    // RouterOS requires >= 1024 blocks (4 MiB with 4K blocks) for ext4.
    anyhow::ensure!(
        total_blocks >= 1024,
        "volume too small for ext4: {} blocks ({} bytes), minimum 1024 blocks (4 MiB)",
        total_blocks, total_bytes
    );

    // Calculate group parameters
    let blocks_per_group: u32 = block_size * 8; // bitmap covers block_size*8 blocks
    let group_count = ((total_blocks + blocks_per_group as u64 - 1) / blocks_per_group as u64) as u32;

    // Scale inodes to volume size: 1 inode per 16 KB (like mkfs.ext4 default).
    // Round up to multiple of (block_size / inode_size) so inode table fills whole blocks.
    let inodes_per_block = block_size / EXT4_INODE_SIZE as u32; // 16 for 4K/256
    let raw_inodes = (total_bytes / 16384) as u32;
    let per_group_raw = (raw_inodes / group_count).max(inodes_per_block);
    // Enforce minimum 256 inodes/group — RouterOS needs sufficient inode table density.
    let per_group_clamped = per_group_raw.max(256);
    let inodes_per_group = ((per_group_clamped + inodes_per_block - 1) / inodes_per_block) * inodes_per_block;
    let total_inodes = inodes_per_group * group_count;
    let inode_table_blocks = (inodes_per_group * EXT4_INODE_SIZE as u32) / block_size;
    let first_data_block: u32 = if block_size == 1024 { 1 } else { 0 };
    let sb_block: u64 = first_data_block as u64;
    let gdt_blocks: u64 = 1; // 1 block for GDT (supports up to 64 groups with 64-byte descriptors)

    // Calculate layout for each group
    let mut groups: Vec<GroupLayout> = Vec::with_capacity(group_count as usize);
    let mut total_free_blocks: u64 = 0;

    for g in 0..group_count {
        let group_start = first_data_block as u64 + g as u64 * blocks_per_group as u64;
        let group_end = ((g as u64 + 1) * blocks_per_group as u64 + first_data_block as u64).min(total_blocks);
        let group_blocks = group_end.saturating_sub(group_start);

        // Groups with backup superblock have SB + GDT before metadata
        let backup = if has_backup_super(g) { 1 + gdt_blocks } else { 0 };

        // Metadata: backup(0 or 2) + block_bitmap(1) + inode_bitmap(1) + inode_table(N)
        let bb = group_start + backup;
        let ib = bb + 1;
        let it = ib + 1;
        let overhead = backup + 1 + 1 + inode_table_blocks as u64;
        let free = group_blocks.saturating_sub(overhead);

        total_free_blocks += free;

        groups.push(GroupLayout {
            group_start,
            block_bitmap: bb,
            inode_bitmap: ib,
            inode_table: it,
            overhead,
            total_blocks: group_blocks,
            free_blocks: free,
        });
    }

    // Root directory uses 1 data block from group 0
    let root_data_block = groups[0].inode_table + inode_table_blocks as u64;
    total_free_blocks -= 1; // root dir block
    groups[0].free_blocks -= 1;

    // --- Build superblock ---
    let sb_data = build_superblock(
        block_size, total_blocks, total_inodes, total_free_blocks,
        first_data_block, blocks_per_group, inodes_per_group,
        &uuid, label, group_count,
    );

    // Write primary superblock (group 0)
    writer.write_block(sb_block, &sb_data).await?;

    // --- Build GDT ---
    let gdt_data = build_gdt(block_size, &groups, inodes_per_group, inode_table_blocks);

    // Write primary GDT (group 0)
    writer.write_block(sb_block + 1, &gdt_data).await?;

    // --- Write per-group metadata ---
    let zero_block = vec![0u8; block_size as usize];

    for (g, layout) in groups.iter().enumerate() {
        // Write backup superblock + GDT for groups with backups (skip group 0 = primary)
        if g > 0 && has_backup_super(g as u32) {
            // Backup superblock with updated block_group_nr field
            let mut backup_sb = sb_data.clone();
            let sb_off = if block_size > 1024 { 1024 } else { 0 };
            LittleEndian::write_u16(&mut backup_sb[sb_off + 90..], g as u16);
            writer.write_block(layout.group_start, &backup_sb).await?;
            writer.write_block(layout.group_start + 1, &gdt_data).await?;
        }

        // Block bitmap
        let mut bb = vec![0u8; block_size as usize];
        // Mark overhead blocks as used (from bit 0 within this group)
        // Overhead includes backup SB/GDT + bitmaps + inode table
        for i in 0..layout.overhead {
            bb[i as usize / 8] |= 1 << (i as usize % 8);
        }
        // Group 0: also mark root directory data block
        if g == 0 {
            let db_bit = (root_data_block - layout.group_start) as usize;
            bb[db_bit / 8] |= 1 << (db_bit % 8);
        }
        // Mark blocks beyond group end as used (partial last group)
        let group_end_block = layout.total_blocks;
        for i in group_end_block..blocks_per_group as u64 {
            bb[i as usize / 8] |= 1 << (i as usize % 8);
        }
        writer.write_block(layout.block_bitmap, &bb).await?;

        // Inode bitmap
        let mut ib = vec![0u8; block_size as usize];
        if g == 0 {
            // Mark first 11 inodes as used (1-11 reserved, including root inode 2)
            ib[0] = 0xFF; // inodes 1-8
            ib[1] = 0x07; // inodes 9-11
        }
        // Mark inodes beyond actual count as used (partial last group)
        // This only matters for the last group if total_inodes % inodes_per_group != 0
        writer.write_block(layout.inode_bitmap, &ib).await?;

        // Inode table (all groups need initialized inode table)
        if g == 0 {
            // Group 0: write root directory inode at inode 2
            let mut inode_block = vec![0u8; block_size as usize];
            write_root_inode(&mut inode_block, block_size, root_data_block as u32, inode_table_blocks);
            writer.write_block(layout.inode_table, &inode_block).await?;

            // Remaining inode table blocks for group 0
            for i in 1..inode_table_blocks {
                writer.write_block(layout.inode_table + i as u64, &zero_block).await?;
            }
        } else {
            // Other groups: zero inode table
            for i in 0..inode_table_blocks {
                writer.write_block(layout.inode_table + i as u64, &zero_block).await?;
            }
        }
    }

    // --- Root directory data block ---
    let dir_block = build_root_dir(block_size);
    writer.write_block(root_data_block, &dir_block).await?;

    tracing::info!(
        "ext4 formatted: {} blocks, {} inodes, {} groups, label={:?}",
        total_blocks,
        total_inodes,
        group_count,
        label,
    );

    Ok(())
}

fn build_superblock(
    block_size: u32, total_blocks: u64, total_inodes: u32, free_blocks: u64,
    first_data_block: u32, blocks_per_group: u32, inodes_per_group: u32,
    uuid: &[u8; 16], label: &str, group_count: u32,
) -> Vec<u8> {
    let mut sb = vec![0u8; block_size as usize];
    let sb_off = if block_size > 1024 { 1024 } else { 0 };

    let free_inodes = total_inodes.saturating_sub(11);
    let reserved = total_blocks / 20;

    // Core superblock fields
    LittleEndian::write_u32(&mut sb[sb_off + 0..], total_inodes);
    LittleEndian::write_u32(&mut sb[sb_off + 4..], total_blocks as u32);
    LittleEndian::write_u32(&mut sb[sb_off + 8..], reserved as u32);
    LittleEndian::write_u32(&mut sb[sb_off + 12..], free_blocks as u32);
    LittleEndian::write_u32(&mut sb[sb_off + 16..], free_inodes);
    LittleEndian::write_u32(&mut sb[sb_off + 20..], first_data_block);
    LittleEndian::write_u32(&mut sb[sb_off + 24..], EXT4_LOG_BLOCK_SIZE);
    LittleEndian::write_u32(&mut sb[sb_off + 28..], EXT4_LOG_BLOCK_SIZE);
    LittleEndian::write_u32(&mut sb[sb_off + 32..], blocks_per_group);
    LittleEndian::write_u32(&mut sb[sb_off + 36..], blocks_per_group);
    LittleEndian::write_u32(&mut sb[sb_off + 40..], inodes_per_group);
    LittleEndian::write_u32(&mut sb[sb_off + 48..], unix_timestamp()); // write time
    LittleEndian::write_u16(&mut sb[sb_off + 52..], 0); // mount count
    LittleEndian::write_u16(&mut sb[sb_off + 54..], u16::MAX); // max mount count
    LittleEndian::write_u16(&mut sb[sb_off + 56..], EXT4_SUPER_MAGIC);
    LittleEndian::write_u16(&mut sb[sb_off + 58..], 1); // state: clean
    LittleEndian::write_u16(&mut sb[sb_off + 60..], 1); // error behavior: continue
    LittleEndian::write_u16(&mut sb[sb_off + 62..], 0); // minor revision
    LittleEndian::write_u32(&mut sb[sb_off + 64..], unix_timestamp()); // last check
    LittleEndian::write_u32(&mut sb[sb_off + 68..], 15552000); // check interval (6 months)
    LittleEndian::write_u32(&mut sb[sb_off + 72..], 0); // creator OS: Linux
    LittleEndian::write_u32(&mut sb[sb_off + 76..], 1); // revision level: dynamic

    // Extended superblock fields (rev 1+)
    LittleEndian::write_u32(&mut sb[sb_off + 84..], 11); // first non-reserved inode
    LittleEndian::write_u16(&mut sb[sb_off + 88..], EXT4_INODE_SIZE);
    LittleEndian::write_u16(&mut sb[sb_off + 90..], 0); // block group of this superblock

    // Feature flags
    LittleEndian::write_u32(&mut sb[sb_off + 92..], EXT4_FEATURE_COMPAT_EXT_ATTR);
    LittleEndian::write_u32(
        &mut sb[sb_off + 96..],
        EXT4_FEATURE_INCOMPAT_FILETYPE
            | EXT4_FEATURE_INCOMPAT_EXTENTS,
    );
    LittleEndian::write_u32(
        &mut sb[sb_off + 100..],
        EXT4_FEATURE_RO_COMPAT_SPARSE_SUPER
            | EXT4_FEATURE_RO_COMPAT_LARGE_FILE
            | EXT4_FEATURE_RO_COMPAT_EXTRA_ISIZE,
    );

    // UUID + label
    sb[sb_off + 104..sb_off + 120].copy_from_slice(uuid);
    let label_bytes = label.as_bytes();
    let len = label_bytes.len().min(16);
    sb[sb_off + 120..sb_off + 120 + len].copy_from_slice(&label_bytes[..len]);

    // Desc size — 32-byte group descriptors (64BIT feature not enabled)
    LittleEndian::write_u16(&mut sb[sb_off + 254..], 32);
    // Inode extra size
    LittleEndian::write_u16(&mut sb[sb_off + 256..], 32);

    // Total blocks hi (offset 336)
    LittleEndian::write_u32(&mut sb[sb_off + 336..], (total_blocks >> 32) as u32);

    // Reserved block count hi (offset 340)
    LittleEndian::write_u32(&mut sb[sb_off + 340..], (reserved >> 32) as u32);

    // Free blocks hi (offset 344)
    LittleEndian::write_u32(&mut sb[sb_off + 344..], (free_blocks >> 32) as u32);

    // Verify group descriptors fit in single GDT block (32 bytes per descriptor)
    let _gdt_entries_per_block = block_size as u32 / 32;
    assert!(group_count <= _gdt_entries_per_block, "too many groups for single GDT block");

    sb
}

fn build_gdt(block_size: u32, groups: &[GroupLayout], inodes_per_group: u32, _inode_table_blocks: u32) -> Vec<u8> {
    let mut gdt = vec![0u8; block_size as usize];
    let gd_size = 32usize; // 32-byte descriptors (no 64BIT feature)

    for (g, layout) in groups.iter().enumerate() {
        let off = g * gd_size;

        // Block bitmap location
        LittleEndian::write_u32(&mut gdt[off..], layout.block_bitmap as u32);
        // Inode bitmap location
        LittleEndian::write_u32(&mut gdt[off + 4..], layout.inode_bitmap as u32);
        // Inode table location
        LittleEndian::write_u32(&mut gdt[off + 8..], layout.inode_table as u32);
        // Free blocks count (16-bit)
        LittleEndian::write_u16(&mut gdt[off + 12..], layout.free_blocks as u16);
        // Free inodes count (16-bit)
        let free_inodes = if g == 0 { inodes_per_group - 11 } else { inodes_per_group };
        LittleEndian::write_u16(&mut gdt[off + 14..], free_inodes as u16);
        // Used dirs count (16-bit)
        LittleEndian::write_u16(&mut gdt[off + 16..], if g == 0 { 2 } else { 0 });
    }

    gdt
}

fn write_root_inode(inode_block: &mut [u8], block_size: u32, root_data_block: u32, _inode_table_blocks: u32) {
    let off = 1 * EXT4_INODE_SIZE as usize; // inode 2 at index 1 (0-based)

    // Mode: directory + 0755
    LittleEndian::write_u16(&mut inode_block[off..], 0x41ED);
    // Size (one block)
    LittleEndian::write_u32(&mut inode_block[off + 4..], block_size);
    // Access/change/modification time
    let ts = unix_timestamp();
    LittleEndian::write_u32(&mut inode_block[off + 8..], ts);
    LittleEndian::write_u32(&mut inode_block[off + 12..], ts);
    LittleEndian::write_u32(&mut inode_block[off + 16..], ts);
    // Links count (. and ..)
    LittleEndian::write_u16(&mut inode_block[off + 26..], 2);
    // Blocks count (in 512-byte units)
    LittleEndian::write_u32(&mut inode_block[off + 28..], block_size / 512);
    // Flags: EXT4_EXTENTS_FL
    LittleEndian::write_u32(&mut inode_block[off + 32..], 0x00080000);

    // Extent tree header
    let ib = off + 40;
    LittleEndian::write_u16(&mut inode_block[ib..], 0xF30A); // magic
    LittleEndian::write_u16(&mut inode_block[ib + 2..], 1); // entries
    LittleEndian::write_u16(&mut inode_block[ib + 4..], 4); // max entries
    LittleEndian::write_u16(&mut inode_block[ib + 6..], 0); // depth

    // First extent: logical block 0, 1 block, physical = root_data_block
    LittleEndian::write_u32(&mut inode_block[ib + 12..], 0); // logical block
    LittleEndian::write_u16(&mut inode_block[ib + 16..], 1); // length
    LittleEndian::write_u16(&mut inode_block[ib + 18..], 0); // start_hi
    LittleEndian::write_u32(&mut inode_block[ib + 20..], root_data_block); // start_lo

    // Extra isize
    LittleEndian::write_u16(&mut inode_block[off + 128..], 32);
}

fn build_root_dir(block_size: u32) -> Vec<u8> {
    let mut dir = vec![0u8; block_size as usize];

    // "." entry (inode 2)
    LittleEndian::write_u32(&mut dir[0..], 2);
    LittleEndian::write_u16(&mut dir[4..], 12);
    dir[6] = 1; // name_len
    dir[7] = 2; // file_type = directory
    dir[8] = b'.';

    // ".." entry (inode 2, root's parent is itself)
    LittleEndian::write_u32(&mut dir[12..], 2);
    LittleEndian::write_u16(&mut dir[16..], (block_size as usize - 12) as u16);
    dir[18] = 2; // name_len
    dir[19] = 2; // file_type = directory
    dir[20] = b'.';
    dir[21] = b'.';

    dir
}

fn unix_timestamp() -> u32 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as u32
}
