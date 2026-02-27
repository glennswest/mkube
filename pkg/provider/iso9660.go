package provider

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

// ISO9660 constants
const (
	iso9660SectorSize = 2048
	iso9660PVDSector  = 16 // Primary Volume Descriptor at sector 16
	iso9660PVDOffset  = iso9660PVDSector * iso9660SectorSize
)

// ─── Both-Endian Helpers ─────────────────────────────────────────────────────

// readBothEndian32 reads a both-endian (LE then BE) uint32 from 8 bytes.
func readBothEndian32(buf []byte) uint32 {
	return binary.LittleEndian.Uint32(buf[:4])
}

// writeBothEndian32 writes a uint32 in both-endian format (LE then BE) to 8 bytes.
func writeBothEndian32(buf []byte, val uint32) {
	binary.LittleEndian.PutUint32(buf[:4], val)
	binary.BigEndian.PutUint32(buf[4:8], val)
}

// ─── PVD ─────────────────────────────────────────────────────────────────────

// iso9660PVD represents key fields from the Primary Volume Descriptor.
type iso9660PVD struct {
	VolumeSpaceSize uint32 // total sectors in volume (bytes 80-87)
	RootDirLBA      uint32 // root directory extent LBA
	RootDirSize     uint32 // root directory data length
}

// iso9660ReadPVD reads the Primary Volume Descriptor from an ISO file.
func iso9660ReadPVD(f *os.File) (*iso9660PVD, error) {
	buf := make([]byte, iso9660SectorSize)
	if _, err := f.ReadAt(buf, int64(iso9660PVDOffset)); err != nil {
		return nil, fmt.Errorf("reading PVD: %w", err)
	}

	// Verify PVD type (byte 0 = 1) and magic "CD001" (bytes 1-5)
	if buf[0] != 1 || string(buf[1:6]) != "CD001" {
		return nil, fmt.Errorf("not a valid ISO9660 PVD at sector %d", iso9660PVDSector)
	}

	pvd := &iso9660PVD{
		VolumeSpaceSize: readBothEndian32(buf[80:88]),
	}

	// Root directory record is at PVD offset 156, 34 bytes
	rootRec := buf[156:190]
	pvd.RootDirLBA = readBothEndian32(rootRec[2:10])
	pvd.RootDirSize = readBothEndian32(rootRec[10:18])

	return pvd, nil
}

// iso9660UpdateVolumeSpaceSize updates the Volume Space Size in the PVD.
func iso9660UpdateVolumeSpaceSize(f *os.File, newSectors uint32) error {
	buf := make([]byte, 8)
	writeBothEndian32(buf, newSectors)
	_, err := f.WriteAt(buf, int64(iso9660PVDOffset)+80)
	return err
}

// ─── Directory Records ───────────────────────────────────────────────────────

// iso9660DirRecord represents a parsed directory record.
type iso9660DirRecord struct {
	RecordLen  uint8  // total length of this record
	ExtentLBA  uint32 // LBA of file data
	DataLen    uint32 // length of file data
	Flags      uint8  // bit 1 = directory
	FileIDLen  uint8  // length of file identifier
	FileID     string // raw ISO9660 file identifier (e.g. "GRUB.CFG;1")
	RRName     string // Rock Ridge NM name (e.g. "grub.cfg"), empty if none
	RecordOff  int64  // byte offset of this record in the ISO file
	IsDir      bool
}

// Name returns the best available name (Rock Ridge NM preferred, then ISO ID).
func (r *iso9660DirRecord) Name() string {
	if r.RRName != "" {
		return r.RRName
	}
	// Strip version number (;1) from ISO ID
	name := r.FileID
	if idx := strings.Index(name, ";"); idx >= 0 {
		name = name[:idx]
	}
	return name
}

// iso9660ParseDirRecord parses a single directory record from raw bytes.
// offset is the absolute byte position in the ISO file where this record starts.
func iso9660ParseDirRecord(raw []byte, offset int64) *iso9660DirRecord {
	if len(raw) < 1 || raw[0] == 0 {
		return nil // end of directory entries or padding
	}
	recLen := raw[0]
	if int(recLen) > len(raw) || recLen < 33 {
		return nil
	}

	rec := &iso9660DirRecord{
		RecordLen: recLen,
		ExtentLBA: readBothEndian32(raw[2:10]),
		DataLen:   readBothEndian32(raw[10:18]),
		Flags:     raw[25],
		FileIDLen: raw[32],
		RecordOff: offset,
	}
	rec.IsDir = (rec.Flags & 0x02) != 0

	if rec.FileIDLen > 0 && int(33+rec.FileIDLen) <= int(recLen) {
		rec.FileID = string(raw[33 : 33+rec.FileIDLen])
	}

	// Parse Rock Ridge NM extension from System Use Area
	suaStart := 33 + int(rec.FileIDLen)
	if rec.FileIDLen%2 == 0 {
		suaStart++ // padding byte for even-length file IDs
	}
	if suaStart < int(recLen) {
		rec.RRName = iso9660ParseRockRidgeNM(raw[suaStart:recLen])
	}

	return rec
}

// iso9660ParseRockRidgeNM extracts the filename from Rock Ridge NM entries
// in the System Use Area.
func iso9660ParseRockRidgeNM(sua []byte) string {
	var name strings.Builder
	for len(sua) >= 5 {
		// Look for "NM" signature
		if sua[0] == 'N' && sua[1] == 'M' {
			entryLen := int(sua[2])
			if entryLen < 5 || entryLen > len(sua) {
				break
			}
			// sua[3] = version (1), sua[4] = flags
			flags := sua[4]
			nameData := sua[5:entryLen]
			name.Write(nameData)
			sua = sua[entryLen:]
			if flags&0x01 == 0 { // CONTINUE flag not set
				break
			}
			continue
		}
		// Skip other System Use entries
		if len(sua) < 3 {
			break
		}
		entryLen := int(sua[2])
		if entryLen < 3 || entryLen > len(sua) {
			break
		}
		sua = sua[entryLen:]
	}
	return name.String()
}

// iso9660ReadDirectory reads all directory records from a directory extent.
func iso9660ReadDirectory(f *os.File, lba, size uint32) ([]iso9660DirRecord, error) {
	data := make([]byte, size)
	if _, err := f.ReadAt(data, int64(lba)*iso9660SectorSize); err != nil {
		return nil, fmt.Errorf("reading directory at LBA %d: %w", lba, err)
	}

	baseOffset := int64(lba) * iso9660SectorSize
	var records []iso9660DirRecord
	pos := 0
	for pos < int(size) {
		// Check for sector boundary padding
		sectorRemain := iso9660SectorSize - (pos % iso9660SectorSize)
		if data[pos] == 0 {
			// Skip to next sector
			pos += sectorRemain
			continue
		}

		rec := iso9660ParseDirRecord(data[pos:], baseOffset+int64(pos))
		if rec == nil {
			pos += sectorRemain
			continue
		}

		records = append(records, *rec)
		pos += int(rec.RecordLen)
	}

	return records, nil
}

// ─── Path Resolution ─────────────────────────────────────────────────────────

// iso9660ResolvePath walks the directory tree to find a file by path.
// Path must start with "/" (e.g., "/EFI/BOOT/grub.cfg").
func iso9660ResolvePath(f *os.File, pvd *iso9660PVD, path string) (*iso9660DirRecord, error) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return &iso9660DirRecord{
			ExtentLBA: pvd.RootDirLBA,
			DataLen:   pvd.RootDirSize,
			IsDir:     true,
		}, nil
	}

	parts := strings.Split(path, "/")
	currentLBA := pvd.RootDirLBA
	currentSize := pvd.RootDirSize

	for i, part := range parts {
		records, err := iso9660ReadDirectory(f, currentLBA, currentSize)
		if err != nil {
			return nil, err
		}

		found := false
		for _, rec := range records {
			// Skip "." and ".." entries
			if rec.FileIDLen == 1 && (rec.FileID == "\x00" || rec.FileID == "\x01") {
				continue
			}

			name := rec.Name()
			if strings.EqualFold(name, part) {
				if i == len(parts)-1 {
					// Final component — return this record
					recCopy := rec
					return &recCopy, nil
				}
				if !rec.IsDir {
					return nil, fmt.Errorf("path component %q is not a directory", part)
				}
				currentLBA = rec.ExtentLBA
				currentSize = rec.DataLen
				found = true
				break
			}
		}

		if !found {
			return nil, fmt.Errorf("path component %q not found in directory", part)
		}
	}

	return nil, fmt.Errorf("path %q not found", path)
}

// iso9660ListDirectory lists entries in a directory at the given path.
func iso9660ListDirectory(f *os.File, pvd *iso9660PVD, path string) ([]iso9660DirRecord, error) {
	rec, err := iso9660ResolvePath(f, pvd, path)
	if err != nil {
		return nil, err
	}
	if !rec.IsDir {
		return nil, fmt.Errorf("%q is not a directory", path)
	}

	records, err := iso9660ReadDirectory(f, rec.ExtentLBA, rec.DataLen)
	if err != nil {
		return nil, err
	}

	// Filter out "." and ".." entries
	var result []iso9660DirRecord
	for _, r := range records {
		if r.FileIDLen == 1 && (r.FileID == "\x00" || r.FileID == "\x01") {
			continue
		}
		result = append(result, r)
	}
	return result, nil
}

// ─── File Operations ─────────────────────────────────────────────────────────

// iso9660ReadFile reads the content of a file given its directory record.
func iso9660ReadFile(f *os.File, rec *iso9660DirRecord) ([]byte, error) {
	if rec.IsDir {
		return nil, fmt.Errorf("cannot read directory as file")
	}
	data := make([]byte, rec.DataLen)
	if _, err := f.ReadAt(data, int64(rec.ExtentLBA)*iso9660SectorSize); err != nil {
		return nil, fmt.Errorf("reading file data at LBA %d: %w", rec.ExtentLBA, err)
	}
	return data, nil
}

// iso9660ReplaceFile replaces a file's content by appending new data at the end
// of the ISO and updating the directory record to point to the new location.
// The file must be opened read-write.
func iso9660ReplaceFile(f *os.File, rec *iso9660DirRecord, content []byte) error {
	// Read current PVD to get volume space size
	pvd, err := iso9660ReadPVD(f)
	if err != nil {
		return err
	}

	// Append new content at end of ISO (sector-aligned)
	newLBA := pvd.VolumeSpaceSize
	newOffset := int64(newLBA) * iso9660SectorSize

	// Write content (pad to sector boundary)
	paddedLen := (len(content) + iso9660SectorSize - 1) / iso9660SectorSize * iso9660SectorSize
	padded := make([]byte, paddedLen)
	copy(padded, content)

	if _, err := f.WriteAt(padded, newOffset); err != nil {
		return fmt.Errorf("writing new file data: %w", err)
	}

	// Update directory record: extent LBA + data length
	lbaBytes := make([]byte, 8)
	writeBothEndian32(lbaBytes, newLBA)
	if _, err := f.WriteAt(lbaBytes, rec.RecordOff+2); err != nil {
		return fmt.Errorf("updating directory record LBA: %w", err)
	}

	sizeBytes := make([]byte, 8)
	writeBothEndian32(sizeBytes, uint32(len(content)))
	if _, err := f.WriteAt(sizeBytes, rec.RecordOff+10); err != nil {
		return fmt.Errorf("updating directory record size: %w", err)
	}

	// Update PVD volume space size
	newSectors := newLBA + uint32(paddedLen/iso9660SectorSize)
	if err := iso9660UpdateVolumeSpaceSize(f, newSectors); err != nil {
		return fmt.Errorf("updating volume space size: %w", err)
	}

	return nil
}

// iso9660DeleteFile zeroes out a directory record, effectively hiding the file.
func iso9660DeleteFile(f *os.File, rec *iso9660DirRecord) error {
	// Zero out the record length byte — this makes the entry invisible to readers
	zero := []byte{0}
	if _, err := f.WriteAt(zero, rec.RecordOff); err != nil {
		return fmt.Errorf("zeroing directory record: %w", err)
	}
	return nil
}

// iso9660AddFile adds a new file to a directory by appending data at the end
// of the ISO and inserting a directory record in the parent directory's free space.
func iso9660AddFile(f *os.File, pvd *iso9660PVD, parentPath, fileName string, content []byte) error {
	// Resolve parent directory
	parentRec, err := iso9660ResolvePath(f, pvd, parentPath)
	if err != nil {
		return fmt.Errorf("resolving parent %q: %w", parentPath, err)
	}
	if !parentRec.IsDir {
		return fmt.Errorf("parent %q is not a directory", parentPath)
	}

	// Read current PVD for volume space size
	pvd, err = iso9660ReadPVD(f)
	if err != nil {
		return err
	}

	// Append file content at end of ISO
	newLBA := pvd.VolumeSpaceSize
	newOffset := int64(newLBA) * iso9660SectorSize

	paddedLen := (len(content) + iso9660SectorSize - 1) / iso9660SectorSize * iso9660SectorSize
	padded := make([]byte, paddedLen)
	copy(padded, content)

	if _, err := f.WriteAt(padded, newOffset); err != nil {
		return fmt.Errorf("writing new file data: %w", err)
	}

	// Build a directory record for the new file
	fileID := strings.ToUpper(fileName) + ";1"
	fileIDLen := len(fileID)
	recLen := 33 + fileIDLen
	if recLen%2 != 0 {
		recLen++ // padding byte
	}

	// Add Rock Ridge NM entry if filename differs from ISO ID
	var nmEntry []byte
	if fileName != strings.ToUpper(fileName)+";1" {
		// NM entry: signature(2) + length(1) + version(1) + flags(1) + name
		nmLen := 5 + len(fileName)
		nmEntry = make([]byte, nmLen)
		nmEntry[0] = 'N'
		nmEntry[1] = 'M'
		nmEntry[2] = byte(nmLen)
		nmEntry[3] = 1 // version
		nmEntry[4] = 0 // flags (no CONTINUE)
		copy(nmEntry[5:], fileName)
		recLen += nmLen
	}
	if recLen%2 != 0 {
		recLen++
	}

	rec := make([]byte, recLen)
	rec[0] = byte(recLen) // record length
	writeBothEndian32(rec[2:10], newLBA)
	writeBothEndian32(rec[10:18], uint32(len(content)))
	// Recording date (7 bytes at offset 18) — leave zeroed
	rec[25] = 0 // flags: regular file
	// File unit size (26), interleave gap (27), volume seq (28-31) — leave defaults
	writeBothEndian32(rec[28:36], 1) // volume sequence number = 1 (both-endian at 28)
	// Actually volume sequence is at bytes 28-31 (4 bytes both-endian)
	// Let me fix: volume sequence number at offset 28, 4 bytes both-endian
	rec[28] = 1 // LE low byte
	rec[29] = 0
	rec[30] = 0
	rec[31] = 1 // BE high byte
	rec[32] = byte(fileIDLen) // file identifier length
	copy(rec[33:33+fileIDLen], fileID)

	// Add NM entry after file ID + padding
	if nmEntry != nil {
		nmStart := 33 + fileIDLen
		if fileIDLen%2 == 0 {
			nmStart++ // padding byte
		}
		copy(rec[nmStart:], nmEntry)
	}

	// Find free space in parent directory
	dirData := make([]byte, parentRec.DataLen)
	dirOffset := int64(parentRec.ExtentLBA) * iso9660SectorSize
	if _, err := f.ReadAt(dirData, dirOffset); err != nil {
		return fmt.Errorf("reading parent directory: %w", err)
	}

	// Scan for free space (zero bytes at end of a sector)
	insertPos := -1
	pos := 0
	for pos < len(dirData) {
		if dirData[pos] == 0 {
			// Found free space — check if enough room in this sector
			sectorEnd := ((pos / iso9660SectorSize) + 1) * iso9660SectorSize
			freeSpace := sectorEnd - pos
			if freeSpace >= recLen {
				insertPos = pos
				break
			}
			// Skip to next sector
			pos = sectorEnd
			continue
		}
		pos += int(dirData[pos]) // skip existing record
	}

	if insertPos < 0 {
		return fmt.Errorf("no free space in parent directory for new entry (need %d bytes)", recLen)
	}

	// Write directory record into parent directory
	writeOffset := dirOffset + int64(insertPos)
	if _, err := f.WriteAt(rec, writeOffset); err != nil {
		return fmt.Errorf("writing directory record: %w", err)
	}

	// Update PVD volume space size
	newSectors := newLBA + uint32(paddedLen/iso9660SectorSize)
	if err := iso9660UpdateVolumeSpaceSize(f, newSectors); err != nil {
		return fmt.Errorf("updating volume space size: %w", err)
	}

	return nil
}

// ─── Patch Operations ────────────────────────────────────────────────────────

// iso9660PatchOp describes a single patch operation on an ISO.
type iso9660PatchOp struct {
	Op      string `json:"op"`      // "replace", "delete", "add"
	Path    string `json:"path"`    // file path in ISO (e.g. "/EFI/BOOT/grub.cfg")
	Content []byte `json:"content"` // file content (for "replace" and "add")
}

// iso9660ApplyPatches applies a sequence of patch operations to an ISO file.
func iso9660ApplyPatches(isoPath string, ops []iso9660PatchOp) error {
	f, err := os.OpenFile(isoPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening ISO: %w", err)
	}
	defer f.Close()

	for i, op := range ops {
		pvd, err := iso9660ReadPVD(f)
		if err != nil {
			return fmt.Errorf("op %d: reading PVD: %w", i, err)
		}

		switch op.Op {
		case "replace":
			rec, err := iso9660ResolvePath(f, pvd, op.Path)
			if err != nil {
				return fmt.Errorf("op %d replace %q: %w", i, op.Path, err)
			}
			if err := iso9660ReplaceFile(f, rec, op.Content); err != nil {
				return fmt.Errorf("op %d replace %q: %w", i, op.Path, err)
			}

		case "delete":
			rec, err := iso9660ResolvePath(f, pvd, op.Path)
			if err != nil {
				return fmt.Errorf("op %d delete %q: %w", i, op.Path, err)
			}
			if err := iso9660DeleteFile(f, rec); err != nil {
				return fmt.Errorf("op %d delete %q: %w", i, op.Path, err)
			}

		case "add":
			parts := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
			if len(parts) < 1 {
				return fmt.Errorf("op %d add: invalid path %q", i, op.Path)
			}
			parentPath := "/" + strings.Join(parts[:len(parts)-1], "/")
			fileName := parts[len(parts)-1]
			if err := iso9660AddFile(f, pvd, parentPath, fileName, op.Content); err != nil {
				return fmt.Errorf("op %d add %q: %w", i, op.Path, err)
			}

		default:
			return fmt.Errorf("op %d: unknown operation %q", i, op.Op)
		}
	}

	return f.Sync()
}

// ─── High-Level Read ─────────────────────────────────────────────────────────

// iso9660ReadFileByPath reads a file from an ISO given its path.
func iso9660ReadFileByPath(isoPath, filePath string) ([]byte, error) {
	f, err := os.Open(isoPath)
	if err != nil {
		return nil, fmt.Errorf("opening ISO: %w", err)
	}
	defer f.Close()

	pvd, err := iso9660ReadPVD(f)
	if err != nil {
		return nil, err
	}

	rec, err := iso9660ResolvePath(f, pvd, filePath)
	if err != nil {
		return nil, err
	}

	return iso9660ReadFile(f, rec)
}

// iso9660ListDirectoryByPath lists directory entries in an ISO.
func iso9660ListDirectoryByPath(isoPath, dirPath string) ([]iso9660DirRecord, error) {
	f, err := os.Open(isoPath)
	if err != nil {
		return nil, fmt.Errorf("opening ISO: %w", err)
	}
	defer f.Close()

	pvd, err := iso9660ReadPVD(f)
	if err != nil {
		return nil, err
	}

	return iso9660ListDirectory(f, pvd, dirPath)
}

// iso9660CopyISO copies an ISO file using streaming io.Copy (no full buffering).
func iso9660CopyISO(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening source ISO: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("creating destination ISO: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(dstPath)
		return fmt.Errorf("copying ISO: %w", err)
	}

	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(dstPath)
		return fmt.Errorf("syncing destination ISO: %w", err)
	}

	return dst.Close()
}

// ─── Derive Request Types ────────────────────────────────────────────────────

// iso9660DeriveRequest is the JSON body for POST .../derive.
type iso9660DeriveRequest struct {
	Name        string           `json:"name"`        // new ISCSICdrom name
	Description string           `json:"description"` // human-readable description
	Operations  []iso9660PatchOp `json:"operations"`  // patch operations to apply
}

// iso9660DirEntry is a simplified directory entry for JSON responses.
type iso9660DirEntry struct {
	Name  string `json:"name"`
	Size  uint32 `json:"size"`
	IsDir bool   `json:"isDir"`
}

