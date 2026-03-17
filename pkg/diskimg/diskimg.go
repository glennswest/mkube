// Package diskimg provides pure Go converters for virtual disk image formats
// (VMDK, QCOW2, VHD) to raw disk images. This replaces qemu-img for scratch
// containers where external tools are unavailable.
package diskimg

import (
	"fmt"
	"io"
	"os"
)

// ConvertToRaw converts a virtual disk image to raw format.
// Supported input formats: "vmdk", "qcow2", "vpc" (VHD).
// outputPath is the destination raw file (created or overwritten).
func ConvertToRaw(inputPath, format, outputPath string) error {
	switch format {
	case "vmdk":
		return convertVMDK(inputPath, outputPath)
	case "qcow2":
		return convertQCOW2(inputPath, outputPath)
	case "vpc", "vhd":
		return convertVHD(inputPath, outputPath)
	default:
		return fmt.Errorf("unsupported disk format: %s", format)
	}
}

// copyAtOffset writes data from reader to the output file at the given byte offset.
func copyAtOffset(dst *os.File, offset int64, src io.Reader, size int64) error {
	if _, err := dst.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to offset %d: %w", offset, err)
	}
	if size > 0 {
		if _, err := io.CopyN(dst, src, size); err != nil {
			return fmt.Errorf("writing %d bytes at offset %d: %w", size, offset, err)
		}
	} else {
		if _, err := io.Copy(dst, src); err != nil {
			return fmt.Errorf("writing at offset %d: %w", offset, err)
		}
	}
	return nil
}

// createSparseOutput creates the output raw file with the given virtual size.
// The file is sparse (only allocated regions consume disk space).
func createSparseOutput(path string, virtualSize int64) (*os.File, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	// Seek to end to set file size (sparse)
	if _, err := f.Seek(virtualSize-1, io.SeekStart); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if _, err := f.Write([]byte{0}); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return f, nil
}
