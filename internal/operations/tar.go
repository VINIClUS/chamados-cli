package operations

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

func memberName(h *tar.Header) (string, error) {
	// git archive emits a global PAX header containing the source commit.
	if h.Typeflag == tar.TypeXGlobalHeader {
		for key := range h.PAXRecords {
			if key != "comment" {
				return "", fmt.Errorf("unsupported global PAX record")
			}
		}
		return "", nil
	}
	name := strings.TrimSuffix(h.Name, "/")
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if (name == "" || name == ".") && h.Typeflag == tar.TypeDir {
		return "", nil
	}
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || path.Clean(name) != name ||
		name == ".." ||
		strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("unsafe tar member")
	}
	if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir {
		return "", fmt.Errorf("unsupported tar member type")
	}
	return name, nil
}

// CheckTar validates the whole member set, including ancestors arriving after children.
func CheckTar(reader io.Reader) error {
	tr := tar.NewReader(reader)
	seen := map[string]bool{}
	parents := map[string]bool{}
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if count >= 100000 {
			return fmt.Errorf("too many tar members")
		}
		name, err := memberName(h)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("duplicate tar member")
		}
		directory := h.Typeflag == tar.TypeDir
		if !directory && parents[name] {
			return fmt.Errorf("file used as directory")
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if isDir, ok := seen[parent]; ok && !isDir {
				return fmt.Errorf("file used as directory")
			}
			parents[parent] = true
		}
		seen[name] = directory
	}
}

func checkTarFile(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return CheckTar(f)
}
