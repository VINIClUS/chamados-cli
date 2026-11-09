package operations

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractSource accepts a raw Git archive or a GitLab gzip archive with one root.
// The validation pass completes before creating the destination.
func ExtractSource(source, dest string, stripRoot bool) error {
	open := func() (io.ReadCloser, error) {
		f, err := os.Open(source)
		if err != nil {
			return nil, err
		}
		var magic [2]byte
		_, err = io.ReadFull(f, magic[:])
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		_, _ = f.Seek(0, io.SeekStart)
		if bytes.Equal(magic[:], []byte{0x1f, 0x8b}) {
			gz, e := gzip.NewReader(f)
			if e != nil {
				_ = f.Close()
				return nil, e
			}
			return &archiveReader{Reader: gz, closers: []io.Closer{gz, f}}, nil
		}
		return f, nil
	}
	r, err := open()
	if err != nil {
		return err
	}
	err = CheckTar(r)
	_ = r.Close()
	if err != nil {
		return err
	}
	r, err = open()
	if err != nil {
		return err
	}
	defer r.Close()
	tr := tar.NewReader(r)
	root := ""
	if err = os.Mkdir(dest, 0700); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dest)
		}
	}()
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name, e := memberName(h)
		if e != nil {
			return e
		}
		if name == "" {
			continue
		}
		if stripRoot {
			parts := strings.SplitN(name, "/", 2)
			if root == "" {
				root = parts[0]
			}
			if root != parts[0] {
				return fmt.Errorf("source must have exactly one root")
			}
			if len(parts) == 1 {
				continue
			}
			name = parts[1]
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".env" {
				return fmt.Errorf("source contains .env")
			}
		}
		if !applicationSourcePath(name) {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if h.Typeflag == tar.TypeDir {
			if e = os.MkdirAll(target, 0700); e != nil {
				return e
			}
			continue
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, tr)
		closeErr := out.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	success = true
	return nil
}

type archiveReader struct {
	io.Reader
	closers []io.Closer
}

func (r *archiveReader) Close() error {
	var err error
	for _, c := range r.closers {
		if e := c.Close(); err == nil {
			err = e
		}
	}
	return err
}
