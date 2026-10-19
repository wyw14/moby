package images

import (
	"context"
	"errors"
	"sort"
	"time"

	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageLayerMetadata exports the locally stored metadata of the layers of
// an image as stable JSON-friendly structures.
//
// The export is strictly read-only: it resolves the image from local stores
// only, never pulls missing layers, and never modifies local images or layer
// metadata. Layers that share a compressed digest are merged into a single
// entry while all of their source-repository references are preserved.
//
// Entries are sorted by compressed digest (layers without a known digest
// last, sorted by ChainID) and the source repositories within each entry
// are sorted and de-duplicated, so the output is deterministic for the same
// local state.
func (i *ImageService) ImageLayerMetadata(ctx context.Context, name string, platform *ocispec.Platform) ([]imagetypes.LayerMetadata, error) {
	start := time.Now()
	img, err := i.GetImage(ctx, name, imagebackend.GetImageOpts{Platform: platform})
	if err != nil {
		return nil, err
	}

	createdByDiffID, err := layerCreatedTimes(img)
	if err != nil {
		return nil, err
	}

	var descriptorStore layer.DescriptorReadStore
	if dp, ok := i.layerStore.(layer.DescriptorReadStore); ok {
		descriptorStore = dp
	}
	metadataService := metadata.NewV2MetadataService(i.distributionMetadataStore)

	// merged accumulates entries keyed by compressed digest. Layers without
	// a known compressed digest (for example layers built locally) are keyed
	// by their ChainID so that they remain distinct.
	merged := map[string]*imagetypes.LayerMetadata{}
	var keys []string

	rootFS := *img.RootFS
	rootFS.DiffIDs = nil
	for _, diffID := range img.RootFS.DiffIDs {
		parentChainID := rootFS.ChainID()
		rootFS.Append(diffID)
		chainID := rootFS.ChainID()

		entry := imagetypes.LayerMetadata{
			DiffID:    diffID.String(),
			ChainID:   chainID.String(),
			Available: i.layerAvailable(chainID),
		}
		if parentChainID != "" {
			entry.Parent = parentChainID.String()
		}
		if created := createdByDiffID[diffID]; created != nil {
			entry.Created = created
		}

		// The descriptor records the compressed media type (and, for some
		// layers such as Windows foreign layers, the compressed digest).
		var descriptorDigest, mediaType string
		if descriptorStore != nil {
			desc, err := descriptorStore.Descriptor(chainID)
			if err != nil {
				return nil, err
			}
			descriptorDigest = desc.Digest.String()
			mediaType = desc.MediaType
		}
		entry.MediaType = mediaType

		// All distribution metadata entries are read-only lookups; a missing
		// or legacy metadata file yields no entries whereas access errors
		// (such as permission denied) abort the export.
		distMetadata, err := metadataService.LookupMetadata(diffID)
		if err != nil {
			return nil, err
		}

		repositoriesByDigest := map[string]map[string]struct{}{}
		for _, meta := range distMetadata {
			dgst := meta.Digest.String()
			if dgst == "" {
				continue
			}
			repos := repositoriesByDigest[dgst]
			if repos == nil {
				repos = map[string]struct{}{}
				repositoriesByDigest[dgst] = repos
			}
			if meta.SourceRepository != "" {
				repos[meta.SourceRepository] = struct{}{}
			}
		}
		if len(repositoriesByDigest) == 0 && descriptorDigest != "" {
			// No v2 metadata, but the layer descriptor still records the
			// compressed digest (e.g. layers loaded with foreign sources).
			repositoriesByDigest[descriptorDigest] = map[string]struct{}{}
		}

		if len(repositoriesByDigest) == 0 {
			// Locally built layer (or layer with no recorded distribution
			// metadata): there is no compressed digest to merge on.
			entry.SourceRepositories = []string{}
			key := "\x00" + chainID.String()
			if _, exists := merged[key]; !exists {
				keys = append(keys, key)
			}
			merged[key] = &entry
			continue
		}

		for dgst, repos := range repositoriesByDigest {
			existing, exists := merged[dgst]
			if !exists {
				copyEntry := entry
				copyEntry.Digest = dgst
				keys = append(keys, dgst)
				merged[dgst] = &copyEntry
				existing = merged[dgst]
			}
			// Preserve every source-repository reference when merging.
			for repo := range repos {
				existing.SourceRepositories = append(existing.SourceRepositories, repo)
			}
		}
	}

	result := make([]imagetypes.LayerMetadata, 0, len(keys))
	for _, key := range keys {
		repos := merged[key].SourceRepositories
		sort.Strings(repos)
		merged[key].SourceRepositories = dedupeSorted(repos)
		result = append(result, *merged[key])
	}

	sort.SliceStable(result, func(a, b int) bool {
		aKnown, bKnown := result[a].Digest != "", result[b].Digest != ""
		if aKnown != bKnown {
			return aKnown
		}
		if result[a].Digest != result[b].Digest {
			return result[a].Digest < result[b].Digest
		}
		return result[a].ChainID < result[b].ChainID
	})

	metrics.ImageActions.WithValues("layers").UpdateSince(start)
	return result, nil
}

// layerAvailable reports whether the layer content for the given ChainID is
// present locally. It only retains and releases the layer; it never
// registers or downloads content.
func (i *ImageService) layerAvailable(chainID layer.ChainID) bool {
	l, err := i.layerStore.Get(chainID)
	if err != nil {
		return false
	}
	layer.ReleaseAndLog(i.layerStore, l)
	return true
}

// layerCreatedTimes maps the image's non-empty history entries to the
// corresponding layer DiffIDs, in history order.
func layerCreatedTimes(img *image.Image) (map[layer.DiffID]*time.Time, error) {
	created := map[layer.DiffID]*time.Time{}
	layerCounter := 0
	for _, h := range img.History {
		if h.EmptyLayer {
			continue
		}
		if len(img.RootFS.DiffIDs) <= layerCounter {
			return nil, errors.New("too many non-empty layers in History section")
		}
		created[img.RootFS.DiffIDs[layerCounter]] = h.Created
		layerCounter++
	}
	return created, nil
}

// dedupeSorted returns s without duplicate entries. s must already be
// sorted.
func dedupeSorted(s []string) []string {
	if len(s) <= 1 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
