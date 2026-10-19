package images

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/distribution"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

var (
	diffIDA   = digest.FromString("diff-a").String()
	diffIDB   = digest.FromString("diff-b").String()
	diffIDC   = digest.FromString("diff-c").String()
	diffIDAMD = digest.FromString("diff-amd").String()
	diffIDARM = digest.FromString("diff-arm").String()

	dgstBase  = digest.FromString("compressed-base").String()
	dgstApp   = digest.FromString("compressed-app").String()
	digestAMD = digest.FromString("compressed-amd").String()
	digestARM = digest.FromString("compressed-arm").String()

	manifestAMD = digest.FromString("manifest-amd").String()
	manifestARM = digest.FromString("manifest-arm").String()
)

// chainIDs returns the chain ID for each prefix of the given diff IDs.
func chainIDs(diffIDs []layer.DiffID) []digest.Digest {
	var acc []digest.Digest
	var out []digest.Digest
	for _, d := range diffIDs {
		acc = append(acc, digest.Digest(d))
		out = append(out, identity.ChainID(acc))
	}
	return out
}

func testConfigImage(id, osName, arch, variant string, diffIDs []layer.DiffID, times []time.Time) *image.Image {
	img := image.NewImage(image.ID(digest.Digest(id)))
	img.OS = osName
	img.Architecture = arch
	img.Variant = variant

	rootFS := image.NewRootFS()
	for _, d := range diffIDs {
		rootFS.Append(d)
	}
	img.RootFS = rootFS

	for _, created := range times {
		c := created
		img.History = append(img.History, ocispec.History{
			Created:    &c,
			CreatedBy:  "test layer",
			EmptyLayer: false,
		})
	}
	return img
}

// fakeLayerStore implements layer.Store and layer.DescriptorReadStore.
type fakeLayerStore struct {
	present       map[layer.ChainID]bool
	descriptors   map[layer.ChainID]distribution.Descriptor
	descriptorErr error
	registerCalls int
}

func (s *fakeLayerStore) Register(io.Reader, layer.ChainID) (layer.Layer, error) {
	s.registerCalls++
	return nil, errors.New("register must not be called by the export")
}

func (s *fakeLayerStore) Get(id layer.ChainID) (layer.Layer, error) {
	if !s.present[id] {
		return nil, layer.ErrLayerDoesNotExist
	}
	return &fakeLayer{chainID: id}, nil
}

func (s *fakeLayerStore) Map() map[layer.ChainID]layer.Layer { return nil }

func (s *fakeLayerStore) Release(layer.Layer) ([]layer.Metadata, error) { return nil, nil }

func (s *fakeLayerStore) CreateRWLayer(string, layer.ChainID, *layer.CreateRWLayerOpts) (layer.RWLayer, error) {
	return nil, errors.New("not used")
}

func (s *fakeLayerStore) GetRWLayer(string) (layer.RWLayer, error) {
	return nil, layer.ErrMountDoesNotExist
}
func (s *fakeLayerStore) GetMountID(string) (string, error) { return "", nil }
func (s *fakeLayerStore) ReleaseRWLayer(layer.RWLayer) ([]layer.Metadata, error) {
	return nil, nil
}
func (s *fakeLayerStore) Cleanup() error            { return nil }
func (s *fakeLayerStore) DriverStatus() [][2]string { return nil }
func (s *fakeLayerStore) DriverName() string        { return "fake" }

func (s *fakeLayerStore) Descriptor(id layer.ChainID) (distribution.Descriptor, error) {
	if s.descriptorErr != nil {
		return distribution.Descriptor{}, s.descriptorErr
	}
	return s.descriptors[id], nil
}

type fakeLayer struct {
	chainID layer.ChainID
}

func (l *fakeLayer) TarStream() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (l *fakeLayer) TarStreamFrom(layer.ChainID) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (l *fakeLayer) ChainID() layer.ChainID               { return l.chainID }
func (l *fakeLayer) DiffID() layer.DiffID                 { return "" }
func (l *fakeLayer) Parent() layer.Layer                  { return nil }
func (l *fakeLayer) Size() int64                          { return 0 }
func (l *fakeLayer) DiffSize() int64                      { return 0 }
func (l *fakeLayer) Metadata() (map[string]string, error) { return nil, nil }

type fakeImageStore struct {
	images map[image.ID]*image.Image
}

func (s *fakeImageStore) Create([]byte) (image.ID, error) { return "", nil }
func (s *fakeImageStore) Get(id image.ID) (*image.Image, error) {
	if img, ok := s.images[id]; ok {
		return img, nil
	}
	return nil, errors.New("no such image")
}
func (s *fakeImageStore) Delete(image.ID) ([]layer.Metadata, error) { return nil, nil }
func (s *fakeImageStore) Search(string) (image.ID, error)           { return "", errors.New("not found") }
func (s *fakeImageStore) SetParent(image.ID, image.ID) error        { return nil }
func (s *fakeImageStore) GetParent(image.ID) (image.ID, error)      { return "", errors.New("no parent") }
func (s *fakeImageStore) SetLastUpdated(image.ID) error             { return nil }
func (s *fakeImageStore) GetLastUpdated(image.ID) (time.Time, error) {
	return time.Time{}, nil
}
func (s *fakeImageStore) SetBuiltLocally(image.ID) error        { return nil }
func (s *fakeImageStore) IsBuiltLocally(image.ID) (bool, error) { return false, nil }
func (s *fakeImageStore) Children(image.ID) []image.ID          { return nil }
func (s *fakeImageStore) Map() map[image.ID]*image.Image        { return s.images }
func (s *fakeImageStore) Heads() map[image.ID]*image.Image      { return s.images }
func (s *fakeImageStore) Len() int                              { return len(s.images) }

type fakeReferenceStore struct {
	ids map[string]digest.Digest
}

func (s *fakeReferenceStore) References(digest.Digest) []reference.Named { return nil }
func (s *fakeReferenceStore) ReferencesByName(reference.Named) []refstore.Association {
	return nil
}
func (s *fakeReferenceStore) AddTag(reference.Named, digest.Digest, bool) error { return nil }
func (s *fakeReferenceStore) AddDigest(reference.Canonical, digest.Digest, bool) error {
	return nil
}
func (s *fakeReferenceStore) Delete(reference.Named) (bool, error) { return false, nil }
func (s *fakeReferenceStore) Get(ref reference.Named) (digest.Digest, error) {
	if id, ok := s.ids[ref.String()]; ok {
		return id, nil
	}
	return "", refstore.ErrDoesNotExist
}

type errMetadataStore struct{ err error }

func (s *errMetadataStore) Get(string, string) ([]byte, error) { return nil, s.err }
func (s *errMetadataStore) Set(string, string, []byte) error   { return nil }
func (s *errMetadataStore) Delete(string, string) error        { return nil }

func newExportService(t *testing.T, images map[image.ID]*image.Image, refs map[string]digest.Digest, layers *fakeLayerStore, metaStore metadata.Store) *ImageService {
	t.Helper()
	return &ImageService{
		imageStore:                &fakeImageStore{images: images},
		referenceStore:            &fakeReferenceStore{ids: refs},
		layerStore:                layers,
		distributionMetadataStore: metaStore,
	}
}

// findByDigest returns the layer-metadata entry with the given compressed
// digest, or fails the test.
func findByDigest(t *testing.T, result []imagetypes.LayerMetadata, dgst string) imagetypes.LayerMetadata {
	t.Helper()
	for _, r := range result {
		if r.Digest == dgst {
			return r
		}
	}
	t.Fatalf("layer with digest %s not found in result", dgst)
	return imagetypes.LayerMetadata{}
}

// TestImageLayerMetadataMerge verifies that the export walks the layer chain,
// reports the expected fields, merges duplicate compressed digests while
// preserving all source repositories, tolerates missing layers and metadata,
// and emits deterministic output. It also verifies that the export never
// registers (pulls) layer content.
func TestImageLayerMetadataMerge(t *testing.T) {
	tmp := t.TempDir()
	fsStore, err := metadata.NewFSMetadataStore(tmp)
	assert.NilError(t, err)
	v2 := metadata.NewV2MetadataService(fsStore)

	diffIDs := []layer.DiffID{layer.DiffID(diffIDA), layer.DiffID(diffIDB), layer.DiffID(diffIDC)}
	chains := chainIDs(diffIDs)

	// Layer A: two metadata entries with the same digest but different
	// repositories (cross-repository blob mount).
	assert.NilError(t, v2.Add(layer.DiffID(diffIDA), metadata.V2Metadata{
		Digest:           digest.Digest(dgstBase),
		SourceRepository: "example.com/base",
	}))
	assert.NilError(t, v2.Add(layer.DiffID(diffIDA), metadata.V2Metadata{
		Digest:           digest.Digest(dgstBase),
		SourceRepository: "docker.io/library/base",
	}))
	// Layer B: single repository, with a recorded compressed media type.
	assert.NilError(t, v2.Add(layer.DiffID(diffIDB), metadata.V2Metadata{
		Digest:           digest.Digest(dgstApp),
		SourceRepository: "example.com/app",
	}))
	// Layer C: no distribution metadata (built locally).

	times := []time.Time{
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC),
	}
	imgID := image.ID(digest.FromString("image-merge"))
	img := testConfigImage(imgID.String(), "linux", "amd64", "", diffIDs, times)

	layers := &fakeLayerStore{
		present: map[layer.ChainID]bool{
			chains[0]: true,
			chains[1]: true,
			// chains[2] content is missing locally.
		},
		descriptors: map[layer.ChainID]distribution.Descriptor{
			chains[1]: {Digest: digest.Digest(dgstApp), MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip"},
		},
	}

	svc := newExportService(t,
		map[image.ID]*image.Image{imgID: img},
		map[string]digest.Digest{},
		layers,
		fsStore,
	)

	result, err := svc.ImageLayerMetadata(context.Background(), imgID.String(), nil)
	assert.NilError(t, err)
	assert.Equal(t, len(result), 3)
	assert.Equal(t, layers.registerCalls, 0, "export must not register or pull layers")

	// Deterministic JSON: marshaling twice yields identical bytes.
	first, err := json.Marshal(result)
	assert.NilError(t, err)
	second, err := json.Marshal(result)
	assert.NilError(t, err)
	assert.DeepEqual(t, first, second)

	// Entries with a known digest come first (sorted), unknown digest last.
	assert.Equal(t, result[2].Digest, "")
	base := findByDigest(t, result, dgstBase)
	app := findByDigest(t, result, dgstApp)

	// Layer A: merged references, sorted and de-duplicated.
	assert.DeepEqual(t, base.SourceRepositories, []string{"docker.io/library/base", "example.com/base"})
	assert.Equal(t, base.DiffID, diffIDA)
	assert.Equal(t, base.ChainID, chains[0].String())
	assert.Equal(t, base.Parent, "")
	assert.Equal(t, base.Available, true)
	assert.Assert(t, base.Created.Equal(times[0]))

	// Layer B: parent chain, compressed media type, creation time.
	assert.DeepEqual(t, app.SourceRepositories, []string{"example.com/app"})
	assert.Equal(t, app.DiffID, diffIDB)
	assert.Equal(t, app.ChainID, chains[1].String())
	assert.Equal(t, app.Parent, chains[0].String())
	assert.Equal(t, app.MediaType, "application/vnd.docker.image.rootfs.diff.tar.gzip")
	assert.Equal(t, app.Available, true)
	assert.Assert(t, app.Created.Equal(times[1]))

	// Layer C: built locally, missing content, still exported.
	unknown := result[2]
	assert.Equal(t, unknown.DiffID, diffIDC)
	assert.Equal(t, unknown.ChainID, chains[2].String())
	assert.Equal(t, unknown.Parent, chains[1].String())
	assert.Equal(t, unknown.MediaType, "")
	assert.Equal(t, unknown.Available, false)
	assert.Assert(t, len(unknown.SourceRepositories) == 0)
	assert.Assert(t, unknown.Created.Equal(times[2]))
}

// TestImageLayerMetadataMultiPlatform verifies that the platform option
// selects the correct variant of a multi-platform image.
func TestImageLayerMetadataMultiPlatform(t *testing.T) {
	amdDiffIDs := []layer.DiffID{layer.DiffID(diffIDAMD)}
	armDiffIDs := []layer.DiffID{layer.DiffID(diffIDARM)}
	amdChains := chainIDs(amdDiffIDs)
	armChains := chainIDs(armDiffIDs)

	idAMD := image.ID(digest.FromString("image-amd64"))
	idARM := image.ID(digest.FromString("image-arm64"))
	created := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	imgAMD := testConfigImage(idAMD.String(), "linux", "amd64", "", amdDiffIDs, []time.Time{created})
	imgARM := testConfigImage(idARM.String(), "linux", "arm", "v8", armDiffIDs, []time.Time{created})

	tmp := t.TempDir()
	fsStore, err := metadata.NewFSMetadataStore(tmp)
	assert.NilError(t, err)
	v2 := metadata.NewV2MetadataService(fsStore)
	assert.NilError(t, v2.Add(layer.DiffID(diffIDAMD), metadata.V2Metadata{Digest: digest.Digest(digestAMD), SourceRepository: "example.com/app"}))
	assert.NilError(t, v2.Add(layer.DiffID(diffIDARM), metadata.V2Metadata{Digest: digest.Digest(digestARM), SourceRepository: "example.com/app"}))

	refAMD := "example.com/app@" + manifestAMD
	refARM := "example.com/app@" + manifestARM
	anyAMD, err := reference.ParseAnyReference(refAMD)
	assert.NilError(t, err)
	anyARM, err := reference.ParseAnyReference(refARM)
	assert.NilError(t, err)
	refs := map[string]digest.Digest{
		anyAMD.(reference.Named).String(): idAMD.Digest(),
		anyARM.(reference.Named).String(): idARM.Digest(),
	}

	layers := &fakeLayerStore{
		present: map[layer.ChainID]bool{amdChains[0]: true, armChains[0]: true},
	}

	// Reuse the bolt-backed lease/content stores so that the platform
	// post-check in GetImage can query (and not find) manifest-list leases.
	ctx, cs, leasedImages, cleanup := setupTestStores(t)
	defer cleanup(t)

	svc := newExportService(t,
		map[image.ID]*image.Image{idAMD: imgAMD, idARM: imgARM},
		refs,
		layers,
		fsStore,
	)
	svc.leases = leasedImages.leases
	svc.content = cs

	amdPlatform := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	armPlatform := ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v8"}

	amdResult, err := svc.ImageLayerMetadata(ctx, refAMD, &amdPlatform)
	assert.NilError(t, err)
	assert.Equal(t, len(amdResult), 1)
	assert.Equal(t, amdResult[0].Digest, digestAMD)
	assert.Equal(t, amdResult[0].ChainID, amdChains[0].String())

	armResult, err := svc.ImageLayerMetadata(ctx, refARM, &armPlatform)
	assert.NilError(t, err)
	assert.Equal(t, len(armResult), 1)
	assert.Equal(t, armResult[0].Digest, digestARM)
	assert.Equal(t, armResult[0].ChainID, armChains[0].String())

	// Platform mismatch: the amd64 variant does not match an arm64 request
	// and no manifest-list lease exists, so the image is not found.
	_, err = svc.ImageLayerMetadata(ctx, refAMD, &armPlatform)
	assert.Assert(t, err != nil)
	assert.Check(t, is.Contains(err.Error(), "does not match the specified platform"))
}

// TestImageLayerMetadataPermissionErrors verifies that permission errors
// while reading local metadata are surfaced rather than swallowed.
func TestImageLayerMetadataPermissionErrors(t *testing.T) {
	diffIDs := []layer.DiffID{layer.DiffID(diffIDA)}
	chains := chainIDs(diffIDs)
	created := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	imgID := image.ID(digest.FromString("image-permission"))

	t.Run("distribution metadata permission denied", func(t *testing.T) {
		img := testConfigImage(imgID.String(), "linux", "amd64", "", diffIDs, []time.Time{created})
		layers := &fakeLayerStore{present: map[layer.ChainID]bool{chains[0]: true}}
		svc := newExportService(t,
			map[image.ID]*image.Image{imgID: img},
			map[string]digest.Digest{},
			layers,
			&errMetadataStore{err: os.ErrPermission},
		)
		_, err := svc.ImageLayerMetadata(context.Background(), imgID.String(), nil)
		assert.Assert(t, err != nil)
		assert.Assert(t, errors.Is(err, os.ErrPermission), "expected permission error, got %v", err)
		assert.Equal(t, layers.registerCalls, 0)
	})

	t.Run("layer descriptor permission denied", func(t *testing.T) {
		img := testConfigImage(imgID.String(), "linux", "amd64", "", diffIDs, []time.Time{created})
		layers := &fakeLayerStore{
			present:       map[layer.ChainID]bool{chains[0]: true},
			descriptorErr: os.ErrPermission,
		}
		svc := newExportService(t,
			map[image.ID]*image.Image{imgID: img},
			map[string]digest.Digest{},
			layers,
			&errMetadataStore{},
		)
		_, err := svc.ImageLayerMetadata(context.Background(), imgID.String(), nil)
		assert.Assert(t, err != nil)
		assert.Assert(t, errors.Is(err, os.ErrPermission), "expected permission error, got %v", err)
	})
}

// TestImageLayerMetadataLegacyMetadata verifies that metadata stored by
// older daemons (single-entry format) or in an unrecognized format does not
// break the export: legacy entries are used and corrupt entries are skipped.
func TestImageLayerMetadataLegacyMetadata(t *testing.T) {
	tmp := t.TempDir()
	fsStore, err := metadata.NewFSMetadataStore(tmp)
	assert.NilError(t, err)

	diffIDs := []layer.DiffID{layer.DiffID(diffIDA), layer.DiffID(diffIDB)}
	chains := chainIDs(diffIDs)
	created := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	imgID := image.ID(digest.FromString("image-legacy"))
	img := testConfigImage(imgID.String(), "linux", "amd64", "", diffIDs, []time.Time{created, created})

	// Raw legacy single-object metadata for layer A; unparseable metadata
	// for layer B.
	writeRawV2Metadata(t, tmp, layer.DiffID(diffIDA), `{"Digest":"`+dgstBase+`","SourceRepository":"example.com/legacy"}`)
	writeRawV2Metadata(t, tmp, layer.DiffID(diffIDB), `{this is not valid metadata`)

	layers := &fakeLayerStore{present: map[layer.ChainID]bool{chains[0]: true, chains[1]: true}}
	svc := newExportService(t,
		map[image.ID]*image.Image{imgID: img},
		map[string]digest.Digest{},
		layers,
		fsStore,
	)

	result, err := svc.ImageLayerMetadata(context.Background(), imgID.String(), nil)
	assert.NilError(t, err)
	assert.Equal(t, len(result), 2)

	assert.Equal(t, result[0].Digest, dgstBase)
	assert.DeepEqual(t, result[0].SourceRepositories, []string{"example.com/legacy"})
	assert.Equal(t, result[0].ChainID, chains[0].String())

	// Corrupt metadata for layer B: layer is still exported without a digest.
	assert.Equal(t, result[1].Digest, "")
	assert.Equal(t, result[1].ChainID, chains[1].String())
	assert.Equal(t, result[1].Parent, chains[0].String())
}

// writeRawV2Metadata writes raw bytes to the v2 metadata file for a diffID,
// bypassing the service's JSON marshaling so that legacy formats can be
// simulated.
func writeRawV2Metadata(t *testing.T, basePath string, diffID layer.DiffID, raw string) {
	t.Helper()
	d := digest.Digest(diffID)
	dir := filepath.Join(basePath, "v2metadata-by-diffid", d.Algorithm().String())
	assert.NilError(t, os.MkdirAll(dir, 0o755))
	assert.NilError(t, os.WriteFile(filepath.Join(dir, d.Encoded()), []byte(raw), 0o644))
}

// TestImageLayerMetadataNotFound verifies that resolving an unknown image
// fails without touching the layer store.
func TestImageLayerMetadataNotFound(t *testing.T) {
	layers := &fakeLayerStore{}
	svc := newExportService(t,
		map[image.ID]*image.Image{},
		map[string]digest.Digest{},
		layers,
		&errMetadataStore{},
	)
	_, err := svc.ImageLayerMetadata(context.Background(), "example.com/missing:latest", nil)
	assert.Assert(t, err != nil)
	assert.Equal(t, layers.registerCalls, 0)
}
