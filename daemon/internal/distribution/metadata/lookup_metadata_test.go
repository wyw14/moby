package metadata

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
)

// fakeStore is an in-memory metadata.Store used to inject raw bytes or
// read errors.
type fakeStore struct {
	data map[string][]byte
	err  error
}

func (s *fakeStore) Get(namespace, key string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	data, ok := s.data[namespace+"/"+key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (s *fakeStore) Set(namespace, key string, value []byte) error {
	s.data[namespace+"/"+key] = value
	return nil
}

func (s *fakeStore) Delete(namespace, key string) error {
	delete(s.data, namespace+"/"+key)
	return nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{data: map[string][]byte{}}
}

func TestLookupMetadataCurrentFormat(t *testing.T) {
	store := newFakeStore()
	service := NewV2MetadataService(store)
	diffID := layer.DiffID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	expected := []V2Metadata{
		{Digest: digest.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), SourceRepository: "example.com/a"},
		{Digest: digest.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"), SourceRepository: "example.com/b"},
	}
	assert.NilError(t, service.Add(diffID, expected[0]))
	assert.NilError(t, service.Add(diffID, expected[1]))

	got, err := service.LookupMetadata(diffID)
	assert.NilError(t, err)
	assert.Assert(t, reflect.DeepEqual(got, expected), "got %#v, want %#v", got, expected)
}

// TestLookupMetadataLegacySingleEntry verifies that metadata written by an
// older daemon as a single JSON object instead of an array is accepted.
func TestLookupMetadataLegacySingleEntry(t *testing.T) {
	store := newFakeStore()
	service := NewV2MetadataService(store)
	diffID := layer.DiffID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	legacy := &v2MetadataService{store: store}
	store.Set(legacy.diffIDNamespace(), legacy.diffIDKey(diffID), []byte(`{"Digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","SourceRepository":"example.com/legacy"}`))

	got, err := service.LookupMetadata(diffID)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Digest, digest.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	assert.Equal(t, got[0].SourceRepository, "example.com/legacy")
}

// TestLookupMetadataMissingEntry verifies that absent metadata results in no
// entries rather than an error.
func TestLookupMetadataMissingEntry(t *testing.T) {
	service := NewV2MetadataService(newFakeStore())

	got, err := service.LookupMetadata(layer.DiffID("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"))
	assert.NilError(t, err)
	assert.Assert(t, len(got) == 0)
}

// TestLookupMetadataCorruptEntry verifies that metadata in an unknown format
// is ignored instead of failing the read-only lookup.
func TestLookupMetadataCorruptEntry(t *testing.T) {
	store := newFakeStore()
	service := NewV2MetadataService(store)
	diffID := layer.DiffID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	legacy := &v2MetadataService{store: store}
	store.Set(legacy.diffIDNamespace(), legacy.diffIDKey(diffID), []byte(`{not valid json`))

	got, err := service.LookupMetadata(diffID)
	assert.NilError(t, err)
	assert.Assert(t, len(got) == 0)
}

// TestLookupMetadataNilStore verifies that a service without backend storage
// returns no metadata without error.
func TestLookupMetadataNilStore(t *testing.T) {
	service := NewV2MetadataService(nil)

	got, err := service.LookupMetadata(layer.DiffID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	assert.NilError(t, err)
	assert.Assert(t, len(got) == 0)
}

// TestLookupMetadataPermissionError verifies that access errors (such as
// permission denied) are surfaced to the caller instead of being swallowed.
func TestLookupMetadataPermissionError(t *testing.T) {
	store := newFakeStore()
	store.err = os.ErrPermission
	service := NewV2MetadataService(store)

	_, err := service.LookupMetadata(layer.DiffID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	assert.Assert(t, err != nil)
	assert.Assert(t, errors.Is(err, os.ErrPermission), "expected permission error, got %v", err)
}
