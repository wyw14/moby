package client

import (
	"errors"
	"net/http"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/image"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestImageLayersError(t *testing.T) {
	client, err := New(WithMockClient(errorMock(http.StatusInternalServerError, "Server error")))
	assert.NilError(t, err)
	_, err = client.ImageLayers(t.Context(), "nothing")
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))
}

func TestImageLayersEmptyID(t *testing.T) {
	client, err := New(WithMockClient(errorMock(http.StatusInternalServerError, "unused")))
	assert.NilError(t, err)
	_, err = client.ImageLayers(t.Context(), "")
	assert.Check(t, is.ErrorContains(err, "No such image"))
	var notFound interface{ NotFound() }
	assert.Assert(t, errors.As(err, &notFound))
}

func TestImageLayers(t *testing.T) {
	const (
		expectedURL      = "/images/image_id/layers"
		expectedPlatform = `{"architecture":"arm64","os":"linux","variant":"v8"}`
	)
	created := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	layersResponse := `[
		{
			"Digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"DiffID": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"ChainID": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"MediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"Created": "2024-01-01T00:00:00Z",
			"SourceRepositories": ["example.com/a", "example.com/b"],
			"Available": true
		},
		{
			"Digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			"DiffID": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"ChainID": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"Parent": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"Available": false
		}
	]`

	client, err := New(WithMockClient(func(req *http.Request) (*http.Response, error) {
		assert.Check(t, assertRequest(req, http.MethodGet, expectedURL))
		assert.Check(t, is.Equal(req.URL.Query().Get("platform"), expectedPlatform))
		return mockResponse(http.StatusOK, nil, layersResponse)(req)
	}))
	assert.NilError(t, err)

	expected := ImageLayersResult{
		Items: []image.LayerMetadata{
			{
				Digest:             "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				DiffID:             "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				ChainID:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				MediaType:          "application/vnd.oci.image.layer.v1.tar+gzip",
				Created:            &created,
				SourceRepositories: []string{"example.com/a", "example.com/b"},
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

	result, err := client.ImageLayers(t.Context(), "image_id", ImageLayersWithPlatform(ocispec.Platform{
		Architecture: "arm64",
		OS:           "linux",
		Variant:      "v8",
	}))
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(result, expected))
}

func TestImageLayersRequiresVersion157(t *testing.T) {
	client, err := New(WithMockClient(errorMock(http.StatusInternalServerError, "unused")))
	assert.NilError(t, err)
	client.version = "1.56"

	_, err = client.ImageLayers(t.Context(), "image_id")
	assert.Check(t, is.ErrorContains(err, "requires API version 1.57"))
}
