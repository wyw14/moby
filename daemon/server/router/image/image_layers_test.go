package image

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/distribution/reference"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/internal/filters"
	dockerimage "github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
)

// fakeLayersBackend implements the image router Backend interface for the
// ImageLayers handler tests. Only ImageLayerMetadata is meaningful; the
// remaining methods exist to satisfy the interface.
type fakeLayersBackend struct {
	layers   []imagetypes.LayerMetadata
	err      error
	platform *ocispec.Platform
	name     string
}

func (b *fakeLayersBackend) ImageLayerMetadata(_ context.Context, imageName string, platform *ocispec.Platform) ([]imagetypes.LayerMetadata, error) {
	b.name = imageName
	b.platform = platform
	return b.layers, b.err
}

func (b *fakeLayersBackend) ImageDelete(context.Context, string, imagebackend.RemoveOptions) ([]imagetypes.DeleteResponse, error) {
	return nil, nil
}
func (b *fakeLayersBackend) ImageHistory(context.Context, string, *ocispec.Platform) ([]*imagetypes.HistoryResponseItem, error) {
	return nil, nil
}
func (b *fakeLayersBackend) Images(context.Context, imagebackend.ListOptions) ([]imagetypes.Summary, error) {
	return nil, nil
}
func (b *fakeLayersBackend) GetImage(context.Context, string, imagebackend.GetImageOpts) (*dockerimage.Image, error) {
	return nil, nil
}
func (b *fakeLayersBackend) ImageInspect(context.Context, string, imagebackend.ImageInspectOpts) (*imagebackend.InspectData, error) {
	return nil, nil
}
func (b *fakeLayersBackend) ImageAttestations(context.Context, string, imagebackend.AttestationOpts) ([]imagetypes.AttestationStatement, error) {
	return nil, nil
}
func (b *fakeLayersBackend) TagImage(context.Context, dockerimage.ID, reference.Named) error {
	return nil
}
func (b *fakeLayersBackend) ImagePrune(context.Context, filters.Args) (*imagetypes.PruneReport, error) {
	return nil, nil
}
func (b *fakeLayersBackend) LoadImage(context.Context, io.ReadCloser, []ocispec.Platform, io.Writer, bool) error {
	return nil
}
func (b *fakeLayersBackend) ImportImage(context.Context, reference.Named, *ocispec.Platform, string, io.Reader, []string) (dockerimage.ID, error) {
	return "", nil
}
func (b *fakeLayersBackend) ExportImage(context.Context, []string, []ocispec.Platform, io.Writer) error {
	return nil
}
func (b *fakeLayersBackend) PullImage(context.Context, reference.Named, imagebackend.PullOptions) error {
	return nil
}
func (b *fakeLayersBackend) PushImage(context.Context, reference.Named, imagebackend.PushOptions) error {
	return nil
}

var _ Backend = &fakeLayersBackend{}

func callGetImageLayers(t *testing.T, backend Backend, target string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, "1.57")
	req := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	resp := httptest.NewRecorder()
	err := (&imageRouter{backend: backend}).getImageLayers(ctx, resp, req, map[string]string{"name": "example.com/app:latest"})
	return resp, err
}

func TestGetImageLayersOK(t *testing.T) {
	backend := &fakeLayersBackend{
		layers: []imagetypes.LayerMetadata{
			{
				Digest:             "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				DiffID:             "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				ChainID:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				MediaType:          "application/vnd.docker.image.rootfs.diff.tar.gzip",
				SourceRepositories: []string{"example.com/app"},
				Available:          true,
			},
			{
				Digest:    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				DiffID:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				ChainID:   "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Parent:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Available: false,
			},
		},
	}

	resp, err := callGetImageLayers(t, backend, "/images/example.com/app:latest/layers?platform=%7B%22os%22%3A%22linux%22%2C%22architecture%22%3A%22amd64%22%7D")
	assert.NilError(t, err)
	assert.Equal(t, resp.Code, http.StatusOK)
	assert.Equal(t, resp.Header().Get("Content-Type"), "application/json")

	var got []imagetypes.LayerMetadata
	assert.NilError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, len(got), 2)
	assert.DeepEqual(t, got, backend.layers)

	assert.Assert(t, backend.platform != nil)
	assert.Equal(t, backend.platform.OS, "linux")
	assert.Equal(t, backend.platform.Architecture, "amd64")
	assert.Equal(t, backend.name, "example.com/app:latest")
}

func TestGetImageLayersEmpty(t *testing.T) {
	backend := &fakeLayersBackend{layers: []imagetypes.LayerMetadata{}}

	resp, err := callGetImageLayers(t, backend, "/images/example.com/app:latest/layers")
	assert.NilError(t, err)
	assert.Equal(t, resp.Code, http.StatusOK)
	// An empty result must be a JSON array, not null.
	assert.Equal(t, resp.Body.String(), "[]\n")
}

func TestGetImageLayersBadPlatform(t *testing.T) {
	backend := &fakeLayersBackend{}

	_, err := callGetImageLayers(t, backend, "/images/example.com/app:latest/layers?platform=not-json")
	assert.Assert(t, err != nil)
	var invalidParameter interface{ InvalidParameter() }
	assert.Assert(t, errors.As(err, &invalidParameter), "expected invalid-parameter error, got %v", err)
}

func TestGetImageLayersBackendError(t *testing.T) {
	backend := &fakeLayersBackend{err: errdefs.NotFound(errors.New("no such image"))}

	_, err := callGetImageLayers(t, backend, "/images/example.com/app:latest/layers")
	assert.Assert(t, err != nil)
	var notFound interface{ NotFound() }
	assert.Assert(t, errors.As(err, &notFound), "expected not-found error, got %v", err)
}
