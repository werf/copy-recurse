//go:build !windows
// +build !windows

package copyrec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/werf/logboek"
)

func New(src, dest string, opts Options) (*CopyRecurse, error) {
	copyRec := &CopyRecurse{
		uid:                           opts.UID,
		gid:                           opts.GID,
		abortIfDestParentDirNotExists: opts.AbortIfDestParentDirNotExists,
		parallelism:                   opts.Parallelism,
	}

	if copyRec.parallelism < 1 {
		copyRec.parallelism = 1
	}

	// Cached once so per-file copy can decide whether an explicit fchmod/fchown
	// is required after O_CREAT. Umask read is racy against other goroutines
	// touching umask, but copy-recurse is the sole consumer in werf and callers
	// do not race with New().
	u := syscall.Umask(0)
	syscall.Umask(u)
	copyRec.processUmask = os.FileMode(u)
	copyRec.processUID = os.Geteuid()
	copyRec.processGID = os.Getegid()

	var err error
	copyRec.src, err = filepath.Abs(src)
	if err != nil {
		return nil, fmt.Errorf("error getting absolute path for src %q: %w", src, err)
	}

	copyRec.dest, err = filepath.Abs(dest)
	if err != nil {
		return nil, fmt.Errorf("error getting absolute path for dest %q: %w", dest, err)
	}

	copyRec.dest, err = dereferenceDestIfDir(copyRec.dest)
	if err != nil {
		return nil, fmt.Errorf("error dereferencing dest if directory: %w", err)
	}

	switch {
	case opts.MatchDir == nil && opts.MatchFile == nil:
		copyRec.matchDir = func(path string) (DirAction, error) {
			return DirMatch, nil
		}
		copyRec.matchFile = func(path string) (bool, error) {
			return true, nil
		}
	case opts.MatchDir == nil:
		copyRec.matchDir = func(path string) (DirAction, error) {
			return DirFallThrough, nil
		}
		copyRec.matchFile = opts.MatchFile
	case opts.MatchFile == nil:
		copyRec.matchDir = opts.MatchDir
		copyRec.matchFile = func(path string) (bool, error) {
			return true, nil
		}
	default:
		copyRec.matchDir = opts.MatchDir
		copyRec.matchFile = opts.MatchFile
	}

	return copyRec, nil
}

func (c *CopyRecurse) Run(ctx context.Context) error {
	if err := c.prepareDestParentDir(ctx); err != nil {
		return fmt.Errorf("error creating destination directory: %w", err)
	}

	c.startWorkers(ctx)

	walkErr := walkPath(ctx, c.src, func(relEntryPath string, dirEntry *fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error walking path: %w", err)
		}

		if e := c.workError(); e != nil {
			return e
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
	})

	workErr := c.stopWorkers()

	if walkErr != nil {
		return fmt.Errorf("error walking path: %w", walkErr)
	}
	return workErr
}

func (c *CopyRecurse) startWorkers(ctx context.Context) {
	if c.parallelism <= 1 {
		return
	}
	c.workCtx, c.workCancel = context.WithCancel(ctx)
	c.workCh = make(chan copyJob, c.parallelism*4)
	for i := 0; i < c.parallelism; i++ {
		c.workWG.Add(1)
		go c.workerLoop()
	}
}

func (c *CopyRecurse) stopWorkers() error {
	if c.workCh == nil {
		return nil
	}
	close(c.workCh)
	c.workWG.Wait()
	c.workCancel()
	c.workCh = nil
	return c.firstErr
}

func (c *CopyRecurse) workerLoop() {
	defer c.workWG.Done()
	for job := range c.workCh {
		if c.workError() != nil {
			continue
		}
		if err := c.runJob(c.workCtx, job); err != nil {
			c.recordErr(err)
		}
	}
}

func (c *CopyRecurse) runJob(ctx context.Context, job copyJob) error {
	switch job.kind {
	case copyJobFile:
		return c.copyFile(ctx, job.src, job.srcFileInfo, statOf(job.srcFileInfo), job.dest)
	case copyJobSymlink:
		return c.copySymlink(ctx, job.src, job.dest)
	default:
		return fmt.Errorf("unexpected copy job kind %d", job.kind)
	}
}

func (c *CopyRecurse) recordErr(err error) {
	c.errMu.Lock()
	if c.firstErr == nil {
		c.firstErr = err
		if c.workCancel != nil {
			c.workCancel()
		}
	}
	c.errMu.Unlock()
}

func (c *CopyRecurse) workError() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.firstErr
}

func (c *CopyRecurse) enqueueOrRunFile(ctx context.Context, src string, srcFileInfo os.FileInfo, srcStat *syscall.Stat_t, dest string) error {
	if c.workCh == nil {
		return c.copyFile(ctx, src, srcFileInfo, srcStat, dest)
	}
	if e := c.workError(); e != nil {
		return e
	}
	select {
	case <-c.workCtx.Done():
		if e := c.workError(); e != nil {
			return e
		}
		return c.workCtx.Err()
	case c.workCh <- copyJob{kind: copyJobFile, src: src, dest: dest, srcFileInfo: srcFileInfo}:
		return nil
	}
}

func (c *CopyRecurse) enqueueOrRunSymlink(ctx context.Context, src, dest string) error {
	if c.workCh == nil {
		return c.copySymlink(ctx, src, dest)
	}
	if e := c.workError(); e != nil {
		return e
	}
	select {
	case <-c.workCtx.Done():
		if e := c.workError(); e != nil {
			return e
		}
		return c.workCtx.Err()
	case c.workCh <- copyJob{kind: copyJobSymlink, src: src, dest: dest}:
		return nil
	}
}

func statOf(fi os.FileInfo) *syscall.Stat_t {
	if fi == nil {
		return nil
	}
	st, _ := fi.Sys().(*syscall.Stat_t)
	return st
}

func (c *CopyRecurse) prepareDestParentDir(ctx context.Context) error {
	logboek.Context(ctx).Debug().LogF("Preparing parent dir for destination %q.\n", c.dest)

	destParentDir := getParentDir(c.dest)
	if fileInfo, err := os.Lstat(destParentDir); errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		if c.abortIfDestParentDirNotExists {
			return fmt.Errorf("directory %q does not exist", destParentDir)
		}

		logboek.Context(ctx).Debug().LogF("Creating destination parent dir (and its parents) at %q.\n", destParentDir)
		if err := os.MkdirAll(destParentDir, os.ModePerm); err != nil {
			return fmt.Errorf("error creating directories up to parent destination directory %q: %w", destParentDir, err)
		}
	} else if err != nil {
		return fmt.Errorf("error getting file info about parent destination directory %q: %w", destParentDir, err)
	} else if !fileInfo.IsDir() && fileInfo.Mode()&os.ModeSymlink == 0 {
		if err := c.recreateParentDir(ctx, destParentDir); err != nil {
			return fmt.Errorf("error recreating parent dir: %w", err)
		}
	} else if fileInfo.Mode()&os.ModeSymlink != 0 {
		if dereferencedDestParentDir, err := os.Stat(destParentDir); errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			if err := c.recreateParentDir(ctx, destParentDir); err != nil {
				return fmt.Errorf("error recreating parent dir: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("error getting dereferenced file info for destination parent dir %q: %w", destParentDir, err)
		} else if !dereferencedDestParentDir.IsDir() {
			if err := c.recreateParentDir(ctx, destParentDir); err != nil {
				return fmt.Errorf("error recreating parent dir: %w", err)
			}
		}
	}

	return nil
}

func (c *CopyRecurse) recreateParentDir(ctx context.Context, destParentDir string) error {
	if c.abortIfDestParentDirNotExists {
		return fmt.Errorf("something is in place of a destination parent dir %q", destParentDir)
	}

	logboek.Context(ctx).Debug().LogF("Removing file in place of a destination parent dir %q.\n", destParentDir)
	if err := os.RemoveAll(destParentDir); err != nil {
		return fmt.Errorf("error removing file in place of a destination parent dir %q: %w", destParentDir, err)
	}

	logboek.Context(ctx).Debug().LogF("Creating destination parent dir (and its parents) at %q.\n", destParentDir)
	if err := os.MkdirAll(destParentDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating directories up to parent destination directory %q: %w", destParentDir, err)
	}

	return nil
}

func (c *CopyRecurse) processFile(ctx context.Context, src, dest string) error {
	logboek.Context(ctx).Debug().LogF("Processing file %q.\n", src)

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

	if filepath.Clean(src) == c.src {
		logboek.Context(ctx).Debug().LogF("Will look for matches in directory %q.\n", src)
		return nil
	}

	action, err := c.matchDir(src)
	if err != nil {
		return fmt.Errorf("error matching directory %q: %w", src, err)
	}

	switch action {
	case DirMatch:
		logboek.Context(ctx).Debug().LogF("Dir %q fully matched.\n", src)
		if err := c.copyRecurse(ctx, src, dest); err != nil {
			return fmt.Errorf("error copying directory: %w", err)
		}
		return nil
	case DirFallThrough:
		logboek.Context(ctx).Debug().LogF("Will look for matches in directory %q.\n", src)
		return nil
	case DirSkip:
		logboek.Context(ctx).Debug().LogF("Skipping directory %q.\n", src)
		return fs.SkipDir
	default:
		panic(fmt.Sprintf("unexpected action (int %d)", action))
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

			if werr := c.workError(); werr != nil {
				return werr
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
			case srcEntryFileInfo.Mode().IsRegular():
				if err := c.createEmptyDirsChain(ctx, getParentDir(absEntryDestPath)); err != nil {
					return fmt.Errorf("error creating empty dirs chain: %w", err)
				}

				if err := c.enqueueOrRunFile(ctx, absEntrySrcPath, srcEntryFileInfo, statOf(srcEntryFileInfo), absEntryDestPath); err != nil {
					return fmt.Errorf("error copying file: %w", err)
				}
			case srcEntryFileInfo.Mode()&os.ModeSymlink != 0:
				if err := c.createEmptyDirsChain(ctx, getParentDir(absEntryDestPath)); err != nil {
					return fmt.Errorf("error creating empty dirs chain: %w", err)
				}

				if err := c.enqueueOrRunSymlink(ctx, absEntrySrcPath, absEntryDestPath); err != nil {
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

		if dest != c.dest {
			if err := c.createEmptyDirsChain(ctx, getParentDir(dest)); err != nil {
				return fmt.Errorf("error creating empty dirs chain: %w", err)
			}
		}

		if err := c.enqueueOrRunFile(ctx, src, srcFileInfo, srcStat, dest); err != nil {
			return fmt.Errorf("error copying file: %w", err)
		}
	case srcFileInfo.Mode()&os.ModeSymlink != 0:
		if dest != c.dest {
			if err := c.createEmptyDirsChain(ctx, getParentDir(dest)); err != nil {
				return fmt.Errorf("error creating empty dirs chain: %w", err)
			}
		}

		if err := c.enqueueOrRunSymlink(ctx, src, dest); err != nil {
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

	dirsToVisit := []string{c.dest}

	if strings.HasPrefix(destPath, c.dest) && strings.TrimPrefix(filepath.Clean(destPath), c.dest) != "" {
		relDestPath, err := filepath.Rel(c.dest, destPath)
		if err != nil {
			return fmt.Errorf("error calculating relative path for base %q and target %q: %w", c.dest, destPath, err)
		}

		relDestPathParts := strings.Split(relDestPath, string(filepath.Separator))
		for i := 0; i < len(relDestPathParts); i++ {
			dirsToVisit = append([]string{filepath.Join(c.dest, filepath.Join(relDestPathParts[:i+1]...))}, dirsToVisit...)
		}
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
				dirsToVisit = dirsToVisit[:i+1]
				break
			}
		}
	}

	sort.Slice(dirsToVisit, func(i, j int) bool { return i > j })

	for _, dir := range dirsToVisit {
		if err := c.createEmptyDirInChain(ctx, dir); err != nil {
			return fmt.Errorf("error creating empty dir %q: %w", destPath, err)
		}
	}

	c.visitedDestDirs = append(c.visitedDestDirs, dirsToVisit...)

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

	perm := srcFileInfo.Mode().Perm()

	var existingStat *syscall.Stat_t
	freshlyCreated := false

	destFileInfo, err := os.Lstat(destPath)
	if errors.Is(err, os.ErrNotExist) {
		logboek.Context(ctx).Debug().LogF("Creating dir %q with perms %s.\n", destPath, perm)
		if err := os.Mkdir(destPath, perm); err != nil {
			return fmt.Errorf("error creating directory %q: %w", destPath, err)
		}
		freshlyCreated = true
		if perm&c.processUmask != 0 {
			if err := os.Chmod(destPath, perm); err != nil {
				return fmt.Errorf("error changing permissions for %q to %s: %w", destPath, perm, err)
			}
		}
	} else if err != nil {
		return fmt.Errorf("can't get file info for %q: %w", destPath, err)
	} else if !destFileInfo.IsDir() {
		logboek.Context(ctx).Debug().LogF("Removing path %q.\n", destPath)
		if err := os.RemoveAll(destPath); err != nil {
			return fmt.Errorf("error removing path %q: %w", destPath, err)
		}

		logboek.Context(ctx).Debug().LogF("Creating dir %q with perms %s.\n", destPath, perm)
		if err := os.Mkdir(destPath, perm); err != nil {
			return fmt.Errorf("error creating directory %q: %w", destPath, err)
		}
		freshlyCreated = true
		if perm&c.processUmask != 0 {
			if err := os.Chmod(destPath, perm); err != nil {
				return fmt.Errorf("error changing permissions for %q to %s: %w", destPath, perm, err)
			}
		}
	} else {
		existingStat, _ = destFileInfo.Sys().(*syscall.Stat_t)
		if destFileInfo.Mode().Perm() != perm {
			logboek.Context(ctx).Debug().LogF("Setting perms of already present dir %q to %s.\n", destPath, perm)
			if err := os.Chmod(destPath, perm); err != nil {
				return fmt.Errorf("error changing permissions for %q to %s: %w", destPath, perm, err)
			}
		}
	}

	if err := c.processDirOwnership(ctx, destPath, srcStat, existingStat, freshlyCreated); err != nil {
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

	var existingPerm os.FileMode
	var existingStat *syscall.Stat_t
	destExisted := false

	if fi, err := os.Lstat(dest); err == nil {
		if !fi.Mode().IsRegular() {
			logboek.Context(ctx).Debug().LogF("Removing non-regular path %q.\n", dest)
			if err := os.RemoveAll(dest); err != nil {
				return fmt.Errorf("error removing path %q: %w", dest, err)
			}
		} else {
			destExisted = true
			existingPerm = fi.Mode().Perm()
			existingStat, _ = fi.Sys().(*syscall.Stat_t)
		}
	}

	perm := srcFileInfo.Mode().Perm()

	logboek.Context(ctx).Debug().LogF("Creating destination file %q with perms %s.\n", dest, perm)
	destFile, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("error creating file %q: %w", dest, err)
	}
	defer destFile.Close()

	needChmod := false
	if destExisted {
		needChmod = existingPerm != perm
	} else {
		needChmod = perm&c.processUmask != 0
	}
	if needChmod {
		logboek.Context(ctx).Debug().LogF("Chmod destination file %q to %s.\n", dest, perm)
		if err := destFile.Chmod(perm); err != nil {
			return fmt.Errorf("error changing permissions for file %q to %s: %w", dest, perm, err)
		}
	}

	if err := c.processFileOwnership(ctx, srcStat, destFile, existingStat); err != nil {
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

func (c *CopyRecurse) processFileOwnership(ctx context.Context, srcStat *syscall.Stat_t, destFile *os.File, existingStat *syscall.Stat_t) error {
	logboek.Context(ctx).Debug().LogF("Processing file %q ownership.\n", destFile.Name())

	targetUID, targetGID := getNewUIDAndGID(c.uid, c.gid, srcStat)
	currentUID, currentGID := currentOwnership(existingStat, c.processUID, c.processGID)

	if targetUID == currentUID && targetGID == currentGID {
		return nil
	}

	logboek.Context(ctx).Debug().LogF("Changing file %q ownership to %d/%d.\n", destFile.Name(), targetUID, targetGID)
	if err := destFile.Chown(targetUID, targetGID); err != nil {
		return fmt.Errorf("error changing ownership for %q: %w", destFile.Name(), err)
	}

	return nil
}

func (c *CopyRecurse) processDirOwnership(ctx context.Context, path string, srcStat, existingStat *syscall.Stat_t, freshlyCreated bool) error {
	logboek.Context(ctx).Debug().LogF("Processing dir %q ownership.\n", path)

	targetUID, targetGID := getNewUIDAndGID(c.uid, c.gid, srcStat)
	var currentUID, currentGID int
	if freshlyCreated {
		currentUID, currentGID = c.processUID, c.processGID
	} else {
		currentUID, currentGID = currentOwnership(existingStat, c.processUID, c.processGID)
	}

	if targetUID == currentUID && targetGID == currentGID {
		return nil
	}

	logboek.Context(ctx).Debug().LogF("Changing dir %q ownership to %d/%d.\n", path, targetUID, targetGID)
	if err := os.Lchown(path, targetUID, targetGID); err != nil {
		return fmt.Errorf("error changing ownership for %q: %w", path, err)
	}

	return nil
}

func currentOwnership(st *syscall.Stat_t, fallbackUID, fallbackGID int) (int, int) {
	if st == nil {
		return fallbackUID, fallbackGID
	}
	return int(st.Uid), int(st.Gid)
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

func dereferenceDestIfDir(dest string) (string, error) {
	newDest := dest

	destFileInfo, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return newDest, nil
	} else if err != nil {
		return "", fmt.Errorf("error getting file info for file %q: %w", dest, err)
	}

	if destFileInfo.Mode()&os.ModeSymlink != 0 {
		if dereferencedFileInfo, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return newDest, nil
		} else if err != nil {
			return "", fmt.Errorf("error getting dereferencing file info for %q: %w", dest, err)
		} else if dereferencedFileInfo.IsDir() {
			newDest, err = filepath.EvalSymlinks(dest)
			if err != nil {
				return "", fmt.Errorf("error resolving symlink at %q: %w", dest, err)
			}
		}
	}

	return newDest, nil
}
