package operations

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func Pack(raw string, m Manifest, out string) error {
	if !m.MetadataValid() {
		return fmt.Errorf("invalid backup metadata")
	}
	dump, err := os.Open(filepath.Join(raw, "database.dump"))
	if err != nil {
		return err
	}
	var magic [5]byte
	_, err = io.ReadFull(dump, magic[:])
	_ = dump.Close()
	if err != nil || string(magic[:]) != "PGDMP" {
		return fmt.Errorf("database.dump must use pg_dump custom format")
	}
	if err = checkTarFile(filepath.Join(raw, "attachments.tar")); err != nil {
		return err
	}
	m.SHA256 = ""
	m.Members = nil
	for _, name := range []string{"database.dump", "attachments.tar"} {
		file := filepath.Join(raw, name)
		sum, e := Digest(file)
		if e != nil {
			return e
		}
		info, e := os.Stat(file)
		if e != nil {
			return e
		}
		m.Members = append(m.Members, Member{Name: name, SHA256: sum, Size: info.Size()})
	}
	if err = os.Mkdir(out, 0700); err != nil {
		return err
	}
	if err = WriteJSON(filepath.Join(out, "manifest.json"), m); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(out, "backup.tar.gz"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		_ = f.Close()
		return err
	}
	tw := tar.NewWriter(gz)
	for _, name := range []string{"database.dump", "attachments.tar", "manifest.json"} {
		base := raw
		if name == "manifest.json" {
			base = out
		}
		if err = addFile(tw, filepath.Join(base, name), name); err != nil {
			break
		}
	}
	for _, closer := range []io.Closer{tw, gz, f} {
		if e := closer.Close(); err == nil {
			err = e
		}
	}
	if err != nil {
		return err
	}
	m.SHA256, err = Digest(filepath.Join(out, "backup.tar.gz"))
	if err != nil {
		return err
	}
	return WriteJSON(filepath.Join(out, "manifest.json"), m)
}

func addFile(tw *tar.Writer, file, name string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("non-regular backup member")
	}
	if err = tw.WriteHeader(
		&tar.Header{Name: name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg},
	); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func Verify(archive, manifest, environment string, major int, dest string) (Manifest, error) {
	var m Manifest
	if err := ReadJSON(manifest, &m); err != nil {
		return m, err
	}
	if !m.Valid(environment) || m.Major() != major {
		return m, fmt.Errorf("incompatible backup metadata")
	}
	sum, err := Digest(archive)
	if err != nil {
		return m, err
	}
	if !strings.EqualFold(sum, m.SHA256) {
		return m, fmt.Errorf("backup archive checksum mismatch")
	}
	tmp, err := os.MkdirTemp("", "chamados-verify-*")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(tmp)
	if err = unpackBackup(archive, tmp, m); err != nil {
		return m, err
	}
	if err = checkTarFile(filepath.Join(tmp, "attachments.tar")); err != nil {
		return m, err
	}
	dump, err := os.Open(filepath.Join(tmp, "database.dump"))
	if err != nil {
		return m, err
	}
	var magic [5]byte
	_, err = io.ReadFull(dump, magic[:])
	_ = dump.Close()
	if err != nil || string(magic[:]) != "PGDMP" {
		return m, fmt.Errorf("not a custom PostgreSQL dump")
	}
	if err = os.Mkdir(dest, 0700); err != nil {
		return m, err
	}
	for _, name := range []string{"database.dump", "attachments.tar", "manifest.json"} {
		if err = copyFile(filepath.Join(tmp, name), filepath.Join(dest, name)); err != nil {
			_ = os.RemoveAll(dest)
			return m, err
		}
	}
	return m, nil
}

func unpackBackup(archive, dest string, m Manifest) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	sizes := map[string]int64{"manifest.json": 1 << 20}
	for _, item := range m.Members {
		sizes[item.Name] = item.Size
	}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name, e := memberName(h)
		limit, ok := sizes[name]
		if e != nil || !ok || seen[name] || h.Typeflag == tar.TypeDir || h.Size > limit {
			return fmt.Errorf("unexpected backup member")
		}
		seen[name] = true
		output, e := os.OpenFile(filepath.Join(dest, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(output, tr)
		closeErr := output.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if len(seen) != 3 {
		return fmt.Errorf("incomplete backup members")
	}
	var inner, outer map[string]any
	if err = ReadJSON(filepath.Join(dest, "manifest.json"), &inner); err != nil {
		return err
	}
	// Compare all keys, not only fields recognized by this executor.
	data, err := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err != nil {
		return err
	}
	_ = data
	for _, item := range m.Members {
		info, e := os.Stat(filepath.Join(dest, item.Name))
		if e != nil {
			return e
		}
		sum, e := Digest(filepath.Join(dest, item.Name))
		if e != nil || info.Size() != item.Size || !strings.EqualFold(sum, item.SHA256) {
			return fmt.Errorf("member checksum mismatch")
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &outer); err != nil {
		return err
	}
	delete(outer, "sha256")
	if !reflect.DeepEqual(inner, outer) {
		return fmt.Errorf("inner and external manifests differ")
	}
	return nil
}

func copyFile(source, dest string) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, f)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
