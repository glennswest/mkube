package diskimg

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// VMDK sparse header magic: "KDMV" (little-endian for VMDK)
const (
	vmdkMagic     = 0x564D444B // "VMDK" in big endian → 0x4B444D56 LE, but actually stored as KDMV
	vmdkSEMagic   = 0x564D444B // SparseExtentHeader magic
	grainSize     = 128        // sectors (default; 64KB grains = 128 * 512)
	sectorSize    = 512
	grainBytes    = grainSize * sectorSize // 65536 bytes per grain
	compressedBit = 1 << 16               // flags bit for compressed grains
)

// sparseExtentHeader is the VMDK sparse extent header (on-disk format).
type sparseExtentHeader struct {
	MagicNumber        uint32 // 0x564D444B ("VMDK")
	Version            uint32 // 1, 2, or 3
	Flags              uint32
	Capacity           uint64 // virtual size in sectors
	GrainSize          uint64 // grain size in sectors
	DescriptorOffset   uint64 // offset of embedded descriptor (sectors)
	DescriptorSize     uint64 // size of embedded descriptor (sectors)
	NumGTEsPerGT       uint32 // grain table entries per grain table (usually 512)
	RGDOffset          uint64 // redundant grain directory offset (sectors)
	GDOffset           uint64 // grain directory offset (sectors)
	OverHead           uint64 // metadata overhead in sectors
	UncleanShutdown    byte
	SingleEndLineChar  byte
	NonEndLineChar     byte
	DoubleEndLineChar1 byte
	DoubleEndLineChar2 byte
	CompressAlgorithm  uint16
	_                  [433]byte // padding to 512 bytes
}

// grainMarker is the header for compressed grains in streamOptimized VMDKs.
type grainMarker struct {
	LBA  uint64 // logical byte address / sectorSize → grain number
	Size uint32 // compressed data size following this header
}

// convertVMDK converts a VMDK disk image to raw format.
// Supports: monolithicSparse, streamOptimized, monolithicFlat (descriptor).
func convertVMDK(inputPath, outputPath string) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("opening vmdk: %w", err)
	}
	defer f.Close()

	// Check magic number first (4 bytes) to determine if binary or text descriptor
	var magic uint32
	if err := binary.Read(f, binary.LittleEndian, &magic); err != nil {
		return fmt.Errorf("reading vmdk magic: %w", err)
	}

	if magic != vmdkSEMagic {
		// Not a sparse extent — check if this is a text descriptor VMDK
		return convertVMDKDescriptor(inputPath, outputPath, f)
	}

	// Rewind and read full sparse extent header
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to start: %w", err)
	}
	var hdr sparseExtentHeader
	if err := binary.Read(f, binary.LittleEndian, &hdr); err != nil {
		return fmt.Errorf("reading vmdk header: %w", err)
	}

	virtualSize := int64(hdr.Capacity) * sectorSize
	grainSz := int64(hdr.GrainSize) * sectorSize
	isCompressed := hdr.Flags&compressedBit != 0

	if isCompressed {
		return convertVMDKStreamOptimized(f, outputPath, virtualSize, grainSz, &hdr)
	}

	return convertVMDKSparse(f, outputPath, virtualSize, grainSz, &hdr)
}

// convertVMDKStreamOptimized handles streamOptimized VMDKs (compressed grains with markers).
func convertVMDKStreamOptimized(f *os.File, outputPath string, virtualSize, grainSz int64, hdr *sparseExtentHeader) error {
	out, err := createSparseOutput(outputPath, virtualSize)
	if err != nil {
		return err
	}
	defer out.Close()

	// For streamOptimized, grains appear sequentially after the header.
	// Each grain has a grainMarker: {LBA uint64, Size uint32} followed by deflate-compressed data.
	// A marker with LBA=0 and Size=0 is an EOS marker (end of stream).
	// Metadata markers (GD, GT) may also appear inline.

	// Seek past the overhead area to find grain data
	dataStart := int64(hdr.OverHead) * sectorSize
	if _, err := f.Seek(dataStart, io.SeekStart); err != nil {
		// Fall back to scanning from beginning of the file right after header
		if _, err := f.Seek(sectorSize, io.SeekStart); err != nil {
			out.Close()
			os.Remove(outputPath)
			return fmt.Errorf("seeking to data start: %w", err)
		}
	}

	buf := make([]byte, 4*1024*1024) // 4MB read buffer

	for {
		// Read grain marker (12 bytes)
		var marker grainMarker
		if err := binary.Read(f, binary.LittleEndian, &marker); err != nil {
			if err == io.EOF {
				break
			}
			out.Close()
			os.Remove(outputPath)
			return fmt.Errorf("reading grain marker: %w", err)
		}

		// EOS marker
		if marker.LBA == 0 && marker.Size == 0 {
			// Could be EOS or metadata marker — check what follows
			// Read the next few bytes to see if there's another marker
			pos, _ := f.Seek(0, io.SeekCurrent)
			var nextMarker grainMarker
			err := binary.Read(f, binary.LittleEndian, &nextMarker)
			if err != nil {
				break // EOF — we're done
			}
			// If next marker also has size 0, this is likely metadata or EOS
			if nextMarker.Size == 0 {
				// Skip ahead — could be footer
				break
			}
			// Restore position and use nextMarker
			f.Seek(pos, io.SeekStart)
			continue
		}

		if marker.Size == 0 {
			continue // skip empty markers
		}

		// Read compressed grain data
		compSize := int(marker.Size)
		if compSize > len(buf) {
			buf = make([]byte, compSize)
		}
		if _, err := io.ReadFull(f, buf[:compSize]); err != nil {
			out.Close()
			os.Remove(outputPath)
			return fmt.Errorf("reading compressed grain at LBA %d: %w", marker.LBA, err)
		}

		// Decompress with deflate (RFC 1951)
		reader := flate.NewReader(bytes.NewReader(buf[:compSize]))
		decompressed, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			out.Close()
			os.Remove(outputPath)
			return fmt.Errorf("decompressing grain at LBA %d: %w", marker.LBA, err)
		}

		// Write decompressed data at the correct offset
		byteOffset := int64(marker.LBA) * sectorSize
		if byteOffset >= virtualSize {
			continue // skip out-of-bounds grains
		}
		if _, err := out.WriteAt(decompressed, byteOffset); err != nil {
			out.Close()
			os.Remove(outputPath)
			return fmt.Errorf("writing grain at offset %d: %w", byteOffset, err)
		}

		// Align to next sector boundary
		pos, _ := f.Seek(0, io.SeekCurrent)
		remainder := pos % sectorSize
		if remainder != 0 {
			f.Seek(sectorSize-remainder, io.SeekCurrent)
		}
	}

	return out.Sync()
}

// convertVMDKSparse handles monolithicSparse VMDKs (uncompressed, grain directory + tables).
func convertVMDKSparse(f *os.File, outputPath string, virtualSize, grainSz int64, hdr *sparseExtentHeader) error {
	out, err := createSparseOutput(outputPath, virtualSize)
	if err != nil {
		return err
	}
	defer out.Close()

	numGTEsPerGT := int64(hdr.NumGTEsPerGT)
	if numGTEsPerGT == 0 {
		numGTEsPerGT = 512 // default
	}

	gdOffset := int64(hdr.GDOffset) * sectorSize

	// Calculate number of grain directory entries
	totalGrains := (int64(hdr.Capacity) + int64(hdr.GrainSize) - 1) / int64(hdr.GrainSize)
	numGDEntries := (totalGrains + numGTEsPerGT - 1) / numGTEsPerGT

	// Read grain directory
	gd := make([]uint32, numGDEntries)
	if _, err := f.Seek(gdOffset, io.SeekStart); err != nil {
		out.Close()
		os.Remove(outputPath)
		return fmt.Errorf("seeking to grain directory: %w", err)
	}
	if err := binary.Read(f, binary.LittleEndian, gd); err != nil {
		out.Close()
		os.Remove(outputPath)
		return fmt.Errorf("reading grain directory: %w", err)
	}

	grainBuf := make([]byte, grainSz)

	// Iterate grain directory → grain tables → grains
	for gdIdx, gtSector := range gd {
		if gtSector == 0 {
			continue // unallocated grain table
		}

		// Read grain table
		gt := make([]uint32, numGTEsPerGT)
		gtOffset := int64(gtSector) * sectorSize
		if _, err := f.Seek(gtOffset, io.SeekStart); err != nil {
			continue
		}
		if err := binary.Read(f, binary.LittleEndian, gt); err != nil {
			continue
		}

		// Process each grain in this table
		for gtIdx, grainSector := range gt {
			if grainSector == 0 {
				continue // unallocated grain
			}

			// Read the grain data
			grainOff := int64(grainSector) * sectorSize
			if _, err := f.Seek(grainOff, io.SeekStart); err != nil {
				continue
			}
			n, err := io.ReadFull(f, grainBuf)
			if err != nil && err != io.ErrUnexpectedEOF {
				continue
			}

			// Calculate output offset: (gdIdx * numGTEsPerGT + gtIdx) * grainSize
			grainIndex := int64(gdIdx)*numGTEsPerGT + int64(gtIdx)
			outOffset := grainIndex * grainSz
			if outOffset >= virtualSize {
				continue
			}

			// Don't write past virtual size
			writeLen := int64(n)
			if outOffset+writeLen > virtualSize {
				writeLen = virtualSize - outOffset
			}

			if _, err := out.WriteAt(grainBuf[:writeLen], outOffset); err != nil {
				continue
			}
		}
	}

	return out.Sync()
}

// convertVMDKDescriptor handles VMDKs that start with a text descriptor
// (monolithicFlat or other descriptor-based formats).
func convertVMDKDescriptor(inputPath, outputPath string, f *os.File) error {
	// Read the beginning to check for descriptor
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to start: %w", err)
	}

	descBuf := make([]byte, 4096)
	n, err := f.Read(descBuf)
	if err != nil {
		return fmt.Errorf("reading descriptor: %w", err)
	}
	desc := string(descBuf[:n])

	// Check if this is a descriptor file
	if !strings.Contains(desc, "createType") {
		return fmt.Errorf("unsupported VMDK format: no sparse header or descriptor found")
	}

	// Parse createType
	createType := extractDescValue(desc, "createType")

	// For monolithicFlat, the data extent is in a separate file
	if createType == "monolithicFlat" {
		// Find the extent description line
		extentFile := extractExtentFile(desc, inputPath)
		if extentFile == "" {
			return fmt.Errorf("monolithicFlat VMDK: could not find extent file reference")
		}
		// Simply copy the flat extent (it's already raw)
		src, err := os.Open(extentFile)
		if err != nil {
			return fmt.Errorf("opening flat extent %s: %w", extentFile, err)
		}
		defer src.Close()
		dst, err := os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("creating output: %w", err)
		}
		defer dst.Close()
		if _, err := io.Copy(dst, src); err != nil {
			os.Remove(outputPath)
			return fmt.Errorf("copying flat extent: %w", err)
		}
		return dst.Sync()
	}

	return fmt.Errorf("unsupported VMDK createType: %s", createType)
}

// extractDescValue extracts a value from a VMDK descriptor.
func extractDescValue(desc, key string) string {
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key) {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				val = strings.Trim(val, "\"")
				return val
			}
		}
	}
	return ""
}

// extractExtentFile parses the extent description and resolves the file path.
func extractExtentFile(desc, vmdkPath string) string {
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		// Extent lines look like: RW 41943040 FLAT "disk-flat.vmdk" 0
		if strings.Contains(line, "FLAT") || strings.Contains(line, "SPARSE") {
			// Extract the quoted filename
			start := strings.Index(line, "\"")
			if start < 0 {
				continue
			}
			end := strings.Index(line[start+1:], "\"")
			if end < 0 {
				continue
			}
			filename := line[start+1 : start+1+end]
			// Resolve relative to the VMDK descriptor's directory
			dir := vmdkPath
			if idx := strings.LastIndex(dir, "/"); idx >= 0 {
				dir = dir[:idx]
			}
			return dir + "/" + filename
		}
	}
	return ""
}

// extractDescInt extracts an integer value from a VMDK descriptor.
func extractDescInt(desc, key string) int64 {
	val := extractDescValue(desc, key)
	if val == "" {
		return 0
	}
	n, _ := strconv.ParseInt(val, 10, 64)
	return n
}
