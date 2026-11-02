package release_test

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseArchiveAndChecksums(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("release target linux/amd64")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	cmd := exec.Command("bash", filepath.Join(root, "scripts/package-release.sh"), out)
	cmd.Env = append(os.Environ(), "GO="+filepath.Join(runtime.GOROOT(), "bin/go"))
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("packaging: %v\n%s", err, result)
	}
	name := "opencode-go_0.2.2_linux_amd64.zip"
	raw, err := os.ReadFile(filepath.Join(out, name))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sums), fmt.Sprintf("%x  %s", sha256.Sum256(raw), name)) {
		t.Fatal("wrong archive checksum")
	}
	reader, err := zip.OpenReader(filepath.Join(out, name))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	files := map[string][]byte{}
	for _, f := range reader.File {
		if strings.Contains(f.Name, "/") {
			t.Fatal("nested/unsafe entry")
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	lib, ok := files["opencode-go.so"]
	if !ok || len(lib) == 0 || len(files) != 3 {
		t.Fatalf("unexpected archive entries %v", reader.File)
	}
	if !strings.Contains(string(files["checksums.txt"]), fmt.Sprintf("%x  opencode-go.so", sha256.Sum256(lib))) {
		t.Fatal("wrong library checksum")
	}
	if len(files["LICENSE"]) == 0 {
		t.Fatal("license missing")
	}
}
