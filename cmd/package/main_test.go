package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveLayoutAndReproducibility(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "commandcode.so")
	want := []byte{0, 1, 255, 0, 42}
	if err := os.WriteFile(library, want, 0o755); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.zip")
	for _, path := range []string{a, b} {
		if err := writeArchive(path, []string{library}); err != nil {
			t.Fatal(err)
		}
	}
	dataA, errA := os.ReadFile(a)
	dataB, errB := os.ReadFile(b)
	if errA != nil || errB != nil || !bytes.Equal(dataA, dataB) {
		t.Fatalf("archive bytes are unstable: %v %v", errA, errB)
	}
	reader, err := zip.NewReader(bytes.NewReader(dataA), int64(len(dataA)))
	if err != nil || len(reader.File) != 1 || reader.File[0].Name != "commandcode.so" {
		t.Fatalf("incorrect ZIP-root library layout: %v", err)
	}
	entry, err := reader.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, errRead := io.ReadAll(entry)
	errClose := entry.Close()
	if errRead != nil || errClose != nil || !bytes.Equal(got, want) {
		t.Fatalf("binary contents changed: %v %v", errRead, errClose)
	}
	if archiveName("commandcode", "0.1.0", "linux", "arm64") != "commandcode_0.1.0_linux_arm64.zip" {
		t.Fatal("asset name does not match CPA's plugin-store convention")
	}
}

func TestBuildRejectsInvalidInputsBeforeRunningGo(t *testing.T) {
	for _, tc := range []struct {
		version, os, arch string
		plugins           []string
	}{
		{"", "linux", "amd64", []string{"commandcode"}},
		{"0.1.0", "linux", "386", []string{"commandcode"}},
		{"0.1.0", "freebsd", "amd64", []string{"commandcode"}},
		{"0.1.0", "linux", "amd64", nil},
		{"0.1.0", "linux", "amd64", []string{"../escape"}},
		{"0.1.0", "linux", "amd64", []string{"commandcode", "commandcode"}},
	} {
		if err := build(tc.version, tc.os, tc.arch, t.TempDir(), tc.plugins); err == nil {
			t.Fatalf("accepted invalid input: %+v", tc)
		}
	}
}
