package gitrepo

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// View is a read-only file system over the tree of one commit. It implements
// fs.FS, fs.ReadFileFS and fs.ReadDirFS. Symbolic links appear with
// fs.ModeSymlink and are never followed; submodules are not listed.
type View struct {
	sha  string
	tree *object.Tree
	repo *git.Repository
}

var (
	_ fs.FS         = (*View)(nil)
	_ fs.ReadFileFS = (*View)(nil)
	_ fs.ReadDirFS  = (*View)(nil)
)

// SHA is the commit this view shows.
func (v *View) SHA() string { return v.sha }

// Close releases the repository handles of the view.
func (v *View) Close() error {
	if c, ok := v.repo.Storer.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (v *View) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return v.openDir(name, v.tree)
	}
	entry, err := v.tree.FindEntry(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if entry.Mode == filemode.Dir {
		sub, err := v.tree.Tree(name)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return v.openDir(name, sub)
	}
	data, err := v.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return &memFile{Reader: bytes.NewReader(data), info: fileInfo{name: path.Base(name), size: int64(len(data)), mode: fsMode(entry.Mode)}}, nil
}

func (v *View) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	f, err := v.tree.File(name)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
		}
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	r, err := f.Reader()
	if err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func (v *View) ReadDir(name string) ([]fs.DirEntry, error) {
	tree := v.tree
	if name != "." {
		if !fs.ValidPath(name) {
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
		}
		sub, err := v.tree.Tree(name)
		if err != nil {
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
		}
		tree = sub
	}
	return dirEntries(tree), nil
}

func (v *View) openDir(name string, tree *object.Tree) (fs.File, error) {
	return &dirFile{info: fileInfo{name: path.Base(name), mode: fs.ModeDir}, entries: dirEntries(tree)}, nil
}

func dirEntries(tree *object.Tree) []fs.DirEntry {
	out := make([]fs.DirEntry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		if e.Mode == filemode.Submodule {
			continue
		}
		info := fileInfo{name: e.Name, mode: fsMode(e.Mode)}
		if e.Mode != filemode.Dir {
			if f, err := tree.TreeEntryFile(&e); err == nil {
				info.size = f.Size
			}
		}
		out = append(out, dirEntry{info: info})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func fsMode(m filemode.FileMode) fs.FileMode {
	switch m {
	case filemode.Dir:
		return fs.ModeDir
	case filemode.Symlink:
		return fs.ModeSymlink
	}
	return 0
}

type fileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode | 0o444 }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }

type dirEntry struct{ info fileInfo }

func (e dirEntry) Name() string               { return e.info.name }
func (e dirEntry) IsDir() bool                { return e.info.IsDir() }
func (e dirEntry) Type() fs.FileMode          { return e.info.mode.Type() }
func (e dirEntry) Info() (fs.FileInfo, error) { return e.info, nil }

type memFile struct {
	*bytes.Reader
	info fileInfo
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }

type dirFile struct {
	info    fileInfo
	entries []fs.DirEntry
	offset  int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dirFile) Close() error               { return nil }
func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}

func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.offset:]
	if n <= 0 {
		d.offset = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.offset += n
	return rest[:n], nil
}
