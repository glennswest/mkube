package diskimg

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestConvertToRaw_UnsupportedFormat(t *testing.T) {
	err := ConvertToRaw("/tmp/test.xyz", "xyz", "/tmp/out.raw")
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestCreateSparseOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse.raw")

	f, err := createSparseOutput(path, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1024*1024 {
		t.Fatalf("expected size %d, got %d", 1024*1024, info.Size())
	}
}

func TestDetectVMDKMagic(t *testing.T) {
	// Write a file with VMDK magic
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vmdk")
	outPath := filepath.Join(dir, "out.raw")

	// Create a minimal sparse VMDK header
	hdr := sparseExtentHeader{
		MagicNumber:  vmdkSEMagic,
		Version:      1,
		Flags:        0, // uncompressed
		Capacity:     2048,
		GrainSize:    128,
		NumGTEsPerGT: 512,
		GDOffset:     1, // sector 1
		OverHead:     4,
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(f, binary.LittleEndian, &hdr); err != nil {
		f.Close()
		t.Fatal(err)
	}
	// Pad out to a few sectors so it doesn't fail on reads
	f.Seek(4*512, 0)
	f.Write(make([]byte, 512))
	f.Close()

	// This should parse the header correctly (may produce empty output due to no grain data)
	err = ConvertToRaw(path, "vmdk", outPath)
	if err != nil {
		t.Fatal(err)
	}
	// Output should exist with virtual size = 2048 * 512 = 1MB
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2048*512 {
		t.Fatalf("expected size %d, got %d", 2048*512, info.Size())
	}
}

func TestQCOW2Magic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.qcow2")
	outPath := filepath.Join(dir, "out.raw")

	// Create a minimal QCOW2 header
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	hdr := qcow2Header{
		Magic:       qcow2Magic,
		Version:     2,
		ClusterBits: 16, // 64KB clusters
		Size:        1024 * 1024,
		L1Size:      1,
		L1TableOffset: 0x10000, // 64KB offset
	}
	if err := binary.Write(f, binary.BigEndian, &hdr); err != nil {
		f.Close()
		t.Fatal(err)
	}
	// Write an empty L1 table (all zeros = unallocated)
	f.Seek(0x10000, 0)
	f.Write(make([]byte, 8))
	f.Close()

	err = ConvertToRaw(path, "qcow2", outPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1024*1024 {
		t.Fatalf("expected 1MB, got %d", info.Size())
	}
}

func TestQCOW2BadMagic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.qcow2")
	outPath := filepath.Join(dir, "out.raw")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("not a qcow2 file at all!!"))
	f.Close()

	err = ConvertToRaw(path, "qcow2", outPath)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestVHDFixedDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhd")
	outPath := filepath.Join(dir, "out.raw")

	virtualSize := int64(65536) // 64KB virtual disk

	// Create fixed VHD: raw data + 512-byte footer
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	// Write some test data
	testData := bytes.Repeat([]byte{0xAB}, int(virtualSize))
	f.Write(testData)

	// Write VHD footer
	var footer vhdFooter
	copy(footer.Cookie[:], vhdFooterCookie)
	footer.DiskType = vhdDiskFixed
	footer.CurrentSize = uint64(virtualSize)
	footer.DataOffset = 0xFFFFFFFFFFFFFFFF
	if err := binary.Write(f, binary.BigEndian, &footer); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	err = ConvertToRaw(path, "vpc", outPath)
	if err != nil {
		t.Fatal(err)
	}

	// Read output and verify
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != virtualSize {
		t.Fatalf("expected %d bytes, got %d", virtualSize, len(raw))
	}
	if !bytes.Equal(raw, testData) {
		t.Fatal("output data doesn't match input")
	}
}

func TestVMDKStreamOptimized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vmdk")
	outPath := filepath.Join(dir, "out.raw")

	// Create a streamOptimized VMDK with one compressed grain
	grainSizeBytes := int64(128 * 512) // 64KB
	virtualSectors := uint64(256)       // 128KB virtual disk
	virtualSize := int64(virtualSectors) * 512

	// Compress a grain of test data
	testGrain := bytes.Repeat([]byte{0xCD}, int(grainSizeBytes))
	var compBuf bytes.Buffer
	w, _ := flate.NewWriter(&compBuf, flate.DefaultCompression)
	w.Write(testGrain)
	w.Close()
	compData := compBuf.Bytes()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	// Write sparse extent header
	hdr := sparseExtentHeader{
		MagicNumber:  vmdkSEMagic,
		Version:      1,
		Flags:        compressedBit, // streamOptimized
		Capacity:     virtualSectors,
		GrainSize:    128,
		NumGTEsPerGT: 512,
		OverHead:     1, // 1 sector overhead
	}
	if err := binary.Write(f, binary.LittleEndian, &hdr); err != nil {
		f.Close()
		t.Fatal(err)
	}

	// Write grain marker at sector 1 (after header)
	f.Seek(512, 0) // sector 1
	marker := grainMarker{
		LBA:  0, // first grain
		Size: uint32(len(compData)),
	}
	binary.Write(f, binary.LittleEndian, &marker)
	f.Write(compData)

	// Write EOS marker
	eos := grainMarker{LBA: 0, Size: 0}
	// Align to sector
	pos, _ := f.Seek(0, 1)
	remainder := pos % 512
	if remainder != 0 {
		f.Write(make([]byte, 512-remainder))
	}
	binary.Write(f, binary.LittleEndian, &eos)
	binary.Write(f, binary.LittleEndian, &eos) // double EOS
	f.Close()

	err = ConvertToRaw(path, "vmdk", outPath)
	if err != nil {
		t.Fatal(err)
	}

	// Verify output
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != virtualSize {
		t.Fatalf("expected %d bytes, got %d", virtualSize, len(raw))
	}
	// First 64KB should be our test pattern
	if !bytes.Equal(raw[:grainSizeBytes], testGrain) {
		t.Fatal("first grain data doesn't match")
	}
}

func TestVMDKDescriptorFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vmdk")
	outPath := filepath.Join(dir, "out.raw")

	// Create a descriptor-only VMDK pointing to a flat extent
	flatPath := filepath.Join(dir, "test-flat.vmdk")
	flatData := bytes.Repeat([]byte{0xEF}, 1024)
	os.WriteFile(flatPath, flatData, 0o644)

	desc := `# Disk DescriptorFile
version=1
CID=fffffffe
parentCID=ffffffff
createType="monolithicFlat"
# Extent description
RW 2 FLAT "test-flat.vmdk" 0
`
	os.WriteFile(path, []byte(desc), 0o644)

	err := ConvertToRaw(path, "vmdk", outPath)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, flatData) {
		t.Fatal("flat extent data doesn't match")
	}
}
