package markdown

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
)

const (
	// maxFiles is how many Markdown files a scan reads before it stops with
	// errTooMany rather than index part of the folder.
	maxFiles = 10_000
	// maxFileBytes is the largest file a scan indexes. It reads one byte
	// more, whatever size the file reports, to tell it is over.
	maxFileBytes = 1 << 20
)

// problem is an error whose text is a sentence for the source's page.
type problem string

func (p problem) Error() string { return string(p) }

const (
	errNoFolder problem = "No folder is mounted for this deployment."
	errNoFiles  problem = "The folder has no Markdown files."
	errTooMany  problem = "The folder has more than 10,000 Markdown files."
)

var (
	// errSkip is a file a scan counts but does not index: over
	// maxFileBytes, not UTF-8 text (invalid UTF-8, or a NUL byte, which
	// Postgres text can't hold), or empty.
	errSkip = errors.New("markdown: file skipped")
	// errNotRegular is a path that was a regular file when the folder was
	// listed and is something else now, such as a symlink.
	errNotRegular = errors.New("markdown: not a regular file")
)

// describe is what the source's page says about err.
func describe(err error) string {
	if p, ok := errors.AsType[problem](err); ok {
		return string(p)
	}
	return "The scan failed: " + err.Error() + "."
}

// folder is what one walk of the folder found.
type folder struct {
	// paths are its Markdown files, relative and slash-separated, in
	// lexical order.
	paths []string
	// badNames counts its Markdown files whose path is not UTF-8.
	badNames int
}

// list walks the folder under root for Markdown files: regular files named
// .md or .markdown. It skips hidden files and directories, symlinks
// whatever they point to, and anything that is not a regular file or a
// directory, and stops with errTooMany past limit files.
func list(ctx context.Context, root *os.Root, limit int) (folder, error) {
	var found folder
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case p == ".":
			return nil
		case strings.HasPrefix(d.Name(), "."):
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case !d.Type().IsRegular() || !isMarkdown(d.Name()):
			return nil
		case len(found.paths)+found.badNames == limit:
			return errTooMany
		case !utf8.ValidString(p):
			found.badNames++
		default:
			found.paths = append(found.paths, p)
		}
		return nil
	})
	return found, err
}

func isMarkdown(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".md" || ext == ".markdown"
}

// file is one Markdown file's text and when it last changed.
type file struct {
	text     string
	modified time.Time
}

// read reads the file at p under root. It opens without blocking, so a file
// replaced by a named pipe since the walk is refused rather than waited on.
// os.Root follows a symlink inside the folder, so read refuses what it
// opened unless p itself is that regular file, not a link to it.
func read(root *os.Root, p string) (file, error) {
	f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return file{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return file{}, err
	}
	named, err := root.Lstat(p)
	if err != nil {
		return file{}, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, named) {
		return file{}, errNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return file{}, err
	}
	if len(data) > maxFileBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 || len(bytes.TrimSpace(data)) == 0 {
		return file{}, errSkip
	}
	return file{text: string(data), modified: info.ModTime()}, nil
}

// counts are what a scan did.
type counts struct {
	// indexed and skipped are the files it wrote or found unchanged, and
	// the ones it counted but did not index.
	indexed, skipped int64
	// changed and removed are the events it inserted or changed, and
	// deleted.
	changed, removed int
}

// scan reads the folder at dir once and writes an event for each Markdown
// file. When it has read the whole folder without an error, it deletes the
// events of files that are not there; a file skipped for its size or
// encoding is there, so its event stays. A folder with no Markdown files
// is errNoFiles, and deletes nothing.
func scan(ctx context.Context, deps source.Deps, dir, baseURL string, limit int) (counts, error) {
	var done counts
	root, err := os.OpenRoot(dir)
	if err != nil {
		return done, err
	}
	defer func() { _ = root.Close() }()
	found, err := list(ctx, root, limit)
	if err != nil {
		return done, err
	}
	done.skipped = int64(found.badNames)
	seen := map[string]bool{}
	for _, p := range found.paths {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		f, err := read(root, p)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotRegular):
			continue
		case errors.Is(err, errSkip):
			seen[p] = true
			done.skipped++
			continue
		case err != nil:
			return done, err
		}
		seen[p] = true
		written, err := deps.Sink.Upsert(ctx, f.event(p, baseURL))
		if err != nil {
			return done, err
		}
		done.indexed++
		if !written.NoChange {
			done.changed++
		}
	}
	if len(seen) == 0 {
		return done, errNoFiles
	}
	indexed, err := deps.Sink.ExternalIDs(ctx, ConnectorName)
	if err != nil {
		return done, err
	}
	for _, id := range indexed {
		if seen[id] {
			continue
		}
		if _, err := deps.Sink.Delete(ctx, ConnectorName, id); err != nil {
			return done, err
		}
		done.removed++
	}
	return done, nil
}

// event is the file at p as an event, linked under baseURL when it is set.
func (f file) event(p, baseURL string) events.Event {
	return events.Event{
		Source: ConnectorName, ExternalID: p, Scope: events.ScopeInternal,
		OccurredAt: f.modified.UTC(), Title: title(p, f.text), Body: f.text, URL: link(baseURL, p),
	}
}

// title is the text of the first "# " heading outside a code fence, or
// else the file's name without its extension.
func title(p, text string) string {
	fenced := false
	for line := range strings.Lines(text) {
		switch {
		case strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"):
			fenced = !fenced
		case !fenced && strings.HasPrefix(line, "# "):
			if heading := strings.TrimSpace(line[2:]); heading != "" {
				return heading
			}
		}
	}
	name := path.Base(p)
	return strings.TrimSuffix(name, path.Ext(name))
}

// link is baseURL followed by p, each of its segments escaped, or "" when
// baseURL is.
func link(baseURL, p string) string {
	if baseURL == "" {
		return ""
	}
	segments := strings.Split(p, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.TrimSuffix(baseURL, "/") + "/" + strings.Join(segments, "/")
}
