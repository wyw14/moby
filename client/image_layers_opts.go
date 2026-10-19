package client

import (
	"github.com/moby/moby/api/types/image"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageLayersResult is the result of an ImageLayers operation.
type ImageLayersResult struct {
	Items []image.LayerMetadata
}

// ImageLayersOption is a functional option for the ImageLayers operation.
type ImageLayersOption interface {
	Apply(*imageLayersOpts) error
}

type imageLayersOptionFunc func(*imageLayersOpts) error

func (f imageLayersOptionFunc) Apply(o *imageLayersOpts) error { return f(o) }

type imageLayersOpts struct {
	platform *ocispec.Platform
}

// ImageLayersWithPlatform selects the image variant whose layer metadata
// is exported. If omitted, the daemon's default platform is used.
func ImageLayersWithPlatform(platform ocispec.Platform) ImageLayersOption {
	return imageLayersOptionFunc(func(o *imageLayersOpts) error {
		o.platform = &platform
		return nil
	})
}
