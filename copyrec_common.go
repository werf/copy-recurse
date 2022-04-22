package copyrec

type DirAction int

const (
	DirMatch DirAction = iota
	DirFallThrough
	DirSkip
)

type Options struct {
	// Set UID for copied files/directories.
	UID *uint32

	// Set GID for copied files/directories.
	GID *uint32

	// Function decides should we match a directory while walking, fall through it to continue searching for matches or skip it.
	// If not defined, but matchFile is defined, then it always returns DirFallThrough.
	// If not defined and matchFile is undefined, then it always returns DirMatch.
	MatchDir func(path string) (DirAction, error)

	// Function decides whether should we match a file while walking.
	// If not defined, then it always returns true.
	MatchFile func(path string) (bool, error)

	AbortIfDestParentDirNotExists bool
}

type CopyRecurse struct {
	src  string
	dest string
	uid  *uint32
	gid  *uint32

	matchDir  func(path string) (DirAction, error)
	matchFile func(path string) (bool, error)

	abortIfDestParentDirNotExists bool

	// TODO: how memory/CPU-effective is working with this?
	visitedDestDirs []string
}
