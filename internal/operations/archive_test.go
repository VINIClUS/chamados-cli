package operations

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureBackup(t *testing.T) (string, Manifest) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "database.dump"), []byte("PGDMP\x01snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	content := []byte("attachment\x00\xff")
	if err := w.WriteHeader(
		&tar.Header{Name: "nested/file.bin", Mode: 0600, Size: int64(len(content))},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "attachments.tar"), b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		SchemaVersion: 1,
		Environment:   "staging",
		Revision:      strings.Repeat("a", 40),
		Ref:           "main",
		PostgresMajor: json.RawMessage("16"),
		CreatedAt:     "2026-11-07T12:00:00Z",
	}
	return root, m
}

func TestBackupRoundTripAndCorruptionNeverExtracts(t *testing.T) {
	raw, m := fixtureBackup(t)
	out := filepath.Join(t.TempDir(), "package")
	if err := Pack(raw, m, out); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "verified")
	if _, err := Verify(
		filepath.Join(out, "backup.tar.gz"),
		filepath.Join(out, "manifest.json"),
		"staging",
		16,
		dest,
	); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "database.dump"))
	if err != nil || string(b) != "PGDMP\x01snapshot" {
		t.Fatalf("dump: %q %v", b, err)
	}
	archive := filepath.Join(out, "backup.tar.gz")
	f, err := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("corrupt")
	_ = f.Close()
	invalid := filepath.Join(t.TempDir(), "invalid")
	if _, err = Verify(archive, filepath.Join(out, "manifest.json"), "staging", 16, invalid); err == nil {
		t.Fatal("accepted corrupt archive")
	}
	if _, err = os.Stat(invalid); !os.IsNotExist(err) {
		t.Fatalf("created destination before validation: %v", err)
	}
}

func TestAttachmentArchiveRejectsTraversalLinksAndFileAncestors(t *testing.T) {
	for _, headers := range [][]*tar.Header{
		{{Name: "../../escape", Typeflag: tar.TypeReg}},
		{{Name: "/absolute", Typeflag: tar.TypeReg}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		{{Name: "file/child", Typeflag: tar.TypeReg}, {Name: "file", Typeflag: tar.TypeReg}},
		{{Name: "duplicate", Typeflag: tar.TypeReg}, {Name: "./duplicate", Typeflag: tar.TypeReg}},
	} {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		for _, h := range headers {
			if err := w.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
		}
		_ = w.Close()
		if err := CheckTar(bytes.NewReader(b.Bytes())); err == nil {
			t.Fatalf("accepted unsafe members: %+v", headers)
		}
	}
}

func TestSourceAcceptsGitGlobalPAXMetadataWithoutExtractingIt(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(
		&tar.Header{
			Name:       "pax_global_header",
			Typeflag:   tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)},
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "app.txt", Typeflag: tar.TypeReg, Mode: 0600, Size: 7}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("source\n"))
	_ = tw.Close()
	input := filepath.Join(t.TempDir(), "source.tar")
	if err := os.WriteFile(input, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "app")
	if err := ExtractSource(input, dest, false); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(dest, "app.txt")); err != nil || string(content) != "source\n" {
		t.Fatalf("source: %q %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "pax_global_header")); !os.IsNotExist(err) {
		t.Fatal("extracted global metadata as a file")
	}
}

func TestTarRejectsGlobalPAXThatChangesExtractionSemantics(t *testing.T) {
	for _, record := range []map[string]string{{"path": "../../escape"}, {"linkpath": "/etc/passwd"}, {"size": "900000000"}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		if err := tw.WriteHeader(
			&tar.Header{Name: "global", Typeflag: tar.TypeXGlobalHeader, PAXRecords: record},
		); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(
			&tar.Header{Name: "safe", Typeflag: tar.TypeReg, Mode: 0600, Size: 0},
		); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		if err := CheckTar(bytes.NewReader(b.Bytes())); err == nil {
			t.Fatalf("accepted semantic global PAX record: %v", record)
		}
	}
}
