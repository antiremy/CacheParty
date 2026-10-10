package store

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"cacheparty/internal/metrics"
)

var (
	bucketChunks  = []byte("chunks")
	bucketObjects = []byte("objects")
)

const (
	shardCount          = 256
	defaultFlushEvery   = 10 * time.Second
	copyBufSize         = 256 << 10
	indexFile           = "index.db"
	tmpDir              = "tmp"
	chunkRecordSize     = 16
	objectRecordMinSize = 8 + 4 + 4
)

// Options tunes a FileStore; zero values select defaults.
type Options struct {
	// FlushInterval is how often batched index updates (new chunks, deletes,
	// last-access times) are written to bbolt. Default 10s.
	FlushInterval time.Duration
	Log           *slog.Logger
	// BytesWritten, if set, counts chunk bytes written to disk as they are
	// written, so a long fill shows up while it runs.
	BytesWritten *metrics.Counter
}

// entry is the in-memory index record for one chunk. It is stored by value
// and holds no pointers, so the garbage collector never has to scan the
// index, however many chunks it holds.
type entry struct {
	size       int64
	lastAccess int64
	id         uint32 // per commit, so a re-commit is never mistaken for the entry it replaced
	seen       uint32 // generation of the last Scrub that found its file
	dirty      bool   // already queued for the next flush
	// verified: the file is known to hold size bytes, because this process
	// wrote it or Get has checked it. Only a crash before start-up can
	// leave a short file, so one check per entry is enough.
	verified bool
}

type shard struct {
	mu sync.RWMutex
	m  map[Key]entry
}

// FileStore is the v1 ChunkStore: one file per chunk, a 2-level hashed
// directory tree (like nginx levels=2:2) and a bbolt index.
//
// The whole index lives in memory and is loaded from bbolt at startup, so
// start-up reads one file and never walks the tree. Index changes are queued
// and written in batches: a hit costs a shard-lock update and no disk I/O.
// A crash can lose up to FlushInterval of index updates; the consequence is
// an unindexed file (wasted space, never wrong bytes) or a stale access time.
type FileStore struct {
	root    string
	db      *bolt.DB
	log     *slog.Logger
	written *metrics.Counter

	shards [shardCount]shard

	objMu   sync.RWMutex
	objects map[Key]*ObjectMeta

	chunks, bytes atomic.Int64

	// Pending index writes, drained by flush.
	pendMu   sync.Mutex
	dirty    []Key
	deleted  map[Key]struct{}
	objDirty map[Key]*ObjectMeta // nil value = delete

	flushMu  sync.Mutex // serialises flush
	bufs     sync.Pool
	tmpSeq   atomic.Uint64
	scrubMu  sync.Mutex // one Scrub at a time: they share the seen marks
	scrubGen atomic.Uint32
	entryID  atomic.Uint32

	stop chan struct{}
	done chan struct{}
}

// Open opens (creating if needed) a FileStore rooted at dir.
func Open(dir string, opts Options) (*FileStore, error) {
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultFlushEvery
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	tmp := filepath.Join(dir, tmpDir)
	// Anything in tmp is an interrupted write from a previous run.
	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, indexFile), 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	s := &FileStore{
		root:     dir,
		db:       db,
		log:      opts.Log,
		written:  opts.BytesWritten,
		objects:  make(map[Key]*ObjectMeta),
		deleted:  make(map[Key]struct{}),
		objDirty: make(map[Key]*ObjectMeta),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.bufs.New = func() any { b := make([]byte, copyBufSize); return &b }
	for i := range s.shards {
		s.shards[i].m = make(map[Key]entry)
	}
	if err := s.load(); err != nil {
		db.Close()
		return nil, err
	}
	go s.flusher(opts.FlushInterval)
	return s, nil
}

func (s *FileStore) load() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		cb, err := tx.CreateBucketIfNotExists(bucketChunks)
		if err != nil {
			return err
		}
		ob, err := tx.CreateBucketIfNotExists(bucketObjects)
		if err != nil {
			return err
		}
		err = cb.ForEach(func(k, v []byte) error {
			if len(k) != len(Key{}) || len(v) != chunkRecordSize {
				return nil // unknown record: ignore rather than guess
			}
			e := entry{
				size:       int64(binary.LittleEndian.Uint64(v)),
				lastAccess: int64(binary.LittleEndian.Uint64(v[8:])),
				id:         s.entryID.Add(1),
			}
			s.shardOf(Key(k)).m[Key(k)] = e
			s.chunks.Add(1)
			s.bytes.Add(e.size)
			return nil
		})
		if err != nil {
			return err
		}
		apps := map[string]string{}
		return ob.ForEach(func(k, v []byte) error {
			if m, ok := decodeObject(v, apps); ok && len(k) == len(Key{}) {
				s.objects[Key(k)] = m
			}
			return nil
		})
	})
}

func (s *FileStore) shardOf(k Key) *shard { return &s.shards[k[0]] }

// lookup returns a copy of k's entry.
func (s *FileStore) lookup(k Key) (entry, bool) {
	sh := s.shardOf(k)
	sh.mu.RLock()
	e, ok := sh.m[k]
	sh.mu.RUnlock()
	return e, ok
}

// path returns the chunk file path: <root>/<h[30:32]>/<h[28:30]>/<h>.
func (s *FileStore) path(k Key) string {
	var h [32]byte
	hex.Encode(h[:], k[:])
	var stack [192]byte // one allocation (the string) for any sane root
	b := append(stack[:0], s.root...)
	b = append(b, filepath.Separator)
	b = append(b, h[30:32]...)
	b = append(b, filepath.Separator)
	b = append(b, h[28:30]...)
	b = append(b, filepath.Separator)
	b = append(b, h[:]...)
	return string(b)
}

// Get implements ChunkStore.
func (s *FileStore) Get(k Key) (Chunk, error) {
	e, ok := s.lookup(k)
	if !ok {
		return Chunk{}, ErrNotFound
	}
	p := s.path(k)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if s.forget(k, e.id) {
				s.log.Warn("cached chunk file missing; forgot it", "path", p)
			}
			return Chunk{}, ErrNotFound
		}
		return Chunk{}, err
	}
	// A crash between rename and writeback can leave a short or empty file.
	// Never serve it.
	if !e.verified {
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return Chunk{}, err
		}
		if st.Size() != e.size {
			f.Close()
			if s.drop(k, e.id) {
				s.log.Warn("cached chunk file has the wrong size; removed it", "path", p, "size", st.Size(), "want", e.size)
			}
			return Chunk{}, ErrNotFound
		}
	}
	s.touch(k, e.id)
	return Chunk{File: f, Size: e.size}, nil
}

// touch records an access in memory, and that the entry's file has been
// verified; the bbolt write happens at the next flush, and only once per
// flush interval per chunk.
func (s *FileStore) touch(k Key, id uint32) {
	now := time.Now().UnixNano()
	sh := s.shardOf(k)
	sh.mu.Lock()
	e, ok := sh.m[k]
	if !ok || e.id != id {
		sh.mu.Unlock()
		return // gone, or replaced by a newer commit
	}
	queue := !e.dirty
	e.lastAccess, e.dirty, e.verified = now, true, true
	sh.m[k] = e
	sh.mu.Unlock()
	if queue {
		s.pendMu.Lock()
		s.dirty = append(s.dirty, k)
		s.pendMu.Unlock()
	}
}

// Stat implements ChunkStore.
func (s *FileStore) Stat(k Key) (Info, error) {
	e, ok := s.lookup(k)
	if !ok {
		return Info{}, ErrNotFound
	}
	return Info{Size: e.size, LastAccess: e.lastAccess}, nil
}

// Commit implements ChunkStore.
func (s *FileStore) Commit(k Key, size int64, r io.Reader) error {
	if size < 0 {
		return fmt.Errorf("store: negative chunk size %d", size)
	}
	tmp := filepath.Join(s.root, tmpDir, fmt.Sprintf("%016x.%d", s.tmpSeq.Add(1), os.Getpid()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := s.fill(f, size, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	dst := s.path(k)
	if err := os.Rename(tmp, dst); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			os.Remove(tmp)
			return err
		}
		// The two directory levels are created on first use.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	s.publish(k, size)
	return nil
}

// fill copies exactly size bytes from r into f.
func (s *FileStore) fill(f *os.File, size int64, r io.Reader) error {
	bp := s.bufs.Get().(*[]byte)
	defer s.bufs.Put(bp)
	buf := *bp
	var done int64
	for done < size {
		chunk := buf
		if rem := size - done; rem < int64(len(chunk)) {
			chunk = chunk[:rem]
		}
		n, err := r.Read(chunk)
		if n > 0 {
			if _, werr := f.Write(chunk[:n]); werr != nil {
				return werr
			}
			s.written.Add(int64(n))
			done += int64(n)
		}
		if err != nil {
			if err == io.EOF && done == size {
				break
			}
			if err == io.EOF {
				return fmt.Errorf("store: short chunk body: got %d of %d bytes: %w", done, size, io.ErrUnexpectedEOF)
			}
			return err
		}
	}
	// The body must end here: extra bytes mean the caller's size is wrong.
	var one [1]byte
	if n, err := r.Read(one[:]); n > 0 {
		return fmt.Errorf("store: chunk body longer than %d bytes", size)
	} else if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// publish makes a renamed file visible in the index.
func (s *FileStore) publish(k Key, size int64) {
	e := entry{size: size, lastAccess: time.Now().UnixNano(), id: s.entryID.Add(1), dirty: true, verified: true}
	sh := s.shardOf(k)
	sh.mu.Lock()
	old, had := sh.m[k]
	sh.m[k] = e
	sh.mu.Unlock()
	if had {
		s.bytes.Add(size - old.size)
	} else {
		s.chunks.Add(1)
		s.bytes.Add(size)
	}
	s.pendMu.Lock()
	s.dirty = append(s.dirty, k)
	delete(s.deleted, k)
	s.pendMu.Unlock()
}

// Delete implements ChunkStore.
func (s *FileStore) Delete(k Key) error {
	e, ok := s.lookup(k)
	if !ok {
		return nil
	}
	if err := os.Remove(s.path(k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.forget(k, e.id)
	return nil
}

// drop forgets a corrupt entry and deletes its file, and reports whether it
// did. The file is only removed if entry id was still current, so a
// concurrent re-commit is not clobbered.
func (s *FileStore) drop(k Key, id uint32) bool {
	if !s.forget(k, id) {
		return false
	}
	if err := os.Remove(s.path(k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("remove corrupt chunk", "err", err)
	}
	return true
}

// forget removes k from the index if entry id is still the current one, and
// reports whether it was.
func (s *FileStore) forget(k Key, id uint32) bool {
	sh := s.shardOf(k)
	sh.mu.Lock()
	cur, ok := sh.m[k]
	if !ok || cur.id != id {
		sh.mu.Unlock()
		return false // gone, or replaced by a newer commit
	}
	delete(sh.m, k)
	sh.mu.Unlock()
	s.chunks.Add(-1)
	s.bytes.Add(-cur.size)
	s.pendMu.Lock()
	s.deleted[k] = struct{}{}
	s.pendMu.Unlock()
	return true
}

// Iterate implements ChunkStore.
func (s *FileStore) Iterate(fn func(Key, Info) bool) {
	type kv struct {
		k Key
		i Info
	}
	var batch []kv
	for i := range s.shards {
		sh := &s.shards[i]
		batch = batch[:0]
		sh.mu.RLock()
		for k, e := range sh.m {
			batch = append(batch, kv{k, Info{e.size, e.lastAccess}})
		}
		sh.mu.RUnlock()
		for _, x := range batch {
			if !fn(x.k, x.i) {
				return
			}
		}
	}
}

// Object implements ChunkStore.
func (s *FileStore) Object(k Key) (*ObjectMeta, bool) {
	s.objMu.RLock()
	m, ok := s.objects[k]
	s.objMu.RUnlock()
	return m, ok
}

// PutObject implements ChunkStore.
func (s *FileStore) PutObject(k Key, m *ObjectMeta) error {
	s.objMu.Lock()
	s.objects[k] = m
	s.objMu.Unlock()
	s.pendMu.Lock()
	s.objDirty[k] = m
	s.pendMu.Unlock()
	return nil
}

// DeleteObject implements ChunkStore.
func (s *FileStore) DeleteObject(k Key) error {
	s.objMu.Lock()
	delete(s.objects, k)
	s.objMu.Unlock()
	s.pendMu.Lock()
	s.objDirty[k] = nil
	s.pendMu.Unlock()
	return nil
}

// IterateObjects implements ChunkStore.
func (s *FileStore) IterateObjects(fn func(Key, *ObjectMeta) bool) {
	type kv struct {
		k Key
		m *ObjectMeta
	}
	s.objMu.RLock()
	batch := make([]kv, 0, len(s.objects))
	for k, m := range s.objects {
		batch = append(batch, kv{k, m})
	}
	s.objMu.RUnlock()
	for _, x := range batch {
		if !fn(x.k, x.m) {
			return
		}
	}
}

// Usage implements ChunkStore.
func (s *FileStore) Usage() (chunks, bytes int64) { return s.chunks.Load(), s.bytes.Load() }

func (s *FileStore) flusher(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := s.Flush(); err != nil {
				s.log.Error("flush cache index", "err", err)
			}
		case <-s.stop:
			return
		}
	}
}

// Flush writes all pending index changes to bbolt in one transaction.
func (s *FileStore) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.pendMu.Lock()
	dirty, deleted, objs := s.dirty, s.deleted, s.objDirty
	s.dirty = nil
	s.deleted = make(map[Key]struct{})
	s.objDirty = make(map[Key]*ObjectMeta)
	s.pendMu.Unlock()
	if len(dirty) == 0 && len(deleted) == 0 && len(objs) == 0 {
		return nil
	}

	// Keys are random, so in arrival order each write descends to a
	// different B+tree leaf. In key order consecutive writes reuse the
	// nodes the previous one loaded. The pages written are the same.
	cmpKey := func(a, b Key) int { return bytes.Compare(a[:], b[:]) }
	slices.SortFunc(dirty, cmpKey)
	dirty = slices.Compact(dirty) // a re-commit queues its key again
	gone := slices.SortedFunc(maps.Keys(deleted), cmpKey)

	err := s.db.Update(func(tx *bolt.Tx) error {
		cb, ob := tx.Bucket(bucketChunks), tx.Bucket(bucketObjects)
		for _, k := range gone {
			if err := cb.Delete(k[:]); err != nil {
				return err
			}
		}
		// bbolt keeps a reference to each value until the transaction
		// commits, so every record needs its own bytes.
		slab := make([]byte, 0, chunkRecordSize*len(dirty))
		for _, k := range dirty {
			sh := s.shardOf(k)
			sh.mu.Lock()
			e, ok := sh.m[k]
			if ok {
				e.dirty = false
				sh.m[k] = e
			}
			sh.mu.Unlock()
			if !ok {
				continue // deleted since; its delete is queued
			}
			at := len(slab)
			slab = binary.LittleEndian.AppendUint64(slab, uint64(e.size))
			slab = binary.LittleEndian.AppendUint64(slab, uint64(e.lastAccess))
			if err := cb.Put(k[:], slab[at:len(slab):len(slab)]); err != nil {
				return err
			}
		}
		for k, m := range objs {
			var err error
			if m == nil {
				err = ob.Delete(k[:])
			} else {
				err = ob.Put(k[:], encodeObject(m))
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Requeue so the next flush retries. Entries still marked dirty are
		// in `dirty` already; re-add them.
		s.pendMu.Lock()
		s.dirty = append(dirty, s.dirty...)
		for k := range deleted {
			s.deleted[k] = struct{}{}
		}
		for k, m := range objs {
			if _, newer := s.objDirty[k]; !newer {
				s.objDirty[k] = m
			}
		}
		s.pendMu.Unlock()
	}
	return err
}

// Close flushes and closes the store.
func (s *FileStore) Close() error {
	close(s.stop)
	<-s.done
	ferr := s.Flush()
	if err := s.db.Close(); err != nil {
		return err
	}
	return ferr
}

// encodeObject lays out an object record: size, then length-prefixed
// Content-Type, Last-Modified and App. App was added later and is left out
// when empty, so records from older versions decode unchanged, and older
// versions ignore it.
func encodeObject(m *ObjectMeta) []byte {
	b := make([]byte, 0, objectRecordMinSize+len(m.ContentType)+len(m.LastModified)+4+len(m.App))
	b = binary.LittleEndian.AppendUint64(b, uint64(m.Size))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(m.ContentType)))
	b = append(b, m.ContentType...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(m.LastModified)))
	b = append(b, m.LastModified...)
	if m.App != "" {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(m.App)))
		b = append(b, m.App...)
	}
	return b
}

// decodeObject decodes an object record. intern, if not nil, dedupes App
// values, which are shared by every object of a game.
func decodeObject(b []byte, intern map[string]string) (*ObjectMeta, bool) {
	if len(b) < objectRecordMinSize {
		return nil, false
	}
	size := int64(binary.LittleEndian.Uint64(b))
	b = b[8:]
	str := func() (string, bool) {
		if len(b) < 4 {
			return "", false
		}
		n := int(binary.LittleEndian.Uint32(b))
		if len(b) < 4+n {
			return "", false
		}
		s := string(b[4 : 4+n])
		b = b[4+n:]
		return s, true
	}
	ct, ok1 := str()
	lm, ok2 := str()
	if !ok1 || !ok2 {
		return nil, false
	}
	m := NewObjectMeta(size, ct, lm)
	if len(b) > 0 {
		app, ok := str()
		if !ok {
			return nil, false
		}
		if intern != nil {
			if s, seen := intern[app]; seen {
				app = s
			} else {
				intern[app] = app
			}
		}
		m.App = app
	}
	return m, true
}

var _ ChunkStore = (*FileStore)(nil)
