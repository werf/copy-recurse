package copyrec_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	copyrec "github.com/werf/copy-recurse"
	"github.com/werf/logboek"
)

func benchCtx() context.Context {
	return logboek.NewContext(context.Background(), logboek.DefaultLogger())
}

type treeSpec struct {
	name        string
	dirs        int
	filesPerDir int
	fileSize    int
}

var benchTrees = []treeSpec{
	{"small_500x1KiB", 25, 20, 1024},
	{"medium_5000x4KiB", 50, 100, 4 * 1024},
	{"large_20000x1KiB", 100, 200, 1024},
}

func mkTree(tb testing.TB, root string, spec treeSpec) {
	tb.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		tb.Fatal(err)
	}
	for d := 0; d < spec.dirs; d++ {
		dp := filepath.Join(root, fmt.Sprintf("d%04d", d))
		if err := os.Mkdir(dp, 0o755); err != nil {
			tb.Fatal(err)
		}
		for f := 0; f < spec.filesPerDir; f++ {
			buf := make([]byte, spec.fileSize)
			if _, err := rand.Read(buf); err != nil {
				tb.Fatal(err)
			}
			perm := os.FileMode(0o644)
			if f%3 == 0 {
				perm = 0o755
			}
			if err := os.WriteFile(filepath.Join(dp, fmt.Sprintf("f%04d", f)), buf, perm); err != nil {
				tb.Fatal(err)
			}
		}
	}
}

func benchOne(b *testing.B, spec treeSpec, parallelism int) {
	src := b.TempDir()
	mkTree(b, src, spec)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dest := b.TempDir()
		b.StartTimer()

		cr, err := copyrec.New(src, dest, copyrec.Options{Parallelism: parallelism})
		if err != nil {
			b.Fatal(err)
		}
		if err := cr.Run(benchCtx()); err != nil {
			b.Fatal(err)
		}
	}
	totalFiles := spec.dirs * spec.filesPerDir
	b.ReportMetric(float64(totalFiles), "files/op")
	b.ReportMetric(float64(totalFiles*spec.fileSize)/1024/1024, "MiB/op")
}

func BenchmarkCopyRecurse(b *testing.B) {
	for _, spec := range benchTrees {
		for _, p := range []int{1, 4, 8, 16} {
			b.Run(fmt.Sprintf("%s/p=%d", spec.name, p), func(b *testing.B) {
				benchOne(b, spec, p)
			})
		}
	}
}
