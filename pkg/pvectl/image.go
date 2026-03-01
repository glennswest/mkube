package pvectl

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/dockersave"
)

// ExtractBinary pulls an OCI image and extracts a named binary from the layers.
// Returns the path to a temp file containing the binary.
func ExtractBinary(ctx context.Context, imageRef, binaryName string, log *zap.SugaredLogger) (string, error) {
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	log.Infow("pulling OCI image", "ref", imageRef)
	img, err := crane.Pull(imageRef,
		crane.WithContext(pullCtx),
		crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "amd64"}),
		crane.WithAuthFromKeychain(
			authn.NewMultiKeychain(authn.DefaultKeychain, dockersave.AnonymousKeychain{}),
		),
	)
	if err != nil {
		return "", fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// Flatten all layers into a single rootfs tarball and search for the binary
	log.Infow("extracting binary from image layers", "binary", binaryName)
	reader := mutate.Extract(img)
	defer reader.Close()

	tr := tar.NewReader(reader)

	// Search for the binary in common locations
	searchPaths := []string{
		binaryName,                            // root-level (scratch images)
		"usr/local/bin/" + binaryName,         // standard install path
		"usr/bin/" + binaryName,               // system path
		"bin/" + binaryName,                   // minimal path
		"app/" + binaryName,                   // container convention
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading tar: %w", err)
		}

		// Normalize the name (strip leading ./ or /)
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimPrefix(name, "/")

		// Check if this entry matches any search path
		matched := false
		for _, sp := range searchPaths {
			if name == sp {
				matched = true
				break
			}
		}
		// Also match by basename if it's an executable file
		if !matched && filepath.Base(name) == binaryName && hdr.Typeflag == tar.TypeReg {
			matched = true
		}

		if !matched {
			continue
		}

		// Found the binary — extract to temp file
		tmp, err := os.CreateTemp("", "pvectl-binary-*")
		if err != nil {
			return "", fmt.Errorf("creating temp file: %w", err)
		}

		n, err := io.Copy(tmp, tr)
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("extracting binary: %w", err)
		}
		tmp.Close()

		if err := os.Chmod(tmp.Name(), 0755); err != nil {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("chmod binary: %w", err)
		}

		log.Infow("binary extracted", "name", name, "size", n, "path", tmp.Name())
		return tmp.Name(), nil
	}

	return "", fmt.Errorf("binary %q not found in image %s (searched: %v)", binaryName, imageRef, searchPaths)
}
