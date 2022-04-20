package copyrec_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/werf/copy-recurse"
)

type (
	CreateSourceFilesFunc func(config CopyRecurseTestConfig)
	ExpectedFunc          func(config CopyRecurseTestConfig)
)

type CopyRecurseTestConfig struct {
	CopyRecurseOptions copyrec.Options
	CreateFilesFunc    CreateSourceFilesFunc
	ExpectedFunc       ExpectedFunc
}

var _ = Describe("CopyRecurse", func() {
	var tmpRoot, tmpSrc, tmpDest string
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()

		var err error
		tmpRoot, err = os.MkdirTemp("", "*-copyrec-test")
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(os.RemoveAll, tmpRoot)

		tmpSrc = filepath.Join(tmpRoot, "src")
		tmpDest = filepath.Join(tmpRoot, "dest")

		Expect(os.Mkdir(tmpSrc, 0o755)).To(Succeed())
		Expect(os.Mkdir(tmpDest, 0o755)).To(Succeed())
	})

	DescribeTable("should succeed and",
		func(config CopyRecurseTestConfig) {
			config.CreateFilesFunc(config)

			copyRec, err := copyrec.New(tmpSrc, tmpDest, config.CopyRecurseOptions)
			Expect(err).ToNot(HaveOccurred())

			Expect(copyRec.Run(ctx)).To(Succeed())

			config.ExpectedFunc(config)
		},
		Entry("copy file with correct mode",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.WriteFile(filepath.Join(tmpSrc, "file"), []byte("content"), os.FileMode(0o754))).To(Succeed())
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					fi, _ := getFileInfoAndStat(filepath.Join(tmpDest, "file"))
					Expect(fi.Mode().String()).To(Equal(os.FileMode(0o754).String()))

					Expect(getFileContent(filepath.Join(tmpDest, "file"))).To(Equal("content"))
				},
			},
		),
		Entry("copy symlink unchanged",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.Symlink("somewhere", filepath.Join(tmpSrc, "symlink"))).To(Succeed())
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					fi, _ := getFileInfoAndStat(filepath.Join(tmpDest, "symlink"))
					Expect(fi.Mode().Type() & os.ModeSymlink).ToNot(Equal(0))
				},
			},
		),
		Entry("copy empty directory with correct mode",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.Mkdir(filepath.Join(tmpSrc, "subdir"), 0o741)).To(Succeed())
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					fi, _ := getFileInfoAndStat(filepath.Join(tmpDest, "subdir"))
					Expect(fi.Mode().String()).To(Equal(os.FileMode(0o741 | os.ModeDir).String()))
				},
			},
		),
		Entry("copy nothing when nothing is in source",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc:    func(config CopyRecurseTestConfig) {},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					files, err := os.ReadDir(tmpDest)
					Expect(err).ToNot(HaveOccurred())
					Expect(files).To(BeEmpty())
				},
			},
		),
		Entry("skip unsupported file",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(syscall.Mkfifo(filepath.Join(tmpSrc, "file"), uint32(os.ModePerm))).To(Succeed())
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					Expect(filepath.Join(tmpDest, "file")).ToNot(BeAnExistingFile())
				},
			},
		),
		Entry("copy nested directories with file",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.MkdirAll(filepath.Join(tmpSrc, "subdir", "subsubdir"), 0o741)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(tmpSrc, "subdir", "subsubdir", "file"), []byte("content"), os.FileMode(0o754))).To(Succeed())
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					fi, _ := getFileInfoAndStat(filepath.Join(tmpDest, "subdir"))
					Expect(fi.Mode().String()).To(Equal(os.FileMode(0o741 | os.ModeDir).String()))

					fi, _ = getFileInfoAndStat(filepath.Join(tmpDest, "subdir", "subsubdir"))
					Expect(fi.Mode().String()).To(Equal(os.FileMode(0o741 | os.ModeDir).String()))

					fi, _ = getFileInfoAndStat(filepath.Join(tmpDest, "subdir", "subsubdir", "file"))
					Expect(fi.Mode().String()).To(Equal(os.FileMode(0o754).String()))

					Expect(getFileContent(filepath.Join(tmpDest, "subdir", "subsubdir", "file"))).To(Equal("content"))
				},
			},
		),
		Entry("copy only matching empty directory",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{
					MatchDir: func(path string) (copyrec.DirAction, error) {
						if filepath.Base(path) == "matched-subdir" {
							return copyrec.DirMatch, nil
						} else {
							return copyrec.DirFallThrough, nil
						}
					},
				},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.Mkdir(filepath.Join(tmpSrc, "matched-subdir"), os.ModePerm)).To(Succeed())
					Expect(os.Mkdir(filepath.Join(tmpSrc, "unmatched-subdir"), os.ModePerm)).To(Succeed())
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					Expect(filepath.Join(tmpDest, "matched-subdir")).To(BeADirectory())
					Expect(filepath.Join(tmpDest, "unmatched-subdir")).ToNot(BeAnExistingFile())
				},
			},
		),
		Entry("copy only matching file",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{
					MatchFile: func(path string) (bool, error) {
						return filepath.Base(path) == "matched-file", nil
					},
				},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					touchFile(filepath.Join(tmpSrc, "matched-file"))
					touchFile(filepath.Join(tmpSrc, "unmatched-file"))
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					Expect(filepath.Join(tmpDest, "matched-file")).To(BeAnExistingFile())
					Expect(filepath.Join(tmpDest, "unmatched-file")).ToNot(BeAnExistingFile())
				},
			},
		),
		Entry("copy only matching directory with file",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{
					MatchFile: func(path string) (bool, error) {
						return filepath.Base(path) == "matched-file", nil
					},
					MatchDir: func(path string) (copyrec.DirAction, error) {
						if filepath.Base(filepath.Clean(path)) == "matched-subdir" {
							return copyrec.DirFallThrough, nil
						} else {
							return copyrec.DirSkip, nil
						}
					},
				},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.Mkdir(filepath.Join(tmpSrc, "matched-subdir"), os.ModePerm)).To(Succeed())
					touchFile(filepath.Join(tmpSrc, "matched-subdir", "matched-file"))

					Expect(os.Mkdir(filepath.Join(tmpSrc, "unmatched-subdir"), os.ModePerm)).To(Succeed())
					touchFile(filepath.Join(tmpSrc, "unmatched-subdir", "matched-file"))
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					Expect(filepath.Join(tmpDest, "matched-subdir", "matched-file")).To(BeAnExistingFile())
					Expect(filepath.Join(tmpDest, "unmatched-subdir")).ToNot(BeAnExistingFile())
				},
			},
		),
		Entry("copy file with custom uid/gid",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{
					// User not allowed to set UID to other than his own on *nix.
					UID: intToUint32Ptr(os.Getuid()),
					// User not allowed to set GID to other than one of his own groups on *nix.
					GID: intToUint32Ptr(getFirstUserGroupSortedNumerically()),
				},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					touchFile(filepath.Join(tmpSrc, "file"))
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					_, stat := getFileInfoAndStat(filepath.Join(tmpDest, "file"))
					Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
					Expect(stat.Gid).To(Equal(uint32(getFirstUserGroupSortedNumerically())))
				},
			},
		),
		Entry("merge directories",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.Mkdir(filepath.Join(tmpSrc, "subdir"), os.ModePerm)).To(Succeed())
					touchFile(filepath.Join(tmpSrc, "subdir", "file1"))
					touchFile(filepath.Join(tmpSrc, "subdir", "file2"))

					Expect(os.Mkdir(filepath.Join(tmpDest, "subdir"), os.ModePerm)).To(Succeed())
					touchFile(filepath.Join(tmpDest, "subdir", "file2"))
					touchFile(filepath.Join(tmpDest, "subdir", "file3"))
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					Expect(filepath.Join(tmpDest, "subdir", "file1")).To(BeAnExistingFile())
					Expect(filepath.Join(tmpDest, "subdir", "file2")).To(BeAnExistingFile())
					Expect(filepath.Join(tmpDest, "subdir", "file3")).To(BeAnExistingFile())
				},
			},
		),
		Entry("replace existing files of different types",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.Mkdir(filepath.Join(tmpSrc, "subdir"), os.ModePerm)).To(Succeed())
					touchFile(filepath.Join(tmpSrc, "file"))

					touchFile(filepath.Join(tmpDest, "subdir"))
					Expect(os.Mkdir(filepath.Join(tmpDest, "file"), os.ModePerm)).To(Succeed())
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					Expect(filepath.Join(tmpDest, "subdir")).To(BeADirectory())
					Expect(filepath.Join(tmpDest, "file")).To(BeARegularFile())
				},
			},
		),
		Entry("complex test, first debug smaller tests if they broke too",
			CopyRecurseTestConfig{
				CopyRecurseOptions: copyrec.Options{
					MatchDir: func(path string) (copyrec.DirAction, error) {
						return copyrec.DirFallThrough, nil
					},
					MatchFile: func(path string) (bool, error) {
						return filepath.Base(filepath.Clean(path)) != "notincluded", nil
					},
					// User not allowed to set UID to other than his own on *nix.
					UID: intToUint32Ptr(os.Getuid()),
					// User not allowed to set GID to other than one of his own groups on *nix.
					GID: intToUint32Ptr(getFirstUserGroupSortedNumerically()),
				},
				CreateFilesFunc: func(config CopyRecurseTestConfig) {
					Expect(os.MkdirAll(filepath.Join(tmpSrc, "sd", "sd", "sd"), 0o750)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(tmpSrc, "sd", "sd", "file1"), []byte("content1"), 0o754)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(tmpSrc, "sd", "sd", "file2"), []byte("content2"), 0o754)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(tmpSrc, "sd", "sd", "sd", "file3"), []byte("content3"), 0o745)).To(Succeed())
					Expect(os.Mkdir(filepath.Join(tmpSrc, "sd", "sd", "emptydir1"), 0o754)).To(Succeed())
					Expect(os.Symlink("somewhere", filepath.Join(tmpSrc, "sd", "sd", "sd", "symlink"))).To(Succeed())
					Expect(syscall.Mkfifo(filepath.Join(tmpSrc, "sd", "sd", "unsupported"), uint32(os.ModePerm))).To(Succeed())
					Expect(os.MkdirAll(filepath.Join(tmpSrc, "sd2", "sd", "sd"), 0o750)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(tmpSrc, "sd2", "sd", "file5"), []byte("content5"), 0o754)).To(Succeed())
					touchFile(filepath.Join(tmpSrc, "sd2", "sd", "sd", "notincluded"))

					Expect(os.MkdirAll(filepath.Join(tmpDest, "sd", "sd"), 0o754)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(tmpDest, "sd", "sd", "file2"), []byte("oldcontent2"), 0o745)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(tmpDest, "sd", "sd", "file4"), []byte("oldcontent4"), 0o745)).To(Succeed())
					Expect(os.Mkdir(filepath.Join(tmpDest, "sd", "emptydir2"), 0o744)).To(Succeed())
					touchFile(filepath.Join(tmpDest, "sd2"))
				},
				ExpectedFunc: func(config CopyRecurseTestConfig) {
					subDirParts := []string{"sd", "sd", "sd"}
					for i := 0; i < len(subDirParts); i++ {
						fi, stat := getFileInfoAndStat(filepath.Join(tmpDest, filepath.Join(subDirParts[:i+1]...)))
						Expect(fi.IsDir()).To(BeTrue())
						Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o750).String()))
						Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
						Expect(stat.Gid).To(Equal(uint32(getFirstUserGroupSortedNumerically())))
					}

					fi, stat := getFileInfoAndStat(filepath.Join(tmpDest, "sd", "sd", "file1"))
					Expect(fi.Mode().IsRegular()).To(BeTrue())
					Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o754).String()))
					Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
					Expect(stat.Gid).To(Equal(uint32(getFirstUserGroupSortedNumerically())))
					Expect(getFileContent(filepath.Join(tmpDest, "sd", "sd", "file1"))).To(Equal("content1"))

					fi, stat = getFileInfoAndStat(filepath.Join(tmpDest, "sd", "sd", "file2"))
					Expect(fi.Mode().IsRegular()).To(BeTrue())
					Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o754).String()))
					Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
					Expect(stat.Gid).To(Equal(uint32(getFirstUserGroupSortedNumerically())))
					Expect(getFileContent(filepath.Join(tmpDest, "sd", "sd", "file2"))).To(Equal("content2"))

					fi, stat = getFileInfoAndStat(filepath.Join(tmpDest, "sd", "sd", "sd", "file3"))
					Expect(fi.Mode().IsRegular()).To(BeTrue())
					Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o745).String()))
					Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
					Expect(stat.Gid).To(Equal(uint32(getFirstUserGroupSortedNumerically())))
					Expect(getFileContent(filepath.Join(tmpDest, "sd", "sd", "sd", "file3"))).To(Equal("content3"))

					Expect(filepath.Join(tmpDest, "sd", "sd", "emptydir1")).ToNot(BeAnExistingFile())

					getFileInfoAndStat(filepath.Join(tmpDest, "sd", "sd", "sd", "symlink"))

					Expect(filepath.Join(tmpDest, "sd", "sd", "unsupported")).ToNot(BeAnExistingFile())

					subDirParts = []string{"sd2", "sd"}
					for i := 0; i < len(subDirParts); i++ {
						fi, stat := getFileInfoAndStat(filepath.Join(tmpDest, filepath.Join(subDirParts[:i+1]...)))
						Expect(fi.IsDir()).To(BeTrue())
						Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o750).String()))
						Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
						Expect(stat.Gid).To(Equal(uint32(getFirstUserGroupSortedNumerically())))
					}

					fi, stat = getFileInfoAndStat(filepath.Join(tmpDest, "sd2", "sd", "file5"))
					Expect(fi.Mode().IsRegular()).To(BeTrue())
					Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o754).String()))
					Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
					Expect(stat.Gid).To(Equal(uint32(getFirstUserGroupSortedNumerically())))
					Expect(getFileContent(filepath.Join(tmpDest, "sd2", "sd", "file5"))).To(Equal("content5"))

					Expect(filepath.Join(tmpDest, "sd2", "sd", "sd")).ToNot(BeAnExistingFile())

					Expect(filepath.Join(tmpDest, "sd2", "sd", "sd", "notincluded")).ToNot(BeAnExistingFile())

					fi, stat = getFileInfoAndStat(filepath.Join(tmpDest, "sd", "sd", "file4"))
					Expect(fi.Mode().IsRegular()).To(BeTrue())
					Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o745).String()))
					Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
					Expect(stat.Gid).To(Equal(uint32(os.Getgid())))
					Expect(getFileContent(filepath.Join(tmpDest, "sd", "sd", "file4"))).To(Equal("oldcontent4"))

					fi, stat = getFileInfoAndStat(filepath.Join(tmpDest, "sd", "emptydir2"))
					Expect(fi.Mode().IsDir()).To(BeTrue())
					Expect(fi.Mode().Perm().String()).To(Equal(os.FileMode(0o744).String()))
					Expect(stat.Uid).To(Equal(uint32(os.Getuid())))
					Expect(stat.Gid).To(Equal(uint32(os.Getgid())))
				},
			},
		),
	)
})

func intToUint32Ptr(n int) *uint32 {
	converted := uint32(n)
	return &converted
}

func getFirstUserGroupSortedNumerically() int {
	groups, err := os.Getgroups()
	Expect(err).ToNot(HaveOccurred())
	return groups[0]
}

func touchFile(path string) {
	_, err := os.Create(path)
	Expect(err).ToNot(HaveOccurred())
}

func getFileInfoAndStat(path string) (os.FileInfo, *syscall.Stat_t) {
	fileInfo, err := os.Lstat(path)
	Expect(err).ToNot(HaveOccurred())

	return fileInfo, fileInfo.Sys().(*syscall.Stat_t)
}

func getFileContent(path string) string {
	content, err := os.ReadFile(path)
	Expect(err).ToNot(HaveOccurred())
	return string(content)
}
