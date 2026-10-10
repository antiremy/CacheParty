// Package store is the chunk cache's storage layer.
//
// ChunkStore is layout-agnostic: callers see opaque 16-byte keys, a Chunk
// that is a file plus an offset and length, and nothing about directories.
// FileStore (v1) keeps one file per chunk under a two-level hashed tree
// with a bbolt index; a later segment-file store can sit behind the same
// interface.
package store

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// ErrNotFound is returned by Get and Stat for a key that is not cached.
var ErrNotFound = errors.New("store: not found")

// Key identifies a chunk or an object. It is a truncated SHA-256, so keys
// are fixed-size and cheap to hold in memory.
type Key [16]byte

// ObjectKey is the key of an object: cache identifier plus request path
// (without the query string).
func ObjectKey(identifier, path string) Key {
	var stack [384]byte
	b := append(stack[:0], identifier...)
	b = append(b, 0)
	b = append(b, path...)
	sum := sha256.Sum256(b)
	return Key(sum[:16])
}

// ChunkKey is the key of chunk idx of an object. The slice size is part of
// the key, so changing CACHE_SLICE_SIZE can never make an old chunk be read
// at the wrong offset; it just starts a fresh set of chunks.
func ChunkKey(obj Key, slice, idx int64) Key {
	var b [32]byte
	copy(b[:16], obj[:])
	binary.LittleEndian.PutUint64(b[16:], uint64(slice))
	binary.LittleEndian.PutUint64(b[24:], uint64(idx))
	sum := sha256.Sum256(b[:])
	return Key(sum[:16])
}

// ObjectMeta is what is remembered about a whole object. It is immutable
// once handed to or returned from a store.
type ObjectMeta struct {
	Size         int64
	ContentType  string
	LastModified string
	// App names what the object belongs to, such as one game, so its
	// chunks can be found again (keys are hashes, so they cannot be). The
	// store treats it as opaque; "" means unknown.
	App string

	// Pre-built single-element header values, so serving a hit does not
	// allocate them per request. Callers must not modify them.
	typeHdr, modHdr []string
}

// NewObjectMeta builds an ObjectMeta.
func NewObjectMeta(size int64, contentType, lastModified string) *ObjectMeta {
	m := &ObjectMeta{Size: size, ContentType: contentType, LastModified: lastModified}
	if contentType != "" {
		m.typeHdr = []string{contentType}
	}
	if lastModified != "" {
		m.modHdr = []string{lastModified}
	}
	return m
}

// ContentTypeHeader is nil when the upstream sent no Content-Type.
func (m *ObjectMeta) ContentTypeHeader() []string { return m.typeHdr }

// LastModifiedHeader is nil when the upstream sent no Last-Modified.
func (m *ObjectMeta) LastModifiedHeader() []string { return m.modHdr }

// Info describes a stored chunk without opening it.
type Info struct {
	Size       int64
	LastAccess int64 // unix nanoseconds
}

// Chunk is an open chunk, returned by value (no allocation per hit). The zero
// value, with a nil File, means "no chunk". The data is Size bytes of File starting at Offset;
// the file position is Offset on return from Get, so io.CopyN(w, c.File, n)
// can use sendfile directly.
type Chunk struct {
	File   *os.File
	Offset int64
	Size   int64
}

// Close releases the chunk's file.
func (c Chunk) Close() error { return c.File.Close() }

// ChunkStore stores chunks and per-object metadata.
type ChunkStore interface {
	// Get opens a chunk and records the access. ErrNotFound if absent (or if
	// the stored file is missing or the wrong length, which is repaired by
	// forgetting the entry and logged). Any other error is the file system's,
	// and the entry is left alone.
	Get(k Key) (Chunk, error)
	// Commit atomically publishes exactly size bytes read from r as chunk k.
	// If r yields a different number of bytes, nothing is published.
	Commit(k Key, size int64, r io.Reader) error
	// Delete removes a chunk. Deleting an absent key is not an error.
	Delete(k Key) error
	// Stat reports a chunk without opening it or recording an access.
	Stat(k Key) (Info, error)
	// Iterate calls fn for every chunk, in no particular order, until fn
	// returns false. It works on a snapshot, so fn may call Delete.
	Iterate(fn func(Key, Info) bool)

	// Object returns the metadata for an object.
	Object(k Key) (*ObjectMeta, bool)
	// PutObject records (or replaces) object metadata.
	PutObject(k Key, m *ObjectMeta) error
	// DeleteObject forgets object metadata.
	DeleteObject(k Key) error
	// IterateObjects calls fn for every object until fn returns false. It
	// works on a snapshot, so fn may call DeleteObject.
	IterateObjects(fn func(Key, *ObjectMeta) bool)

	// Usage reports the number of chunks and the bytes they hold.
	Usage() (chunks, bytes int64)
	Close() error
}
