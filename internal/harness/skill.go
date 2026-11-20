package harness

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bluesky-social/diderot/skills"
)

func skillFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(skills.FS, skills.Name, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := skills.FS.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skills.Name, path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	return files, err
}

// skillState compares the installed skill under parent with the embedded
// one: linked (a symlink, as from a development checkout, left alone),
// missing, current, or stale with the files that differ or should not be
// there.
func skillState(parent string) (dir, state string, stale []string, err error) {
	dir = filepath.Join(parent, skills.Name)
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return dir, "missing", nil, nil
	}
	if err != nil {
		return dir, "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return dir, "linked", nil, nil
	}
	want, err := skillFiles()
	if err != nil {
		return dir, "", nil, err
	}
	for rel, data := range want {
		got, err := os.ReadFile(filepath.Join(dir, rel)) //nolint:gosec // under the skill directory
		if err != nil || !bytes.Equal(got, data) {
			stale = append(stale, rel)
		}
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if _, ok := want[rel]; !ok {
			stale = append(stale, rel)
		}
		return nil
	})
	if err != nil {
		return dir, "", nil, err
	}
	if len(stale) > 0 {
		return dir, "stale", stale, nil
	}
	return dir, "current", nil, nil
}

// InstallSkill writes the embedded skill into parent/diderot, replacing
// files that differ and removing ones the skill no longer has.
func InstallSkill(parent string) (Change, error) {
	dir, state, _, err := skillState(parent)
	if err != nil {
		return Change{}, err
	}
	switch state {
	case "linked":
		return Change{Action: "unchanged", Where: dir + " (a symlink, left alone)"}, nil
	case "current":
		return Change{Action: "unchanged", Where: dir}, nil
	}
	want, err := skillFiles()
	if err != nil {
		return Change{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a skill is meant to be read
		return Change{}, err
	}
	// Remove what the skill no longer has before writing, so an old file
	// where a directory now goes cannot block the write.
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if _, ok := want[rel]; !ok {
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		return Change{}, err
	}
	for rel, data := range want {
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // as above
			return Change{}, err
		}
		// Never write through a symlink to somewhere outside the skill.
		if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return Change{}, fmt.Errorf("%s is a symlink; remove it and run setup again", dst)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil { //nolint:gosec // as above
			return Change{}, err
		}
	}
	action := "added"
	if state == "stale" {
		action = "updated"
	}
	return Change{Action: action, Where: dir}, nil
}

// CheckSkill reports why the skill under parent is not the embedded one.
func CheckSkill(parent string) error {
	dir, state, stale, err := skillState(parent)
	if err != nil {
		return err
	}
	switch state {
	case "missing":
		return fmt.Errorf("the skill is not installed at %s; run diderot setup", dir)
	case "stale":
		return fmt.Errorf("the skill at %s differs from this version's (%v); run diderot setup", dir, stale)
	}
	return nil
}
