package diskimg

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// VHD constants
const (
	vhdFooterCookie = "conectix"
	vhdDiskFixed    = 2
	vhdDiskDynamic  = 3
	vhdDiskDiff     = 4
	vhdFooterSize   = 512
	vhdDynHdrSize   = 1024
)

// vhdFooter is the VHD footer structure (big-endian, at end of file for dynamic/diff, also at start).
type vhdFooter struct {
	Cookie             [8]byte
	Features           uint32
	FileFormatVersion  uint32
	DataOffset         uint64 // offset to dynamic header (0xFFFFFFFF for fixed)
	TimeStamp          uint32
	CreatorApplication [4]byte
	CreatorVersion     uint32
	CreatorHostOS      uint32
	OriginalSize       uint64
	CurrentSize        uint64 // virtual disk size
	DiskGeometry       uint32
	DiskType           uint32
	Checksum           uint32
	UniqueID           [16]byte
	SavedState         byte
	_                  [427]byte
}

// vhdDynamicHeader is the VHD dynamic disk header.
type vhdDynamicHeader struct {
	Cookie          [8]byte
	DataOffset      uint64
	TableOffset     uint64 // BAT offset
	HeaderVersion   uint32
	MaxTableEntries uint32
	BlockSize       uint32 // default 2MB
	Checksum        uint32
	ParentUniqueID  [16]byte
	ParentTimeStamp uint32
	_               [4]byte
	ParentUnicode   [512]byte
	ParentLocator   [8 * 24]byte // 8 parent locator entries
	_               [256]byte
}

// convertVHD converts a VHD disk image to raw format.
func convertVHD(inputPath, outputPath string) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("opening vhd: %w", err)
	}
	defer f.Close()

	// Read footer from beginning (copy at start for dynamic/diff, only footer for fixed)
	var footer vhdFooter
	if err := binary.Read(f, binary.BigEndian, &footer); err != nil {
		return fmt.Errorf("reading vhd footer: %w", err)
	}

	if string(footer.Cookie[:]) != vhdFooterCookie {
		// Try reading footer from end of file
		info, err := f.Stat()
		if err != nil {
			return fmt.Errorf("stat vhd: %w", err)
		}
		if _, err := f.Seek(info.Size()-vhdFooterSize, io.SeekStart); err != nil {
			return fmt.Errorf("seeking to vhd footer: %w", err)
		}
		if err := binary.Read(f, binary.BigEndian, &footer); err != nil {
			return fmt.Errorf("reading vhd footer (end): %w", err)
		}
		if string(footer.Cookie[:]) != vhdFooterCookie {
			return fmt.Errorf("not a VHD file: cookie mismatch")
		}
	}

	virtualSize := int64(footer.CurrentSize)

	switch footer.DiskType {
	case vhdDiskFixed:
		return convertVHDFixed(f, outputPath, virtualSize)
	case vhdDiskDynamic:
		return convertVHDDynamic(f, outputPath, virtualSize, &footer)
	case vhdDiskDiff:
		return fmt.Errorf("VHD differencing disks not supported — merge chain first")
	default:
		return fmt.Errorf("unsupported VHD disk type: %d", footer.DiskType)
	}
}

// convertVHDFixed converts a fixed-size VHD (raw data + 512-byte footer).
func convertVHDFixed(f *os.File, outputPath string, virtualSize int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to start: %w", err)
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Fixed VHD = raw data followed by 512-byte footer. Just copy virtualSize bytes.
	if _, err := io.CopyN(out, f, virtualSize); err != nil {
		out.Close()
		os.Remove(outputPath)
		return fmt.Errorf("copying fixed VHD data: %w", err)
	}

	return out.Sync()
}

// convertVHDDynamic converts a dynamic VHD using the Block Allocation Table (BAT).
func convertVHDDynamic(f *os.File, outputPath string, virtualSize int64, footer *vhdFooter) error {
	// Read dynamic disk header
	if _, err := f.Seek(int64(footer.DataOffset), io.SeekStart); err != nil {
		return fmt.Errorf("seeking to dynamic header: %w", err)
	}

	var dynHdr vhdDynamicHeader
	if err := binary.Read(f, binary.BigEndian, &dynHdr); err != nil {
		return fmt.Errorf("reading dynamic header: %w", err)
	}

	blockSize := int64(dynHdr.BlockSize)
	if blockSize == 0 {
		blockSize = 2 * 1024 * 1024 // default 2MB
	}
	maxTableEntries := int(dynHdr.MaxTableEntries)

	// Each block has a sector bitmap before the data.
	// Bitmap size = ceil(blockSize / sectorSize / 8) rounded up to sector boundary
	sectorsPerBlock := blockSize / sectorSize
	bitmapBytes := (sectorsPerBlock + 7) / 8
	bitmapSectors := (bitmapBytes + sectorSize - 1) / sectorSize
	bitmapSize := bitmapSectors * sectorSize

	// Read BAT
	bat := make([]uint32, maxTableEntries)
	if _, err := f.Seek(int64(dynHdr.TableOffset), io.SeekStart); err != nil {
		return fmt.Errorf("seeking to BAT: %w", err)
	}
	if err := binary.Read(f, binary.BigEndian, bat); err != nil {
		return fmt.Errorf("reading BAT: %w", err)
	}

	out, err := createSparseOutput(outputPath, virtualSize)
	if err != nil {
		return err
	}
	defer out.Close()

	blockBuf := make([]byte, blockSize)

	for i, sectorAddr := range bat {
		if sectorAddr == 0xFFFFFFFF {
			continue // unallocated block
		}

		// Block data starts after the bitmap
		dataOffset := int64(sectorAddr)*sectorSize + bitmapSize

		if _, err := f.Seek(dataOffset, io.SeekStart); err != nil {
			continue
		}

		readSize := blockSize
		virtualOff := int64(i) * blockSize
		if virtualOff+readSize > virtualSize {
			readSize = virtualSize - virtualOff
		}
		if readSize <= 0 {
			continue
		}

		n, err := io.ReadFull(f, blockBuf[:readSize])
		if err != nil && err != io.ErrUnexpectedEOF {
			continue
		}

		if _, err := out.WriteAt(blockBuf[:n], virtualOff); err != nil {
			continue
		}
	}

	return out.Sync()
}
