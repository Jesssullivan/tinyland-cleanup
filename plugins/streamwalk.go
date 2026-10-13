package plugins

import (
	"os"
	"path/filepath"
)

// streamWalkBatch is how many directory entries streamWalk holds at once.
// Tests lower it to cross batch boundaries with small fixtures.
var streamWalkBatch = 1024

// streamWalkHeldDepth is how many nested directories streamWalk keeps open at
// once. Below this depth it reads a directory in full and closes it before
// descending, as filepath.Walk does, so a very deep tree cannot run the
// process out of file descriptors.
const streamWalkHeldDepth = 64

// streamWalk walks the tree at root like filepath.Walk, with two differences
// that keep memory flat on very large directories:
//
//   - a directory is read in batches of streamWalkBatch entries instead of
//     all at once, so a cache directory with a million files costs one batch
//     of memory, not a million names;
//   - entries are visited in directory order, not sorted.
//
// Use it for walks whose result does not depend on order (sizing, "is
// anything recent in here"). Walks that delete as they go, or that resume
// from a cursor, stay on filepath.Walk.
//
// fn follows the filepath.WalkFunc contract: lstat information for every
// entry, symbolic links not followed, filepath.SkipDir and filepath.SkipAll
// honored. A directory that cannot be opened is reported to fn with the
// error. A directory that fails part way through reading is cut short.
func streamWalk(root string, fn filepath.WalkFunc) error {
	info, err := os.Lstat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = streamWalkPath(root, info, fn, 0)
	}
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

func streamWalkPath(path string, info os.FileInfo, fn filepath.WalkFunc, depth int) error {
	if !info.IsDir() {
		return fn(path, info, nil)
	}

	dir, openErr := os.Open(path)
	if err := fn(path, info, openErr); err != nil || openErr != nil {
		if dir != nil {
			dir.Close()
		}
		// As in filepath.Walk: fn decides, and an unreadable directory is
		// not descended into either way.
		return err
	}

	batch := streamWalkBatch
	if depth >= streamWalkHeldDepth || batch <= 0 {
		batch = -1
	}
	for {
		entries, readErr := dir.ReadDir(batch)
		if batch < 0 {
			dir.Close()
			dir = nil
		}
		for _, entry := range entries {
			child := filepath.Join(path, entry.Name())
			childInfo, err := os.Lstat(child)
			if err != nil {
				if err := fn(child, nil, err); err != nil && err != filepath.SkipDir {
					closeDir(dir)
					return err
				}
				continue
			}
			if err := streamWalkPath(child, childInfo, fn, depth+1); err != nil {
				if !childInfo.IsDir() || err != filepath.SkipDir {
					closeDir(dir)
					return err
				}
			}
		}
		if batch < 0 || readErr != nil || len(entries) == 0 {
			// io.EOF, or a read error part way through: either way this
			// directory is finished.
			closeDir(dir)
			return nil
		}
	}
}

func closeDir(dir *os.File) {
	if dir != nil {
		dir.Close()
	}
}
