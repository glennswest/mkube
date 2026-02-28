package proxmox

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/dockersave"
)

// TemplateConverter converts OCI images to Proxmox LXC rootfs templates.
// Proxmox expects a plain rootfs tarball (.tar.gz), not docker-save format.
type TemplateConverter struct {
	log *zap.SugaredLogger
}

// NewTemplateConverter creates a new OCI→LXC template converter.
func NewTemplateConverter(log *zap.SugaredLogger) *TemplateConverter {
	return &TemplateConverter{log: log}
}

// ConvertToTemplate pulls an OCI image and converts it to a gzipped rootfs
// tarball suitable for Proxmox vztmpl upload. Returns the template data
// and the suggested filename.
func (tc *TemplateConverter) ConvertToTemplate(ctx context.Context, imageRef string) (io.Reader, string, error) {
	pullCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	opts := []crane.Option{
		crane.WithContext(pullCtx),
		crane.WithPlatform(&v1.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		}),
		crane.WithAuthFromKeychain(
			authn.NewMultiKeychain(authn.DefaultKeychain, dockersave.AnonymousKeychain{}),
		),
	}

	tc.log.Infow("pulling OCI image for LXC template", "ref", imageRef)
	img, err := crane.Pull(imageRef, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// Flatten OCI layers into a single rootfs tarball
	tc.log.Infow("flattening OCI layers to rootfs", "ref", imageRef)
	rootfsReader := mutate.Extract(img)
	defer rootfsReader.Close()

	// Gzip compress the rootfs tarball
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)

	if _, err := io.Copy(gzWriter, rootfsReader); err != nil {
		return nil, "", fmt.Errorf("compressing rootfs for %s: %w", imageRef, err)
	}
	if err := gzWriter.Close(); err != nil {
		return nil, "", fmt.Errorf("finalizing gzip for %s: %w", imageRef, err)
	}

	filename := dockersave.SanitizeImageRef(imageRef) + ".tar.gz"
	tc.log.Infow("template conversion complete", "ref", imageRef, "filename", filename, "size", buf.Len())

	return bytes.NewReader(buf.Bytes()), filename, nil
}
