package store

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrScrubRunning is returned by Scrub while another Scrub is running.
var ErrScrubRunning = errors.New("store: a scrub is already running")

// ScrubOptions tunes a Scrub.
type ScrubOptions struct {
	// MinAge protects young files: one younger than this is never taken
	// for an orphan, because a chunk is renamed into place a moment before
	// it is indexed.
	MinAge time.Duration
	// Pause is slept between directories, to keep a background scrub
	// gentle on the disk.
	Pause time.Duration
	// DryRun reports what would be repaired without changing anything.
	DryRun bool
}

// ScrubResult is what a Scrub found. In a dry run nothing was removed or
// forgotten, but the counts are the same.
type ScrubResult struct {
	Files   int   // chunk files examined
	Orphans int   // files on disk the index did not know about, removed
	Missing int   // indexed chunks whose file was gone, forgotten
	Corrupt int   // indexed chunks whose file had the wrong size, removed
	Freed   int64 // bytes removed with the orphans and corrupt files
}

// scrubLogLimit caps how many missing or corrupt chunks one Scrub names in
// the log; the result has the totals.
const scrubLogLimit = 100

// Scrub reconciles the index with the files on disk. It removes chunk files
// the index does not know about (left by a crash before an index flush, or
// by a delete that failed), forgets indexed chunks whose file has gone, and
// removes indexed chunks whose file is not the size the index says. None of
// these needs reading chunk contents: it lists directories and stats files,
// so it costs the same however large the chunks are.
//
// It walks the whole tree, sleeping opt.Pause between directories. Only one
// Scrub runs at a time; another gets ErrScrubRunning. It returns early with
// ctx's error if ctx ends.
func (s *FileStore) Scrub(ctx context.Context, opt ScrubOptions) (ScrubResult, error) {
	var res ScrubResult
	if !s.scrubMu.TryLock() {
		return res, ErrScrubRunning
	}
	defer s.scrubMu.Unlock()
	gen := s.scrubGen.Add(1)
	began := time.Now()
	logged := 0

	top, err := os.ReadDir(s.root)
	if err != nil {
		return res, err
	}
	for _, d1 := range top {
		if !d1.IsDir() || len(d1.Name()) != 2 {
			continue // index.db, tmp/
		}
		p1 := filepath.Join(s.root, d1.Name())
		subs, err := os.ReadDir(p1)
		if err != nil {
			continue
		}
		for _, d2 := range subs {
			if !d2.IsDir() || len(d2.Name()) != 2 {
				continue
			}
			if err := sleepCtx(ctx, opt.Pause); err != nil {
				return res, err
			}
			p2 := filepath.Join(p1, d2.Name())
			files, err := os.ReadDir(p2)
			if err != nil {
				continue
			}
			for _, f := range files {
				s.scrubFile(filepath.Join(p2, f.Name()), f, gen, began, &opt, &res, &logged)
			}
		}
	}

	// Anything indexed that no walk found. Entries touched or created since
	// the walk began are skipped: their file may not have existed when its
	// directory was listed.
	s.Iterate(func(k Key, info Info) bool {
		if info.LastAccess >= began.UnixNano() {
			return true
		}
		e, ok := s.lookup(k)
		if !ok || e.seen == gen || (!opt.DryRun && !s.forget(k, e.id)) {
			return true
		}
		res.Missing++
		if logged++; logged <= scrubLogLimit {
			s.log.Warn("cached chunk file missing", "path", s.path(k), "dry_run", opt.DryRun)
		}
		return true
	})
	return res, nil
}

func (s *FileStore) scrubFile(path string, d fs.DirEntry, gen uint32, began time.Time, opt *ScrubOptions, res *ScrubResult, logged *int) {
	var k Key
	if d.IsDir() || len(d.Name()) != 2*len(k) {
		return
	}
	if _, err := hex.Decode(k[:], []byte(d.Name())); err != nil {
		return // not one of ours; leave it
	}
	if s.path(k) != path {
		return // right name, wrong directory
	}
	res.Files++
	e, indexed := s.markSeen(k, gen)
	info, err := d.Info()
	if err != nil {
		return // gone since the listing
	}
	if indexed {
		// A re-commit renames a whole file over this one, and a chunk's
		// size never changes, so a mismatch is never a write in progress.
		if info.Size() == e.size {
			return
		}
		if !opt.DryRun && !s.drop(k, e.id) {
			return // replaced or deleted since
		}
		res.Corrupt++
		res.Freed += info.Size()
		if *logged++; *logged <= scrubLogLimit {
			s.log.Warn("cached chunk file has the wrong size", "path", path, "size", info.Size(), "want", e.size, "dry_run", opt.DryRun)
		}
		return
	}
	if began.Sub(info.ModTime()) < opt.MinAge {
		return
	}
	// Re-check: it may have been indexed since the lookup above.
	if _, indexed := s.markSeen(k, gen); indexed {
		return
	}
	if !opt.DryRun {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("remove orphan chunk file", "err", err)
			return
		}
	}
	res.Orphans++
	res.Freed += info.Size()
}

// markSeen records that scrub generation gen found k's file, and returns
// k's entry if k is indexed.
func (s *FileStore) markSeen(k Key, gen uint32) (entry, bool) {
	sh := s.shardOf(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e, ok := sh.m[k]
	if ok {
		e.seen = gen
		sh.m[k] = e
	}
	return e, ok
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
