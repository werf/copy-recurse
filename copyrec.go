package copyrec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/werf/logboek"
)

type (
	MatchFileFunc      func(path string) (bool, error)
	MatchDirFunc       func(path string) (bool, error)
	FallThroughDirFunc func(path string) (bool, error)
)

type Options struct {
	UID *uint32
	GID *uint32

	MatchDir       MatchDirFunc
	MatchFile      MatchFileFunc
	FallThroughDir FallThroughDirFunc

	AbortIfDestParentDirNotExists bool
}

type CopyRecurse struct {
	src  string
	dest string
	uid  *uint32
	gid  *uint32

	matchDir       MatchDirFunc
	matchFile      MatchFileFunc
	fallThroughDir FallThroughDirFunc

	abortIfDestParentDirNotExists bool

	// TODO: how memory/CPU-effective is working with this?
	visitedDestDirs []string
}

func New(src, dest string, opts Options) (*CopyRecurse, error) {
	copyRec := &CopyRecurse{
		src:                           filepath.Clean(src),
		dest:                          filepath.Clean(dest),
		matchFile:                     opts.MatchFile,
		fallThroughDir:                opts.FallThroughDir,
		uid:                           opts.UID,
		gid:                           opts.GID,
		abortIfDestParentDirNotExists: opts.AbortIfDestParentDirNotExists,
	}

	if opts.MatchDir == nil && opts.MatchFile == nil {
		copyRec.matchDir = func(path string) (bool, error) {
			return true, nil
		}
	} else {
		copyRec.matchDir = opts.MatchDir
	}

	return copyRec, nil
}

func (c *CopyRecurse) Run(ctx context.Context) error {
	if err := c.prepareDestParentDir(ctx); err != nil {
		return fmt.Errorf("error creating destination directory: %w", err)
	}

	if err := walkPath(ctx, c.src, func(relEntryPath string, dirEntry *fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error walking path: %w", err)
		}

		entrySrc := filepath.Join(c.src, relEntryPath)
		entryDest := filepath.Join(c.dest, relEntryPath)

		logboek.Context(ctx).Debug().LogF("Walking path %q.\n", entrySrc)

		if (*dirEntry).IsDir() {
			if err := c.processDir(ctx, entrySrc, entryDest); errors.Is(err, fs.SkipDir) {
				return fs.SkipDir
			} else if err != nil {
				return fmt.Errorf("error processing directory: %w", err)
			}
		} else {
			if err := c.processFile(ctx, entrySrc, entryDest); err != nil {
				return fmt.Errorf("error processing file: %w", err)
			}
		}

		return nil
	}); err != nil {
		return fmt.Errorf("error walking path: %w", err)
	}

	return nil
}

func (c *CopyRecurse) prepareDestParentDir(ctx context.Context) error {
	logboek.Context(ctx).Debug().LogF("Preparing parent dir for destination %q.\n", c.dest)

	destParentDir := getParentDir(c.dest)

	_, err := os.Lstat(destParentDir)
	if errors.Is(err, os.ErrNotExist) {
		if c.abortIfDestParentDirNotExists {
			return fmt.Errorf("directory %q does not exist", destParentDir)
		}

		if err := os.MkdirAll(destParentDir, os.ModePerm); err != nil {
			return fmt.Errorf("error creating directories up to parent destination directory %q: %w", destParentDir, err)
		}
	} else if err != nil {
		return fmt.Errorf("error getting file info about parent destination directory %q: %w", destParentDir, err)
	}

	return nil
}

func (c *CopyRecurse) processFile(ctx context.Context, src, dest string) error {
	logboek.Context(ctx).Debug().LogF("Processing file %q.\n", src)

	if c.matchFile == nil {
		c.matchFile = func(path string) (bool, error) {
			return true, nil
		}
	}

	if match, err := c.matchFile(src); err != nil {
		return fmt.Errorf("error matching file %q: %w", src, err)
	} else if match {
		if err := c.copyRecurse(ctx, src, dest); err != nil {
			return fmt.Errorf("error copying file: %w", err)
		}
		return nil
	} else {
		logboek.Context(ctx).Debug().LogF("Skipping file %q.\n", src)
		return nil
	}
}

func (c *CopyRecurse) processDir(ctx context.Context, src, dest string) error {
	logboek.Context(ctx).Debug().LogF("Processing directory %q.\n", src)

	if c.matchDir != nil {
		if match, err := c.matchDir(src); err != nil {
			return fmt.Errorf("error matching directory %q: %w", src, err)
		} else if match {
			logboek.Context(ctx).Debug().LogF("Dir %q fully matched.\n", src)
			if err := c.copyRecurse(ctx, src, dest); err != nil {
				return fmt.Errorf("error copying directory: %w", err)
			}
			return nil
		}
	}

	if c.fallThroughDir == nil || filepath.Clean(src) == c.src {
		logboek.Context(ctx).Debug().LogF("Will look for matches in directory %q.\n", src)
		return nil
	}

	if fallThrough, err := c.fallThroughDir(src); err != nil {
		return fmt.Errorf("error determining whether to fall through directory %q: %w", src, err)
	} else if fallThrough {
		logboek.Context(ctx).Debug().LogF("Will look for matches in directory %q.\n", src)
		return nil
	} else {
		logboek.Context(ctx).Debug().LogF("Skipping directory %q.\n", src)
		return fs.SkipDir
	}
}

func (c *CopyRecurse) copyRecurse(ctx context.Context, src, dest string) error {
	logboek.Context(ctx).Debug().LogF("Going to recursively copy %q to %q with UID/GID %v/%v.\n", src, dest, uint32PtrPString(c.uid), uint32PtrPString(c.gid))

	srcFileInfo, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("error getting stat for path %q: %w", src, err)
	}

	switch {
	case srcFileInfo.IsDir():
		if err := walkPath(ctx, src, func(entryRelPath string, dirEntry *fs.DirEntry, e error) error {
			if e != nil {
				return fmt.Errorf("error walking path: %w", e)
			}

			absEntrySrcPath := filepath.Join(src, entryRelPath)
			absEntryDestPath := filepath.Join(dest, entryRelPath)

			logboek.Context(ctx).Debug().LogF("Walking path %q for copying.\n", absEntrySrcPath)

			srcEntryFileInfo, err := (*dirEntry).Info()
			if err != nil {
				return fmt.Errorf("error getting file info for entry %q: %w", absEntryDestPath, err)
			}

			switch {
			case srcEntryFileInfo.IsDir():
				if err := c.createEmptyDirsChain(ctx, absEntryDestPath); err != nil {
					return fmt.Errorf("error creating empty dirs chain: %w", err)
				}

				if err := c.processDirOwnership(ctx, absEntryDestPath, srcEntryFileInfo.Sys().(*syscall.Stat_t)); err != nil {
					return fmt.Errorf("error processing directory ownership: %w", err)
				}
			case srcEntryFileInfo.Mode().IsRegular():
				if err := c.createEmptyDirsChain(ctx, getParentDir(absEntryDestPath)); err != nil {
					return fmt.Errorf("error creating empty dirs chain: %w", err)
				}

				if err := c.copyFile(ctx, absEntrySrcPath, srcEntryFileInfo, srcEntryFileInfo.Sys().(*syscall.Stat_t), absEntryDestPath); err != nil {
					return fmt.Errorf("error copying file: %w", err)
				}
			case srcEntryFileInfo.Mode()&os.ModeSymlink != 0:
				if err := c.createEmptyDirsChain(ctx, getParentDir(absEntryDestPath)); err != nil {
					return fmt.Errorf("error creating empty dirs chain: %w", err)
				}

				if err := c.copySymlink(ctx, absEntrySrcPath, absEntryDestPath); err != nil {
					return fmt.Errorf("error copying symlink: %w", err)
				}
			default:
				logboek.Context(ctx).Warn().LogF("File %q is of a type %q. Copying of such a type is not supported, skipping.\n", absEntrySrcPath, srcEntryFileInfo.Mode().Type().String())
			}

			return nil
		}); err != nil {
			return fmt.Errorf("error walking path: %w", err)
		}
	case srcFileInfo.Mode().IsRegular():
		var srcStat *syscall.Stat_t
		if c.uid == nil || c.gid == nil {
			srcStat = srcFileInfo.Sys().(*syscall.Stat_t)
		}

		if err := c.createEmptyDirsChain(ctx, getParentDir(dest)); err != nil {
			return fmt.Errorf("error creating empty dirs chain: %w", err)
		}

		if err := c.copyFile(ctx, src, srcFileInfo, srcStat, dest); err != nil {
			return fmt.Errorf("error copying file: %w", err)
		}
	case srcFileInfo.Mode()&os.ModeSymlink != 0:
		if err := c.createEmptyDirsChain(ctx, getParentDir(dest)); err != nil {
			return fmt.Errorf("error creating empty dirs chain: %w", err)
		}

		if err := c.copySymlink(ctx, src, dest); err != nil {
			return fmt.Errorf("error copying symlink: %w", err)
		}
	default:
		logboek.Context(ctx).Warn().LogF("File %q is of a type %q. Copying of such a type is not supported, skipping.\n", src, srcFileInfo.Mode().Type().String())
	}

	return nil
}

func (c *CopyRecurse) createEmptyDirsChain(ctx context.Context, destPath string) error {
	logboek.Context(ctx).Debug().LogF("Going to create empty dirs chain (if needed) for path %q.\n", destPath)

	for _, visitedDir := range c.visitedDestDirs {
		if visitedDir == destPath {
			return nil
		}
	}

	relDestPath, err := filepath.Rel(c.dest, destPath)
	if err != nil {
		return fmt.Errorf("error calculating relative path for base %q and target %q: %w", c.dest, destPath, err)
	}

	dirsToVisit := []string{c.dest}
	relDestPathParts := strings.Split(relDestPath, "/")
	for i := 0; i < len(relDestPathParts); i++ {
		dirsToVisit = append(dirsToVisit, filepath.Join(c.dest, filepath.Join(relDestPathParts[:len(relDestPathParts)-i]...)))
	}

	for _, visitedDir := range c.visitedDestDirs {
		if len(dirsToVisit) == 0 {
			return nil
		}

		for i, dirToVisit := range dirsToVisit {
			if dirToVisit == visitedDir {
				if i == 0 {
					return nil
				}
				dirsToVisit = dirsToVisit[i+1:]
				break
			}
		}
	}

	for _, dir := range dirsToVisit {
		if err := c.createEmptyDirInChain(ctx, dir); err != nil {
			return fmt.Errorf("error creating empty dir %q: %w", destPath, err)
		}
	}

	return nil
}

func (c *CopyRecurse) createEmptyDirInChain(ctx context.Context, destPath string) error {
	logboek.Context(ctx).Debug().LogF("Going to create empty dir (if needed) %q.\n", destPath)

	relEntryPath, err := filepath.Rel(c.dest, destPath)
	if err != nil {
		return fmt.Errorf("error calculating relative source path from base %q and target %q: %w", c.dest, destPath, err)
	}

	srcPath := filepath.Join(c.src, relEntryPath)

	srcFileInfo, err := os.Lstat(srcPath)
	if err != nil {
		return fmt.Errorf("error getting file info for %q: %w", relEntryPath, err)
	}

	var srcStat *syscall.Stat_t
	if c.uid == nil || c.gid == nil {
		srcStat = srcFileInfo.Sys().(*syscall.Stat_t)
	}

	destFileInfo, err := os.Lstat(destPath)
	if errors.Is(err, os.ErrNotExist) {
		logboek.Context(ctx).Debug().LogF("Creating dir %q with perms %s.\n", destPath, srcFileInfo.Mode().Perm())
		if err := os.Mkdir(destPath, srcFileInfo.Mode().Perm()); err != nil {
			return fmt.Errorf("error creating directory %q: %w", destPath, err)
		}
	} else if err != nil {
		return fmt.Errorf("can't get file info for %q: %w", destPath, err)
	} else if !destFileInfo.IsDir() {
		logboek.Context(ctx).Debug().LogF("Removing path %q.\n", destPath)
		if err := os.RemoveAll(destPath); err != nil {
			return fmt.Errorf("error removing path %q: %w", destPath, err)
		}

		logboek.Context(ctx).Debug().LogF("Creating dir %q with perms %s.\n", destPath, srcFileInfo.Mode().Perm())
		if err := os.Mkdir(destPath, srcFileInfo.Mode().Perm()); err != nil {
			return fmt.Errorf("error creating directory %q: %w", destPath, err)
		}
	} else if srcFileInfo.Mode().Perm() != destFileInfo.Mode().Perm() {
		logboek.Context(ctx).Debug().LogF("Setting perms of already present dir %q to %s.\n", destPath, srcFileInfo.Mode().Perm())
		if err := os.Chmod(destPath, srcFileInfo.Mode().Perm()); err != nil {
			return fmt.Errorf("error changing permissions for %q to %s: %w", destPath, srcFileInfo.Mode().Perm(), err)
		}
	}

	if err := c.processDirOwnership(ctx, destPath, srcStat); err != nil {
		return fmt.Errorf("error processing dir ownership: %w", err)
	}
	return nil
}

func (c *CopyRecurse) copyFile(ctx context.Context, src string, srcFileInfo os.FileInfo, srcStat *syscall.Stat_t, dest string) error {
	logboek.Context(ctx).Debug().LogF("Going to copy file %q to %q with UID/GID %v/%v.\n", src, dest, uint32PtrPString(c.uid), uint32PtrPString(c.gid))

	logboek.Context(ctx).Debug().LogF("Opening source file %q.\n", src)
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("error opening file %q: %w", src, err)
	}
	defer srcFile.Close()

	_, err = os.Lstat(dest)
	if err == nil {
		logboek.Context(ctx).Debug().LogF("Removing path %q.\n", dest)
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("error removing path %q: %w", dest, err)
		}
	}

	logboek.Context(ctx).Debug().LogF("Creating destination file %q.\n", dest)
	destFile, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("error creating file %q: %w", dest, err)
	}
	defer destFile.Close()

	logboek.Context(ctx).Debug().LogF("Chmod destination file %q to %s.\n", dest, srcFileInfo.Mode().Perm())
	if err := destFile.Chmod(srcFileInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("error changing permissions for file %q to %s: %w", dest, srcFileInfo.Mode().Perm(), err)
	}

	if err := c.processFileOwnership(ctx, srcStat, destFile); err != nil {
		return fmt.Errorf("error processing file ownership: %w", err)
	}

	logboek.Context(ctx).Debug().LogF("Copying file contents from %q to %q.\n", src, dest)
	if _, err := io.Copy(destFile, srcFile); err != nil {
		return fmt.Errorf("error copying file from %q to %q: %w", src, dest, err)
	}

	return nil
}

func (c *CopyRecurse) copySymlink(ctx context.Context, src string, dest string) error {
	logboek.Context(ctx).Debug().LogF("Going to copy symlink %q to %q as is with UID/GID %v/%v.\n", src, dest, uint32PtrPString(c.uid), uint32PtrPString(c.gid))

	linkDestination, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("error reading symlink %q: %w", src, err)
	}

	logboek.Context(ctx).Debug().LogF("Removing path %q.\n", dest)
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("error removing path %q: %w", dest, err)
	}

	logboek.Context(ctx).Debug().LogF("Creating symlink from %q to %q.\n", dest, linkDestination)
	if err := os.Symlink(linkDestination, dest); err != nil {
		return fmt.Errorf("error creating symlink %q: %w", dest, err)
	}

	return nil
}

func (c *CopyRecurse) processFileOwnership(ctx context.Context, srcStat *syscall.Stat_t, destFile *os.File) error {
	logboek.Context(ctx).Debug().LogF("Processing file %q ownership.\n", destFile.Name())

	uid, gid := getNewUIDAndGID(c.uid, c.gid, srcStat)

	logboek.Context(ctx).Debug().LogF("Changing file %q ownership to %d/%d.\n", destFile.Name(), uid, gid)
	if err := destFile.Chown(uid, gid); err != nil {
		return fmt.Errorf("error changing ownership for %q: %w", destFile.Name(), err)
	}

	return nil
}

func (c *CopyRecurse) processDirOwnership(ctx context.Context, path string, srcStat *syscall.Stat_t) error {
	logboek.Context(ctx).Debug().LogF("Processing dir %q ownership.\n", path)

	uid, gid := getNewUIDAndGID(c.uid, c.gid, srcStat)

	logboek.Context(ctx).Debug().LogF("Changing dir %q ownership to %d/%d.\n", path, uid, gid)
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("error changing ownership for %q: %w", path, err)
	}

	return nil
}

func walkPath(ctx context.Context, path string, fn func(entryRelPath string, dirEntry *fs.DirEntry, err error) error) error {
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("error getting file info for path %q: %w", path, err)
	}

	if !fileInfo.IsDir() {
		entry := fs.FileInfoToDirEntry(fileInfo)
		logboek.Context(ctx).Debug().LogF("Executing walk function for file entry %q.\n", entry.Name())
		return fn(".", &entry, nil)
	} else {
		rootFs := os.DirFS(path)
		if err := fs.WalkDir(rootFs, ".", func(relSrc string, entry fs.DirEntry, err error) error {
			logboek.Context(ctx).Debug().LogF("Executing walk function for dir entry %q.\n", entry.Name())
			return fn(relSrc, &entry, err)
		}); err != nil {
			return fmt.Errorf("error walking directory %q: %w", rootFs, err)
		}
		return nil
	}
}

func getParentDir(path string) string {
	return filepath.Dir(filepath.Clean(path))
}

func getNewUIDAndGID(newDestUid, newDestGid *uint32, srcStat *syscall.Stat_t) (int, int) {
	var uid int
	if newDestUid != nil {
		uid = int(*newDestUid)
	} else {
		uid = int(srcStat.Uid)
	}

	var gid int
	if newDestGid != nil {
		gid = int(*newDestGid)
	} else {
		gid = int(srcStat.Gid)
	}

	return uid, gid
}

func uint32PtrPString(num *uint32) string {
	if num == nil {
		return "NIL"
	}

	return fmt.Sprintf("%d", *num)
}
