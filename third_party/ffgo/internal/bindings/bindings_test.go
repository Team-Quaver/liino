//go:build linux && (amd64 || arm64)

package bindings

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFakeLibrary(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "libavutil.so."+version), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func discoveredFrom(paths []string, dirs ...string) []string {
	var got []string
	for _, path := range paths {
		for _, dir := range dirs {
			if strings.HasPrefix(path, dir+string(filepath.Separator)) {
				got = append(got, filepath.Base(path))
				break
			}
		}
	}
	return got
}

func TestDiscoverPrefersNewestVersionInDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, version := range []string{"56", "58", "61"} {
		writeFakeLibrary(t, dir, version)
	}
	t.Setenv("LD_LIBRARY_PATH", dir)

	got := discoveredFrom(discover("avutil"), dir)
	want := []string{"libavutil.so.61", "libavutil.so.58", "libavutil.so.56"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discover order = %v, want %v", got, want)
	}
}

func TestDiscoverKeepsSearchPathOrder(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeFakeLibrary(t, first, "58")
	writeFakeLibrary(t, second, "61")
	t.Setenv("LD_LIBRARY_PATH", first+string(os.PathListSeparator)+second)

	got := discoveredFrom(discover("avutil"), first, second)
	want := []string{"libavutil.so.58", "libavutil.so.61"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discover order = %v, want %v", got, want)
	}
}
