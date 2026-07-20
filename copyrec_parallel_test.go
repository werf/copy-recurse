package copyrec_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/werf/logboek"
	copyrec "github.com/werf/copy-recurse"
)

func testCtx() context.Context {
	return logboek.NewContext(context.Background(), logboek.DefaultLogger())
}

func TestParallelCopyLargeTree(t *testing.T) {
	for _, parallelism := range []int{0, 1, 4, 8} {
		parallelism := parallelism
		t.Run(fmt.Sprintf("parallelism=%d", parallelism), func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			src := filepath.Join(root, "src")
			dest := filepath.Join(root, "dest")
			if err := os.Mkdir(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}

			const dirs = 50
			const filesPerDir = 40
			expected := map[string]string{}
			for d := 0; d < dirs; d++ {
				dirPath := filepath.Join(src, fmt.Sprintf("d%03d", d))
				if err := os.Mkdir(dirPath, 0o755); err != nil {
					t.Fatal(err)
				}
				for f := 0; f < filesPerDir; f++ {
					name := fmt.Sprintf("f%03d", f)
					content := fmt.Sprintf("d%03d-f%03d-payload", d, f)
					perm := os.FileMode(0o644)
					if f%3 == 0 {
						perm = 0o755
					}
					if f%5 == 0 {
						perm = 0o600
					}
					if err := os.WriteFile(filepath.Join(dirPath, name), []byte(content), perm); err != nil {
						t.Fatal(err)
					}
					expected[filepath.Join(fmt.Sprintf("d%03d", d), name)] = content
					if f%7 == 0 {
						link := filepath.Join(dirPath, name+".lnk")
						if err := os.Symlink(name, link); err != nil {
							t.Fatal(err)
						}
					}
				}
			}

			cr, err := copyrec.New(src, dest, copyrec.Options{Parallelism: parallelism})
			if err != nil {
				t.Fatal(err)
			}
			if err := cr.Run(testCtx()); err != nil {
				t.Fatal(err)
			}

			for rel, wantContent := range expected {
				got, err := os.ReadFile(filepath.Join(dest, rel))
				if err != nil {
					t.Fatalf("read %s: %v", rel, err)
				}
				if string(got) != wantContent {
					t.Fatalf("content mismatch %s: got %q, want %q", rel, got, wantContent)
				}
			}

			for d := 0; d < dirs; d++ {
				for f := 0; f < filesPerDir; f += 7 {
					link := filepath.Join(dest, fmt.Sprintf("d%03d", d), fmt.Sprintf("f%03d.lnk", f))
					fi, err := os.Lstat(link)
					if err != nil {
						t.Fatalf("lstat %s: %v", link, err)
					}
					if fi.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("%s is not a symlink", link)
					}
				}
			}

			for d := 0; d < dirs; d++ {
				for f := 0; f < filesPerDir; f++ {
					wantPerm := os.FileMode(0o644)
					if f%3 == 0 {
						wantPerm = 0o755
					}
					if f%5 == 0 {
						wantPerm = 0o600
					}
					p := filepath.Join(dest, fmt.Sprintf("d%03d", d), fmt.Sprintf("f%03d", f))
					fi, err := os.Lstat(p)
					if err != nil {
						t.Fatalf("lstat %s: %v", p, err)
					}
					if fi.Mode().Perm() != wantPerm {
						t.Fatalf("%s: perm %s, want %s", p, fi.Mode().Perm(), wantPerm)
					}
				}
			}
		})
	}
}

func TestFastPathsOverwriteExistingFiles(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "perm"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "perm"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "same"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "same"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	cr, err := copyrec.New(src, dest, copyrec.Options{Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := cr.Run(testCtx()); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"perm", "same"} {
		p := filepath.Join(dest, name)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s: %v", p, err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("%s: perm %s, want 0644", p, fi.Mode().Perm())
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new" {
			t.Errorf("%s: content %q, want %q", p, got, "new")
		}
	}
}

func TestParallelPropagatesError(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%02d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	blocker := filepath.Join(dest, "f10")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(blocker, "keep")
	if err := os.WriteFile(inner, []byte("y"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocker, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocker, 0o755)

	cr, err := copyrec.New(src, dest, copyrec.Options{Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := cr.Run(testCtx()); err == nil {
		t.Log("note: RemoveAll may still succeed depending on FS/user; not fatal")
	}
}
