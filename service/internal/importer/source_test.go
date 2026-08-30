package importer

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, records [][]string) (string, []byte) {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "firm_F001_export.csv.gz")
	file, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	csvWriter := csv.NewWriter(gzipWriter)
	for _, record := range records {
		if err := csvWriter.Write(record); err != nil {
			t.Fatal(err)
		}
	}
	csvWriter.Flush()
	if err := csvWriter.Error(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	var decompressed []byte
	for _, record := range records {
		buffer := &sliceWriter{}
		writer := csv.NewWriter(buffer)
		_ = writer.Write(record)
		writer.Flush()
		decompressed = append(decompressed, buffer.bytes...)
	}
	return filePath, decompressed
}

type sliceWriter struct{ bytes []byte }

func (writer *sliceWriter) Write(payload []byte) (int, error) {
	writer.bytes = append(writer.bytes, payload...)
	return len(payload), nil
}

func TestSourceStreamsQuotedRowsAndHash(t *testing.T) {
	header := append([]string(nil), requiredHeader...)
	filePath, decompressed := writeFixture(t, [][]string{
		header,
		{"C001", "Vendor, LLC", "123-45-6789", "2025-01-02", "600.00", "check", "0.00", "line one\nline two"},
		{"C002", "Other", "", "2025-02-03", "10.00", "paypal", "0.00", ""},
	})
	source, err := Open(filePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	first, found, err := source.Next(context.Background())
	if err != nil || !found {
		t.Fatalf("first row: found=%v err=%v", found, err)
	}
	if first.SourceRowNumber != 1 || first.VendorName != "Vendor, LLC" || first.Memo != "line one\nline two" {
		t.Fatalf("unexpected first row: %+v", first)
	}
	second, found, err := source.Next(context.Background())
	if err != nil || !found || second.SourceRowNumber != 2 {
		t.Fatalf("second row: %+v found=%v err=%v", second, found, err)
	}
	if _, found, err := source.Next(context.Background()); err != nil || found {
		t.Fatalf("EOF: found=%v err=%v", found, err)
	}
	digest, err := source.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha256.Sum256(decompressed) {
		t.Fatal("decompressed hash mismatch")
	}
}

func TestSourceRejectsHeaderAndEarlyHash(t *testing.T) {
	filePath, _ := writeFixture(t, [][]string{{"wrong"}})
	if _, err := Open(filePath); err == nil {
		t.Fatal("invalid header accepted")
	}
	filePath, _ = writeFixture(t, [][]string{requiredHeader, {"C001", "Vendor", "123456789", "2025-01-02", "1.00", "check", "0.00", ""}})
	source, err := Open(filePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.SHA256(); err == nil {
		t.Fatal("hash available before EOF")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := source.Next(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v", err)
	}
}

func TestDiscoverUsesExplicitFirmMapping(t *testing.T) {
	directory := t.TempDir()
	filePath := filepath.Join(directory, "firm_F001_export.csv.gz")
	if err := os.WriteFile(filePath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(directory, "F001")
	if err != nil || got != filePath {
		t.Fatalf("discover = %q, %v", got, err)
	}
	if _, err := Discover(directory, "F002"); err == nil {
		t.Fatal("missing firm fixture accepted")
	}
}
