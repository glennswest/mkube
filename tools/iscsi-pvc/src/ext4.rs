//! Minimal ext4 filesystem formatter.
//!
//! Writes enough ext4 structures to create a mountable filesystem
//! directly over iSCSI block writes. No mkfs.ext4 dependency.
//!
//! Layout for a small volume (< 256 MB):
//!   Block 0:    Boot block (1024 bytes padding)
//!   Block 0+:   Superblock (at byte offset 1024)
//!   Block 1:    Group descriptor table
//!   Block 2:    Block bitmap
//!   Block 3:    Inode bitmap
//!   Block 4-N:  Inode table
//!   Block N+1+: Data blocks

use anyhow::Result;
use byteorder::{LittleEndian, ByteOrder};

const EXT4_SUPER_MAGIC: u16 = 0xEF53;
const EXT4_BLOCK_SIZE: u32 = 4096; // 4KB blocks
const EXT4_INODE_SIZE: u16 = 256;
const EXT4_INODES_PER_GROUP: u32 = 8192;
const EXT4_LOG_BLOCK_SIZE: u32 = 2; // log2(4096/1024) = 2

// Feature flags
const EXT4_FEATURE_COMPAT_EXT_ATTR: u32 = 0x0008;
const EXT4_FEATURE_INCOMPAT_FILETYPE: u32 = 0x0002;
const EXT4_FEATURE_INCOMPAT_EXTENTS: u32 = 0x0040;
const EXT4_FEATURE_INCOMPAT_64BIT: u32 = 0x0080;
const EXT4_FEATURE_INCOMPAT_FLEX_BG: u32 = 0x0200;
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

/// Generate an ext4 filesystem on a block device.
/// `total_bytes` is the device size.
pub async fn format_ext4(writer: &mut dyn BlockWriter, total_bytes: u64, uuid: [u8; 16], label: &str) -> Result<()> {
    let block_size = writer.block_size();
    let total_blocks = total_bytes / block_size as u64;

    // Calculate group parameters
    let blocks_per_group: u32 = block_size * 8; // bitmap covers block_size*8 blocks
    let group_count = ((total_blocks + blocks_per_group as u64 - 1) / blocks_per_group as u64) as u32;
    let inodes_per_group = EXT4_INODES_PER_GROUP;
    let total_inodes = inodes_per_group * group_count;
    let inode_table_blocks = (inodes_per_group * EXT4_INODE_SIZE as u32 + block_size - 1) / block_size;

    // First data block after metadata in group 0
    // Layout: superblock(1) + GDT(1) + block_bitmap(1) + inode_bitmap(1) + inode_table(N)
    let overhead_blocks = 1 + 1 + 1 + 1 + inode_table_blocks; // GDT + bitmaps + inodes
    let first_data_block = if block_size == 1024 { 1u32 } else { 0u32 };
    let sb_block = if block_size == 1024 { 1 } else { 0 };

    // --- Superblock ---
    let mut sb = vec![0u8; block_size as usize];
    // If block_size > 1024, superblock is at offset 1024 within block 0
    let sb_off = if block_size > 1024 { 1024 } else { 0 };

    // Total inodes
    LittleEndian::write_u32(&mut sb[sb_off + 0..], total_inodes);
    // Total blocks (lo)
    LittleEndian::write_u32(&mut sb[sb_off + 4..], total_blocks as u32);
    // Reserved blocks (5%)
    let reserved = total_blocks / 20;
    LittleEndian::write_u32(&mut sb[sb_off + 8..], reserved as u32);
    // Free blocks (lo)
    let free_blocks = total_blocks.saturating_sub(overhead_blocks as u64 + first_data_block as u64);
    LittleEndian::write_u32(&mut sb[sb_off + 12..], free_blocks as u32);
    // Free inodes (total - 11 reserved)
    let free_inodes = total_inodes.saturating_sub(11);
    LittleEndian::write_u32(&mut sb[sb_off + 16..], free_inodes);
    // First data block
    LittleEndian::write_u32(&mut sb[sb_off + 20..], first_data_block);
    // Log block size
    LittleEndian::write_u32(&mut sb[sb_off + 24..], EXT4_LOG_BLOCK_SIZE);
    // Log cluster size (same as block)
    LittleEndian::write_u32(&mut sb[sb_off + 28..], EXT4_LOG_BLOCK_SIZE);
    // Blocks per group
    LittleEndian::write_u32(&mut sb[sb_off + 32..], blocks_per_group);
    // Clusters per group (same)
    LittleEndian::write_u32(&mut sb[sb_off + 36..], blocks_per_group);
    // Inodes per group
    LittleEndian::write_u32(&mut sb[sb_off + 40..], inodes_per_group);
    // Mount time (0)
    // Write time
    LittleEndian::write_u32(&mut sb[sb_off + 48..], unix_timestamp());
    // Mount count
    LittleEndian::write_u16(&mut sb[sb_off + 52..], 0);
    // Max mount count
    LittleEndian::write_u16(&mut sb[sb_off + 54..], u16::MAX);
    // Magic
    LittleEndian::write_u16(&mut sb[sb_off + 56..], EXT4_SUPER_MAGIC);
    // State: clean
    LittleEndian::write_u16(&mut sb[sb_off + 58..], 1);
    // Error behavior: continue
    LittleEndian::write_u16(&mut sb[sb_off + 60..], 1);
    // Minor revision
    LittleEndian::write_u16(&mut sb[sb_off + 62..], 0);
    // Last check time
    LittleEndian::write_u32(&mut sb[sb_off + 64..], unix_timestamp());
    // Check interval (6 months)
    LittleEndian::write_u32(&mut sb[sb_off + 68..], 15552000);
    // Creator OS: Linux
    LittleEndian::write_u32(&mut sb[sb_off + 72..], 0);
    // Revision level: 1 (dynamic)
    LittleEndian::write_u32(&mut sb[sb_off + 76..], 1);
    // Default reserved uid/gid
    LittleEndian::write_u16(&mut sb[sb_off + 80..], 0);
    LittleEndian::write_u16(&mut sb[sb_off + 82..], 0);

    // --- Extended superblock fields (rev 1+) ---
    // First non-reserved inode
    LittleEndian::write_u32(&mut sb[sb_off + 84..], 11);
    // Inode size
    LittleEndian::write_u16(&mut sb[sb_off + 88..], EXT4_INODE_SIZE);
    // Block group of this superblock
    LittleEndian::write_u16(&mut sb[sb_off + 90..], 0);
    // Compatible features
    LittleEndian::write_u32(&mut sb[sb_off + 92..], EXT4_FEATURE_COMPAT_EXT_ATTR);
    // Incompatible features
    LittleEndian::write_u32(
        &mut sb[sb_off + 96..],
        EXT4_FEATURE_INCOMPAT_FILETYPE
            | EXT4_FEATURE_INCOMPAT_EXTENTS
            | EXT4_FEATURE_INCOMPAT_FLEX_BG,
    );
    // Read-only compatible features
    LittleEndian::write_u32(
        &mut sb[sb_off + 100..],
        EXT4_FEATURE_RO_COMPAT_SPARSE_SUPER
            | EXT4_FEATURE_RO_COMPAT_LARGE_FILE
            | EXT4_FEATURE_RO_COMPAT_EXTRA_ISIZE,
    );
    // UUID (128-bit)
    sb[sb_off + 104..sb_off + 120].copy_from_slice(&uuid);
    // Volume label (16 bytes)
    let label_bytes = label.as_bytes();
    let len = label_bytes.len().min(16);
    sb[sb_off + 120..sb_off + 120 + len].copy_from_slice(&label_bytes[..len]);

    // Desc size (for 64-bit feature)
    LittleEndian::write_u16(&mut sb[sb_off + 254..], 64);
    // Inode extra size (want_extra_isize)
    LittleEndian::write_u16(&mut sb[sb_off + 256..], 32);

    // Flex BG size (log2) — 4 means 16 groups per flex group
    sb[sb_off + 372] = 4;

    // Total blocks hi (offset 336 = 0x150)
    LittleEndian::write_u32(&mut sb[sb_off + 336..], (total_blocks >> 32) as u32);

    // Write superblock
    writer.write_block(sb_block, &sb).await?;

    // --- Group Descriptor Table ---
    let mut gdt = vec![0u8; block_size as usize];
    let gd_size = 64usize; // ext4 64-byte group descriptors

    for g in 0..group_count.min((block_size as usize / gd_size) as u32) {
        let off = g as usize * gd_size;
        let group_start = first_data_block as u64 + g as u64 * blocks_per_group as u64;

        // Block bitmap location
        let bb_block = if g == 0 { sb_block + 2 } else { group_start + 1 };
        LittleEndian::write_u32(&mut gdt[off..], bb_block as u32);
        // Inode bitmap location
        let ib_block = bb_block + 1;
        LittleEndian::write_u32(&mut gdt[off + 4..], ib_block as u32);
        // Inode table location
        let it_block = ib_block + 1;
        LittleEndian::write_u32(&mut gdt[off + 8..], it_block as u32);
        // Free blocks count in group
        let group_end = ((g as u64 + 1) * blocks_per_group as u64).min(total_blocks);
        let group_total = group_end.saturating_sub(group_start);
        let group_overhead = if g == 0 { overhead_blocks as u64 + 1 } else { 1 + 1 + 1 + inode_table_blocks as u64 };
        let group_free = group_total.saturating_sub(group_overhead);
        LittleEndian::write_u16(&mut gdt[off + 12..], group_free as u16);
        // Free inodes count in group
        let group_free_inodes = if g == 0 { inodes_per_group - 11 } else { inodes_per_group };
        LittleEndian::write_u16(&mut gdt[off + 14..], group_free_inodes as u16);
        // Used dirs count in group
        LittleEndian::write_u16(&mut gdt[off + 16..], if g == 0 { 2 } else { 0 });
        // Flags
        LittleEndian::write_u16(&mut gdt[off + 18..], 0);
        // Checksum
        LittleEndian::write_u16(&mut gdt[off + 30..], 0); // placeholder
    }

    writer.write_block(sb_block + 1, &gdt).await?;

    // --- Block Bitmap (group 0) ---
    let mut bb = vec![0u8; block_size as usize];
    // Mark metadata blocks as used: first overhead_blocks + superblock
    let used_bits = overhead_blocks + 1; // +1 for the superblock block
    for i in 0..used_bits {
        bb[i as usize / 8] |= 1 << (i % 8);
    }
    writer.write_block(sb_block + 2, &bb).await?;

    // --- Inode Bitmap (group 0) ---
    let mut ib = vec![0u8; block_size as usize];
    // Mark first 11 inodes as used (1-11 are reserved, including root inode 2)
    ib[0] = 0xFF; // inodes 1-8
    ib[1] = 0x07; // inodes 9-11
    writer.write_block(sb_block + 3, &ib).await?;

    // --- Inode Table (group 0) ---
    // Write root directory inode (inode 2)
    let mut inode_block = vec![0u8; block_size as usize];
    let root_inode_off = 1 * EXT4_INODE_SIZE as usize; // inode 2 is at index 1 (0-based)

    // Mode: directory + 0755
    LittleEndian::write_u16(&mut inode_block[root_inode_off..], 0x41ED); // S_IFDIR | 0755
    // UID
    LittleEndian::write_u16(&mut inode_block[root_inode_off + 2..], 0);
    // Size (one block for directory entries)
    LittleEndian::write_u32(&mut inode_block[root_inode_off + 4..], block_size);
    // Access time
    LittleEndian::write_u32(&mut inode_block[root_inode_off + 8..], unix_timestamp());
    // Change time
    LittleEndian::write_u32(&mut inode_block[root_inode_off + 12..], unix_timestamp());
    // Modification time
    LittleEndian::write_u32(&mut inode_block[root_inode_off + 16..], unix_timestamp());
    // Delete time
    LittleEndian::write_u32(&mut inode_block[root_inode_off + 20..], 0);
    // GID
    LittleEndian::write_u16(&mut inode_block[root_inode_off + 24..], 0);
    // Links count (. and ..)
    LittleEndian::write_u16(&mut inode_block[root_inode_off + 26..], 2);
    // Blocks count (in 512-byte units)
    LittleEndian::write_u32(&mut inode_block[root_inode_off + 28..], block_size / 512);

    // Flags: EXT4_EXTENTS_FL
    LittleEndian::write_u32(&mut inode_block[root_inode_off + 32..], 0x00080000);

    // i_block[0..14] used for extent tree header + first extent
    let ib_off = root_inode_off + 40;

    // Extent tree header
    LittleEndian::write_u16(&mut inode_block[ib_off..], 0xF30A); // magic
    LittleEndian::write_u16(&mut inode_block[ib_off + 2..], 1); // entries
    LittleEndian::write_u16(&mut inode_block[ib_off + 4..], 4); // max entries
    LittleEndian::write_u16(&mut inode_block[ib_off + 6..], 0); // depth
    LittleEndian::write_u32(&mut inode_block[ib_off + 8..], 0); // generation

    // First extent: logical block 0, length 1, physical = first data block after inode table
    let root_data_block = (sb_block + 4 + inode_table_blocks as u64) as u32;
    LittleEndian::write_u32(&mut inode_block[ib_off + 12..], 0); // logical block
    LittleEndian::write_u16(&mut inode_block[ib_off + 16..], 1); // length
    LittleEndian::write_u16(&mut inode_block[ib_off + 18..], 0); // start_hi
    LittleEndian::write_u32(&mut inode_block[ib_off + 20..], root_data_block); // start_lo

    // Extra isize (256 - 128 = 128, but standard is 32)
    LittleEndian::write_u16(&mut inode_block[root_inode_off + 128..], 32);

    writer.write_block(sb_block + 4, &inode_block).await?;

    // Write remaining inode table blocks as zeros
    let zero_block = vec![0u8; block_size as usize];
    for i in 1..inode_table_blocks {
        writer.write_block(sb_block + 4 + i as u64, &zero_block).await?;
    }

    // --- Root directory data block ---
    let mut dir_block = vec![0u8; block_size as usize];
    let mut doff = 0usize;

    // "." entry (inode 2)
    LittleEndian::write_u32(&mut dir_block[doff..], 2); // inode
    LittleEndian::write_u16(&mut dir_block[doff + 4..], 12); // rec_len
    dir_block[doff + 6] = 1; // name_len
    dir_block[doff + 7] = 2; // file_type = directory
    dir_block[doff + 8] = b'.';
    doff += 12;

    // ".." entry (inode 2, root's parent is itself)
    LittleEndian::write_u32(&mut dir_block[doff..], 2); // inode
    LittleEndian::write_u16(&mut dir_block[doff + 4..], (block_size as usize - doff) as u16); // rec_len (fills rest)
    dir_block[doff + 6] = 2; // name_len
    dir_block[doff + 7] = 2; // file_type = directory
    dir_block[doff + 8] = b'.';
    dir_block[doff + 9] = b'.';

    // Mark this data block in the bitmap
    let db_bit = root_data_block as usize;
    bb[db_bit / 8] |= 1 << (db_bit % 8);
    writer.write_block(sb_block + 2, &bb).await?; // re-write block bitmap

    writer.write_block(root_data_block as u64, &dir_block).await?;

    // Write lost+found directory (inode 11)
    // Minimal: just mark inode 11 as used (already done in bitmap)
    // A real mkfs creates lost+found but for a mountable fs, root dir is enough.

    tracing::info!(
        "ext4 formatted: {} blocks, {} inodes, {} groups, label={:?}",
        total_blocks,
        total_inodes,
        group_count,
        label,
    );

    Ok(())
}

fn unix_timestamp() -> u32 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as u32
}
