package fsnotify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/fsnotify/fsnotify/internal"
)

// Create a directory tree:
//
//	root/a/aa
//	root/a/ab
//	root/b
//	root/file
func mkTree(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	mkdirAll(t, join(tmp, "a", "aa"), noWait)
	mkdirAll(t, join(tmp, "a", "ab"), noWait)
	mkdirAll(t, join(tmp, "b"), noWait)
	touch(t, join(tmp, "file"), noWait)
	return tmp
}

func sortedWatchList(w *Watcher) []string {
	l := w.WatchList()
	slices.Sort(l)
	return l
}

func TestAddTree(t *testing.T) {
	t.Run("register", func(t *testing.T) {
		t.Parallel()
		tmp := mkTree(t)

		w := newWatcher(t)
		defer w.Close()

		list, err := w.AddTree(tmp)
		if err != nil {
			t.Fatal(err)
		}

		want := []string{
			tmp,
			join(tmp, "a"),
			join(tmp, "a", "aa"),
			join(tmp, "a", "ab"),
			join(tmp, "b"),
		}
		if !slices.Equal(list, want) {
			t.Errorf("wrong list\nhave: %q\nwant: %q", list, want)
		}
		if wl := sortedWatchList(w); !slices.Equal(wl, want) {
			t.Errorf("wrong WatchList\nhave: %q\nwant: %q", wl, want)
		}
	})

	t.Run("events", func(t *testing.T) {
		t.Parallel()
		tmp := mkTree(t)

		w := newCollector(t)
		if _, err := w.w.AddTree(tmp); err != nil {
			t.Fatal(err)
		}
		w.collect(t)

		touch(t, join(tmp, "a", "aa", "file"))
		touch(t, join(tmp, "b", "file"))

		cmpEvents(t, tmp, w.stop(t), newEvents(t, `
			CREATE /a/aa/file
			CREATE /b/file
		`))
	})

	t.Run("idempotent", func(t *testing.T) {
		t.Parallel()
		tmp := mkTree(t)

		w := newWatcher(t)
		defer w.Close()

		first, err := w.AddTree(tmp)
		if err != nil {
			t.Fatal(err)
		}
		second, err := w.AddTree(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(first, second) {
			t.Errorf("second call returned different list\nfirst:  %q\nsecond: %q", first, second)
		}
		if l := len(w.WatchList()); l != len(first) {
			t.Errorf("WatchList has %d entries after two AddTree calls; want %d", l, len(first))
		}
	})

	t.Run("no duplicate events after re-register", func(t *testing.T) {
		t.Parallel()
		tmp := mkTree(t)

		w := newCollector(t)
		if _, err := w.w.AddTree(tmp); err != nil {
			t.Fatal(err)
		}
		if _, err := w.w.AddTree(tmp); err != nil {
			t.Fatal(err)
		}
		w.collect(t)

		touch(t, join(tmp, "b", "file"))

		cmpEvents(t, tmp, w.stop(t), newEvents(t, `
			CREATE /b/file
		`))
	})

	t.Run("not a directory", func(t *testing.T) {
		t.Parallel()
		tmp := mkTree(t)

		w := newWatcher(t)
		defer w.Close()

		for _, p := range []string{join(tmp, "file"), join(tmp, "nonexistent")} {
			if _, err := w.AddTree(p); err == nil {
				t.Errorf("AddTree(%q): expected error, got nil", p)
			}
		}
		if l := len(w.WatchList()); l != 0 {
			t.Errorf("WatchList has %d entries; want 0", l)
		}
	})

	t.Run("closed watcher", func(t *testing.T) {
		t.Parallel()
		tmp := mkTree(t)

		w := newWatcher(t)
		w.Close()

		_, err := w.AddTree(tmp)
		if !errors.Is(err, ErrClosed) {
			t.Errorf("expected ErrClosed, got: %v", err)
		}
	})
}

func TestAddTreeMaxDepth(t *testing.T) {
	t.Parallel()
	tmp := mkTree(t)

	tests := []struct {
		depth int
		want  []string
	}{
		{0, []string{tmp}},
		{1, []string{tmp, join(tmp, "a"), join(tmp, "b")}},
		{-1, []string{tmp, join(tmp, "a"), join(tmp, "a", "aa"), join(tmp, "a", "ab"), join(tmp, "b")}},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("depth=%d", tc.depth), func(t *testing.T) {
			w := newWatcher(t)
			defer w.Close()

			list, err := w.AddTree(tmp, WithMaxDepth(tc.depth))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(list, tc.want) {
				t.Errorf("\nhave: %q\nwant: %q", list, tc.want)
			}
		})
	}
}

func TestAddTreeIgnore(t *testing.T) {
	t.Parallel()
	tmp := mkTree(t)

	w := newWatcher(t)
	defer w.Close()

	list, err := w.AddTree(tmp, WithIgnore(func(path string, d fs.DirEntry) bool {
		return strings.HasPrefix(d.Name(), "a") // Ignore "a" and everything below it.
	}))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{tmp, join(tmp, "b")}
	if !slices.Equal(list, want) {
		t.Errorf("wrong list\nhave: %q\nwant: %q", list, want)
	}
	if wl := sortedWatchList(w); !slices.Equal(wl, want) {
		t.Errorf("wrong WatchList\nhave: %q\nwant: %q", wl, want)
	}
}

func TestAddTreeSymlinks(t *testing.T) {
	if !internal.HasPrivilegesForSymlink() {
		t.Skip("symlinks need admin permissions on Windows")
	}
	t.Parallel()

	t.Run("not followed by default", func(t *testing.T) {
		tmp := mkTree(t)
		symlink(t, join(tmp, "b"), join(tmp, "a", "link"), noWait)

		w := newWatcher(t)
		defer w.Close()

		list, err := w.AddTree(tmp)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			tmp,
			join(tmp, "a"),
			join(tmp, "a", "aa"),
			join(tmp, "a", "ab"),
			join(tmp, "b"),
		}
		if !slices.Equal(list, want) {
			t.Errorf("\nhave: %q\nwant: %q", list, want)
		}
	})

	t.Run("follow inside root", func(t *testing.T) {
		tmp := mkTree(t)
		symlink(t, join(tmp, "b"), join(tmp, "a", "link"), noWait)

		w := newWatcher(t)
		defer w.Close()

		list, err := w.AddTree(tmp, WithFollowSymlinks(true))
		if err != nil {
			t.Fatal(err)
		}
		// "b" is reached through "a/link" first (stable lexical order) and
		// watched via that path; visiting "b" itself afterwards is skipped as
		// the inode was already seen. Every directory is watched exactly once.
		want := []string{
			tmp,
			join(tmp, "a"),
			join(tmp, "a", "aa"),
			join(tmp, "a", "ab"),
			join(tmp, "a", "link"),
		}
		if !slices.Equal(list, want) {
			t.Errorf("\nhave: %q\nwant: %q", list, want)
		}
	})

	t.Run("no escape from root", func(t *testing.T) {
		tmp := mkTree(t)
		outside := t.TempDir()
		mkdirAll(t, join(outside, "sub"), noWait)
		symlink(t, outside, join(tmp, "a", "escape"), noWait)

		w := newWatcher(t)
		defer w.Close()

		list, err := w.AddTree(tmp, WithFollowSymlinks(true))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range list {
			if !hasPathPrefix(p, tmp) {
				t.Errorf("watched path escapes root: %q (list: %q)", p, list)
			}
		}
		if l := len(w.WatchList()); l != len(list) {
			t.Errorf("WatchList has %d entries; want %d", l, len(list))
		}
	})

	t.Run("symlink loop", func(t *testing.T) {
		tmp := mkTree(t)
		symlink(t, tmp, join(tmp, "a", "loop"), noWait)

		w := newWatcher(t)
		defer w.Close()

		list, err := w.AddTree(tmp, WithFollowSymlinks(true))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			tmp,
			join(tmp, "a"),
			join(tmp, "a", "aa"),
			join(tmp, "a", "ab"),
			join(tmp, "b"),
		}
		if !slices.Equal(list, want) {
			t.Errorf("\nhave: %q\nwant: %q", list, want)
		}
	})

	t.Run("dangling symlink", func(t *testing.T) {
		tmp := mkTree(t)
		symlink(t, join(tmp, "nonexistent"), join(tmp, "a", "dangling"), noWait)

		w := newWatcher(t)
		defer w.Close()

		if _, err := w.AddTree(tmp, WithFollowSymlinks(true)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAddTreeRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 doesn't restrict access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 doesn't restrict access for root")
	}
	t.Parallel()

	tmp := mkTree(t)
	chmod(t, 0o000, join(tmp, "a", "aa"), noWait)
	defer chmod(t, 0o755, join(tmp, "a", "aa"), noWait) // So TempDir cleanup works.

	w := newWatcher(t, join(tmp, "b")) // Pre-existing watch must survive the rollback.
	defer w.Close()

	_, err := w.AddTree(tmp)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	// Everything the failed call registered must be rolled back; only the
	// pre-existing watch may remain.
	want := []string{join(tmp, "b")}
	if wl := sortedWatchList(w); !slices.Equal(wl, want) {
		t.Errorf("rollback left watches behind\nhave: %q\nwant: %q", wl, want)
	}
}

// Delete or rename a directory exactly when the walk reaches "hook", using
// the ignore callback as a deterministic synchronization point.
func addTreeWithMutation(t *testing.T, w *Watcher, tmp, hook string, mutate func()) ([]string, error) {
	t.Helper()

	var (
		now  = make(chan struct{})
		done = make(chan struct{})
		list []string
		err  error
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		list, err = w.AddTree(tmp, WithIgnore(func(path string, d fs.DirEntry) bool {
			if d.Name() == hook {
				close(now)
				<-done
			}
			return false
		}))
	}()
	<-now
	mutate()
	close(done)
	wg.Wait()
	return list, err
}

func TestAddTreeDeleteDuringRegister(t *testing.T) {
	t.Parallel()

	mkDirs := func(t *testing.T) string {
		tmp := t.TempDir()
		for d := range 10 {
			mkdirAll(t, join(tmp, fmt.Sprintf("dir-%d", d)), noWait)
		}
		return tmp
	}

	t.Run("before visited", func(t *testing.T) {
		t.Parallel()
		tmp := mkDirs(t)

		w := newWatcher(t)
		defer w.Close()

		// Delete dir-5 before the walk reaches it: it was never registered,
		// so this is a successful registration without it.
		list, err := addTreeWithMutation(t, w, tmp, "dir-5", func() {
			rmAll(t, join(tmp, "dir-5"), noWait)
		})
		if err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
		if slices.Contains(list, join(tmp, "dir-5")) {
			t.Fatalf("deleted dir reported as member: %q", list)
		}
		if l := len(list); l != 10 {
			t.Fatalf("list has %d entries; want 10: %q", l, list)
		}
		if wl := sortedWatchList(w); !slices.Equal(list, wl) {
			t.Errorf("returned list doesn't match WatchList\nlist:  %q\nwatch: %q", list, wl)
		}
	})

	t.Run("after registered", func(t *testing.T) {
		t.Parallel()
		tmp := mkDirs(t)

		w := newWatcher(t)
		defer w.Close()

		// Delete dir-5 after it was registered but before the walk finishes:
		// the registration must fail and be rolled back in full.
		_, err := addTreeWithMutation(t, w, tmp, "dir-6", func() {
			rmAll(t, join(tmp, "dir-5"), noWait)
		})
		if !errors.Is(err, ErrTreeChanged) {
			t.Fatalf("expected ErrTreeChanged, got: %v", err)
		}
		if wl := w.WatchList(); len(wl) != 0 {
			t.Errorf("rollback left watches behind: %q", wl)
		}
	})
}

func TestAddTreeRenameDuringRegister(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	for d := range 10 {
		mkdirAll(t, join(tmp, fmt.Sprintf("dir-%d", d)), noWait)
	}

	w := newWatcher(t)
	defer w.Close()

	// Rename dir-5 after it was registered: the old path must not be
	// reported as a member, and the registration must fail and roll back.
	_, err := addTreeWithMutation(t, w, tmp, "dir-6", func() {
		mv(t, join(tmp, "dir-5"), join(tmp, "dir-5-renamed"), noWait)
	})
	if !errors.Is(err, ErrTreeChanged) {
		t.Fatalf("expected ErrTreeChanged, got: %v", err)
	}
	if wl := w.WatchList(); len(wl) != 0 {
		t.Errorf("rollback left watches behind: %q", wl)
	}
}

func TestAddTreeConcurrentCloseRemove(t *testing.T) {
	t.Parallel()

	mkDirs := func(t *testing.T) string {
		tmp := t.TempDir()
		for d := range 100 {
			mkdirAll(t, join(tmp, fmt.Sprintf("dir-%03d", d)), noWait)
		}
		return tmp
	}

	// AddTree racing with Close: either the registration completes fully and
	// Close then removes everything, or Close wins and AddTree fails. Never a
	// half-registered tree, never a deadlock.
	t.Run("close", func(t *testing.T) {
		for range 20 {
			tmp := mkDirs(t)

			w := newWatcher(t)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); w.AddTree(tmp) }()
			go func() { defer wg.Done(); w.Close() }()
			wg.Wait()

			if wl := w.WatchList(); wl != nil {
				t.Fatalf("WatchList not nil after Close: %q", wl)
			}
		}
	})

	// AddTree racing with Remove: Remove blocks until the registration
	// completed or was rolled back in full.
	t.Run("remove", func(t *testing.T) {
		for range 20 {
			tmp := mkDirs(t)

			w := newWatcher(t)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); w.AddTree(tmp) }()
			go func() { defer wg.Done(); w.Remove(tmp) }()
			wg.Wait()
			w.Close()
		}
	})
}
