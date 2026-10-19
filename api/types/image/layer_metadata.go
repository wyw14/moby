package image

import "time"

// LayerMetadata describes the locally stored metadata of a single image
// layer, as returned by the ImageLayers API operation.
//
// Entries are merged by compressed Digest: a layer blob referenced from
// multiple repositories (or through multiple metadata entries) is reported
// once with every known source repository preserved. The export only reads
// local metadata; it never pulls missing layers or modifies local images.
type LayerMetadata struct {
	// Digest is the compressed (distribution) digest of the layer blob.
	//
	// This field is empty for layers that were never pulled from a
	// registry, for example layers built locally.
	Digest string `json:"Digest,omitempty"`

	// DiffID is the digest of the uncompressed layer tar stream.
	DiffID string `json:"DiffID"`

	// ChainID is the content hash of the layer chain from the root to
	// (and including) this layer.
	ChainID string `json:"ChainID"`

	// Parent is the ChainID of the parent layer. It is empty for the
	// first (base) layer of an image.
	Parent string `json:"Parent,omitempty"`

	// MediaType is the media type of the compressed layer blob as
	// recorded when the layer was pulled or loaded. It is empty when the
	// local image store does not record it.
	MediaType string `json:"MediaType,omitempty"`

	// Created is the layer creation time taken from the image history.
	// It is nil when the image history has no creation time for the layer.
	Created *time.Time `json:"Created,omitempty"`

	// SourceRepositories is the sorted, de-duplicated list of all
	// repositories from which the layer blob is known to have been
	// pulled. It may be empty for layers built locally.
	SourceRepositories []string `json:"SourceRepositories,omitempty"`

	// Available reports whether the layer content is present in the
	// local layer store. The export does not pull layers that are
	// missing locally.
	Available bool `json:"Available"`
}
