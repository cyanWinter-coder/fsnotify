package fsnotify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrTreeChanged is returned by [Watcher.AddTree] when a directory in the tree
// was deleted or renamed while the watches were being registered.
//
// Registration is transactional: when this (or any other) error is returned
// all watches added by the call are removed again, and no path from the
// half-registered tree is reported as watched.
var ErrTreeChanged = errors.New("fsnotify: directory tree changed while registering watches")

type (
	// TreeOpt configures [Watcher.AddTree]; see the With* functions.
	TreeOpt  func(*treeOpts)
	treeOpts struct {
		maxDepth       int // < 0 means unlimited.
		followSymlinks bool
		ignore         func(path string, d fs.DirEntry) bool
	}
)

// WithMaxDepth limits how deep [Watcher.AddTree] descends below the root. The
// root itself is depth 0, so WithMaxDepth(0) watches only the root,
// WithMaxDepth(1) also watches its direct subdirectories, and so on.
//
// The default of -1 (or any negative value) means there is no limit.
func WithMaxDepth(depth int) TreeOpt {
	return func(o *treeOpts) { o.maxDepth = depth }
}

// WithFollowSymlinks controls whether [Watcher.AddTree] follows symbolic
// links to directories. The default is false: symlinks are not followed.
//
// Even when enabled, a symlink is only followed if its target resolves to a
// location inside the root directory, so the traversal can never escape the
// tree. Every directory inode is visited at most once, so symlink loops and
// multiple links to the same directory don't result in duplicate watches.
func WithFollowSymlinks(follow bool) TreeOpt {
	return func(o *treeOpts) { o.followSymlinks = follow }
}

// WithIgnore sets a function to exclude paths from [Watcher.AddTree]: no
// watch is set up for paths for which it returns true, and ignored
// directories are not descended into. The path is relative to the root passed
// to AddTree.
//
// The function is called while the registration is in progress and must not
// call any Watcher methods.
//
// The default is to not ignore anything.
func WithIgnore(fn func(path string, d fs.DirEntry) bool) TreeOpt {
	return func(o *treeOpts) { o.ignore = fn }
}

func getTreeOpts(opts []TreeOpt) treeOpts {
	o := treeOpts{maxDepth: -1}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// AddTree recursively watches root and all of its subdirectories, and returns
// the list of watched paths in stable (lexical) order.
//
// Registration is transactional: either the entire tree is registered, or –
// when any error occurs – every watch added by this call is removed again and
// an error is returned. Watches that already existed before the call are
// never touched, so calling AddTree again with the same root is idempotent.
// Concurrent [Watcher.Add], [Watcher.Remove], [Watcher.Close], and
// [Watcher.WatchList] calls block until the registration either succeeded or
// was rolled back in full, and never observe a half-registered tree.
//
// If a directory in the tree is deleted or renamed while the watches are
// being set up, AddTree rolls back and returns an error matching
// [ErrTreeChanged]; stale paths are never reported as members of the tree.
//
// Symbolic links are not followed by default; see [WithFollowSymlinks]. Use
// [WithMaxDepth] and [WithIgnore] to limit the traversal.
//
// New directories created inside the tree after AddTree returns are not
// watched automatically; AddTree registers the tree as it exists at call
// time.
func (w *Watcher) AddTree(root string, opts ...TreeOpt) ([]string, error) {
	o := getTreeOpts(opts)

	w.treeMu.Lock()
	defer w.treeMu.Unlock()

	root = filepath.Clean(root)
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("fsnotify: AddTree: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("fsnotify: AddTree: not a directory: %q", root)
	}
	// Canonical root, used to make sure symlink targets can't escape the tree.
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("fsnotify: AddTree: %w", err)
	}

	// Watches that existed before this call; rollback must not touch these,
	// so that repeated AddTree calls are idempotent.
	existing := make(map[string]struct{})
	for _, p := range w.b.WatchList() {
		existing[p] = struct{}{}
	}

	var (
		list  []string      // All registered paths, in walk order.
		infos []os.FileInfo // Directory identity for every entry in list.
		added []string      // Paths added by this call; rollback candidates.
		dd    dedup
	)
	dd.seen(rootInfo)

	// Best-effort rollback: release every watch descriptor this call
	// registered, leaving pre-existing watches alone.
	rollback := func(err error) ([]string, error) {
		for _, p := range added {
			w.b.Remove(p)
		}
		return nil, err
	}

	var walk func(dir string, info os.FileInfo, depth int) error
	walk = func(dir string, info os.FileInfo, depth int) error {
		if err := w.b.Add(dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: %s", ErrTreeChanged, dir)
			}
			return fmt.Errorf("fsnotify: AddTree: add watch on %q: %w", dir, err)
		}
		if _, ok := existing[dir]; !ok {
			added = append(added, dir)
		}
		list = append(list, dir)
		infos = append(infos, info)

		if o.maxDepth >= 0 && depth >= o.maxDepth {
			return nil
		}
		entries, err := os.ReadDir(dir) // Sorted by filename: stable order.
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: %s", ErrTreeChanged, dir)
			}
			return fmt.Errorf("fsnotify: AddTree: read directory %q: %w", dir, err)
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if o.ignore != nil && o.ignore(p, e) {
				continue
			}

			fi, err := e.Info()
			if err != nil {
				// Vanished between ReadDir and Info; it was never
				// registered, so just skip it.
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return fmt.Errorf("fsnotify: AddTree: %w", err)
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				if !o.followSymlinks {
					continue
				}
				resolved, err := filepath.EvalSymlinks(p)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue // Dangling symlink.
					}
					return fmt.Errorf("fsnotify: AddTree: resolve %q: %w", p, err)
				}
				if !hasPathPrefix(resolved, rootReal) {
					continue // Would escape the root.
				}
				fi, err = os.Stat(p)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					return fmt.Errorf("fsnotify: AddTree: %w", err)
				}
			}
			if !fi.IsDir() || dd.seen(fi) {
				continue
			}
			if err := walk(p, fi, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, rootInfo, 0); err != nil {
		return rollback(err)
	}

	// Make sure nothing was deleted or renamed out from under us while
	// registering, so a stale path is never reported as a member of the tree.
	for i, p := range list {
		fi, err := os.Stat(p)
		if err != nil || !os.SameFile(fi, infos[i]) {
			return rollback(fmt.Errorf("%w: %s", ErrTreeChanged, p))
		}
	}
	return list, nil
}

// fileID uniquely identifies a file on this system.
type fileID struct{ dev, ino uint64 }

// dedup tracks visited directories by inode, so the traversal never visits
// the same directory twice (hardlinks, symlink loops, bind mounts).
type dedup struct {
	ids   map[fileID]struct{}
	infos []os.FileInfo // Fallback for platforms without a file ID.
}

// seen reports if fi was seen before, recording it if not.
func (d *dedup) seen(fi os.FileInfo) bool {
	if id, ok := fileIDOf(fi); ok {
		if d.ids == nil {
			d.ids = make(map[fileID]struct{})
		}
		if _, ok := d.ids[id]; ok {
			return true
		}
		d.ids[id] = struct{}{}
		return false
	}
	for _, i := range d.infos {
		if os.SameFile(i, fi) {
			return true
		}
	}
	d.infos = append(d.infos, fi)
	return false
}
