// Command package builds separately installable native plugin archives. It must
// run from the repository root and never publishes or installs its output.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

var (
	pluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	versionPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
)

func main() {
	version := flag.String("version", "", "shared release version without a leading v")
	goos := flag.String("goos", runtime.GOOS, "target operating system")
	goarch := flag.String("goarch", runtime.GOARCH, "target architecture (requires a matching C compiler)")
	out := flag.String("out", "dist", "output directory")
	flag.Parse()
	if err := build(*version, *goos, *goarch, *out, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func build(version, goos, goarch, out string, plugins []string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("-version must be a release version such as 0.1.0 or 0.1.0-beta.1")
	}
	ext, errPlatform := libraryExtension(goos, goarch)
	if errPlatform != nil {
		return errPlatform
	}
	if len(plugins) == 0 {
		return fmt.Errorf("name at least one plugin to build")
	}
	seen := make(map[string]bool)
	for _, id := range plugins {
		if !pluginIDPattern.MatchString(id) || seen[id] {
			return fmt.Errorf("invalid or repeated plugin id %q", id)
		}
		seen[id] = true
	}
	if errDir := os.MkdirAll(out, 0o755); errDir != nil {
		return errDir
	}
	tmp, errTemp := os.MkdirTemp(out, ".build-")
	if errTemp != nil {
		return errTemp
	}
	defer func() {
		if errRemove := os.RemoveAll(tmp); errRemove != nil {
			fmt.Fprintln(os.Stderr, "remove build directory:", errRemove)
		}
	}()
	var checksums []string
	for _, id := range plugins {
		library := filepath.Join(tmp, id+ext)
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-buildmode=c-shared",
			"-ldflags=-X github.com/unstableneutron/cpa-plugins/internal/nativeabi.Version="+version,
			"-o", library, "./"+id)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOOS="+goos, "GOARCH="+goarch)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if errBuild := cmd.Run(); errBuild != nil {
			return fmt.Errorf("build %s: %w", id, errBuild)
		}
		name := archiveName(id, version, goos, goarch)
		path := filepath.Join(tmp, name)
		files := []string{library, "LICENSE", "GO_NOTICES.txt", filepath.Join(id, "README.md")}
		notices, errNotices := filepath.Glob(filepath.Join(id, "*NOTICES*"))
		if errNotices != nil {
			return errNotices
		}
		files = append(files, notices...)
		if errArchive := writeArchive(path, files); errArchive != nil {
			return fmt.Errorf("package %s: %w", id, errArchive)
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		checksums = append(checksums, fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name))
	}
	// Finish every build before replacing output archives or their checksum list.
	for _, id := range plugins {
		name := archiveName(id, version, goos, goarch)
		if errRename := os.Rename(filepath.Join(tmp, name), filepath.Join(out, name)); errRename != nil {
			return errRename
		}
		fmt.Println(filepath.Join(out, name))
	}
	sort.Strings(checksums)
	if errWrite := os.WriteFile(filepath.Join(tmp, "checksums.txt"), []byte(strings.Join(checksums, "")), 0o644); errWrite != nil {
		return errWrite
	}
	return os.Rename(filepath.Join(tmp, "checksums.txt"), filepath.Join(out, "checksums.txt"))
}

func libraryExtension(goos, goarch string) (string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("unsupported target architecture %q", goarch)
	}
	switch goos {
	case "linux":
		return ".so", nil
	case "darwin":
		return ".dylib", nil
	case "windows":
		return ".dll", nil
	default:
		return "", fmt.Errorf("unsupported native plugin target %q", goos)
	}
}

func archiveName(id, version, goos, goarch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.zip", id, version, goos, goarch)
}

func writeArchive(path string, files []string) (err error) {
	out, errCreate := os.Create(path)
	if errCreate != nil {
		return errCreate
	}
	defer func() {
		if errClose := out.Close(); err == nil {
			err = errClose
		}
	}()
	writer := zip.NewWriter(out)
	for _, path := range files {
		if errCopy := addArchiveFile(writer, path); errCopy != nil {
			_ = writer.Close()
			return errCopy
		}
	}
	return writer.Close()
}

func addArchiveFile(writer *zip.Writer, path string) (err error) {
	in, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		if errClose := in.Close(); err == nil {
			err = errClose
		}
	}()
	info, errStat := in.Stat()
	if errStat != nil {
		return errStat
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive input is not a regular file: %s", path)
	}
	// Stable metadata; do not embed local paths, timestamps, or generated C headers.
	header := &zip.FileHeader{Name: filepath.Base(path), Method: zip.Deflate}
	header.SetMode(info.Mode().Perm())
	dst, errEntry := writer.CreateHeader(header)
	if errEntry != nil {
		return errEntry
	}
	_, errCopy := io.Copy(dst, in)
	return errCopy
}
