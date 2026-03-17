package diskimg

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// QCOW2 constants
const (
	qcow2Magic   = 0x514649FB // "QFI\xfb"
	qcow2Version = 2          // also handles version 3

	// Compression type values
	qcow2CompNone    = 0
	qcow2CompDeflate = 1 // zlib/deflate

	// L2 table entry flags
	qcow2L2Compressed = 1 << 62
	qcow2L2Standard   = 0 // bit 63 = 0, bit 62 = 0
)

// qcow2Header is the QCOW2 file header (on-disk, big-endian).
type qcow2Header struct {
	Magic                 uint32
	Version               uint32
	BackingFileOffset     uint64
	BackingFileSize       uint32
	ClusterBits           uint32
	Size                  uint64 // virtual size in bytes
	CryptMethod           uint32
	L1Size                uint32 // number of entries in L1 table
	L1TableOffset         uint64
	RefcountTableOffset   uint64
	RefcountTableClusters uint32
	NbSnapshots           uint32
	SnapshotsOffset       uint64
}

// convertQCOW2 converts a QCOW2 disk image to raw format.
func convertQCOW2(inputPath, outputPath string) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("opening qcow2: %w", err)
	}
	defer f.Close()

	// Read header (big-endian)
	var hdr qcow2Header
	if err := binary.Read(f, binary.BigEndian, &hdr); err != nil {
		return fmt.Errorf("reading qcow2 header: %w", err)
	}

	if hdr.Magic != qcow2Magic {
		return fmt.Errorf("not a QCOW2 file: magic %#x", hdr.Magic)
	}
	if hdr.Version != 2 && hdr.Version != 3 {
		return fmt.Errorf("unsupported QCOW2 version: %d", hdr.Version)
	}
	if hdr.BackingFileOffset != 0 {
		return fmt.Errorf("QCOW2 backing files (snapshots) not supported — convert backing chain first")
	}

	clusterSize := int64(1) << hdr.ClusterBits
	virtualSize := int64(hdr.Size)
	l1Size := int(hdr.L1Size)
	l2EntriesPerCluster := clusterSize / 8 // each L2 entry is 8 bytes

	// Create output file
	out, err := createSparseOutput(outputPath, virtualSize)
	if err != nil {
		return err
	}
	defer out.Close()

	// Read L1 table
	l1 := make([]uint64, l1Size)
	if _, err := f.Seek(int64(hdr.L1TableOffset), io.SeekStart); err != nil {
		out.Close()
		os.Remove(outputPath)
		return fmt.Errorf("seeking to L1 table: %w", err)
	}
	if err := binary.Read(f, binary.BigEndian, l1); err != nil {
		out.Close()
		os.Remove(outputPath)
		return fmt.Errorf("reading L1 table: %w", err)
	}

	clusterBuf := make([]byte, clusterSize)
	compBuf := make([]byte, clusterSize*2) // compressed data can exceed cluster size

	// Iterate L1 → L2 → clusters
	for l1Idx, l1Entry := range l1 {
		// L1 entry: bits [63:0], offset is bits [55:0] with flags in upper bits
		l2Offset := int64(l1Entry & 0x00FFFFFFFFFFFE00) // bits [55:9] shifted, mask flags
		if l2Offset == 0 {
			continue // unallocated L2 table
		}

		// Read L2 table
		l2 := make([]uint64, l2EntriesPerCluster)
		if _, err := f.Seek(l2Offset, io.SeekStart); err != nil {
			continue
		}
		if err := binary.Read(f, binary.BigEndian, l2); err != nil {
			continue
		}

		// Process each cluster in this L2 table
		for l2Idx, l2Entry := range l2 {
			if l2Entry == 0 {
				continue // unallocated cluster
			}

			// Calculate virtual offset for this cluster
			clusterIndex := int64(l1Idx)*l2EntriesPerCluster + int64(l2Idx)
			virtualOffset := clusterIndex * clusterSize
			if virtualOffset >= virtualSize {
				continue
			}

			isCompressed := l2Entry&qcow2L2Compressed != 0

			if isCompressed {
				if err := readCompressedCluster(f, out, l2Entry, hdr.ClusterBits, virtualOffset, clusterSize, compBuf); err != nil {
					// Log but continue — some compressed clusters may be corrupt
					continue
				}
			} else {
				// Standard cluster: bits [55:0] give the host offset (bits [63] and [62] are flags)
				hostOffset := int64(l2Entry & 0x00FFFFFFFFFFFE00)
				if hostOffset == 0 {
					continue
				}

				if _, err := f.Seek(hostOffset, io.SeekStart); err != nil {
					continue
				}
				readSize := clusterSize
				if virtualOffset+readSize > virtualSize {
					readSize = virtualSize - virtualOffset
				}
				n, err := io.ReadFull(f, clusterBuf[:readSize])
				if err != nil && err != io.ErrUnexpectedEOF {
					continue
				}

				if _, err := out.WriteAt(clusterBuf[:n], virtualOffset); err != nil {
					continue
				}
			}
		}
	}

	return out.Sync()
}

// readCompressedCluster reads and decompresses a QCOW2 compressed cluster.
func readCompressedCluster(f *os.File, out *os.File, l2Entry uint64, clusterBits uint32, virtualOffset, clusterSize int64, compBuf []byte) error {
	// For compressed clusters, the L2 entry format is:
	// bits [61:0] = host offset of compressed data
	// The number of additional 512-byte sectors is encoded differently per version,
	// but typically: compressed size comes from the next cluster offset or EOF.

	// Extract the host offset (bits [61:0]) — the number of sectors to shift
	// depends on cluster_bits. For QCOW2 v2:
	// host_offset = entry & ((1 << 62) - 1)
	// The offset uses 62 - (cluster_bits - 8) bits for the offset
	// and (cluster_bits - 8) bits for the additional sectors count.

	nbSectors := int(clusterBits) - 8
	if nbSectors < 0 {
		nbSectors = 0
	}

	hostOffset := int64(l2Entry & ((1 << (62 - nbSectors)) - 1))
	additionalSectors := int((l2Entry >> (62 - uint(nbSectors))) & ((1 << nbSectors) - 1))
	compressedSize := int64(additionalSectors+1) * sectorSize

	// Cap compressed size
	if compressedSize > int64(len(compBuf)) {
		compressedSize = int64(len(compBuf))
	}

	if _, err := f.Seek(hostOffset, io.SeekStart); err != nil {
		return err
	}
	n, err := io.ReadFull(f, compBuf[:compressedSize])
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}

	// Decompress with deflate (raw, no zlib header)
	reader := flate.NewReader(bytes.NewReader(compBuf[:n]))
	decompressed, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		// Try with zlib header (some QCOW2 files use zlib instead of raw deflate)
		return readCompressedClusterZlib(compBuf[:n], out, virtualOffset, clusterSize)
	}

	writeSize := int64(len(decompressed))
	if virtualOffset+writeSize > virtualOffset+clusterSize {
		writeSize = clusterSize
	}
	if _, err := out.WriteAt(decompressed[:writeSize], virtualOffset); err != nil {
		return err
	}
	return nil
}

// readCompressedClusterZlib tries zlib (deflate with header) decompression.
func readCompressedClusterZlib(compData []byte, out *os.File, virtualOffset, clusterSize int64) error {
	// Zlib header: 2 bytes (CMF, FLG) before deflate data
	if len(compData) < 2 {
		return fmt.Errorf("compressed data too short for zlib")
	}

	// Skip 2-byte zlib header and use raw deflate
	reader := flate.NewReader(bytes.NewReader(compData[2:]))
	decompressed, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		return fmt.Errorf("zlib decompression failed: %w", err)
	}

	writeSize := int64(len(decompressed))
	if writeSize > clusterSize {
		writeSize = clusterSize
	}
	if _, err := out.WriteAt(decompressed[:writeSize], virtualOffset); err != nil {
		return err
	}
	return nil
}
