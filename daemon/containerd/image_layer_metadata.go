package containerd

import (
	"context"

	imagetype "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// ImageLayerMetadata is not implemented when using the containerd
// image-store, which does not keep the graphdriver distribution metadata
// that this export is based on.
func (i *ImageService) ImageLayerMetadata(_ context.Context, _ string, _ *ocispec.Platform) ([]imagetype.LayerMetadata, error) {
	return nil, errdefs.NotImplemented(errors.New("image layer metadata export is not supported with the containerd image store"))
}
