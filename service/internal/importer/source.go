package importer

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"readiness.local/postgres/lifecycle"
)

const MaxRecordBytes = 1 << 20

var requiredHeader = []string{
	"client_id",
	"vendor_name",
	"vendor_tin",
	"payment_date",
	"amount",
	"payment_method",
	"backup_withholding",
	"memo",
}

type Source struct {
	file      *os.File
	gzip      *gzip.Reader
	csv       *csv.Reader
	hash      hash.Hash
	rowNumber int64
	exhausted bool
	closed    bool
}

func Discover(inputPath, firmID string) (string, error) {
	if strings.TrimSpace(firmID) == "" {
		return "", errors.New("firm ID is required")
	}
	info, err := os.Lstat(inputPath)
	if err != nil {
		return "", fmt.Errorf("inspect input: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("input symlinks are not supported")
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() || !strings.HasSuffix(info.Name(), ".csv.gz") {
			return "", errors.New("input must be a regular .csv.gz file")
		}
		return inputPath, nil
	}

	entries, err := os.ReadDir(inputPath)
	if err != nil {
		return "", fmt.Errorf("read input directory: %w", err)
	}
	expectedName := "firm_" + firmID + "_export.csv.gz"
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if entry.Name() != expectedName || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("inspect input entry: %w", err)
		}
		if entryInfo.Mode().IsRegular() {
			matches = append(matches, filepath.Join(inputPath, entry.Name()))
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected one %s in input directory, found %d", expectedName, len(matches))
	}
	return matches[0], nil
}

func Open(filePath string) (*Source, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open export: %w", err)
	}
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("open gzip export: %w", err)
	}
	digest := sha256.New()
	reader := csv.NewReader(io.TeeReader(gzipReader, digest))
	reader.FieldsPerRecord = len(requiredHeader)
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		_ = gzipReader.Close()
		_ = file.Close()
		return nil, fmt.Errorf("read CSV header: %w", err)
	}
	if !validUTF8(header) || !slices.Equal(header, requiredHeader) {
		_ = gzipReader.Close()
		_ = file.Close()
		return nil, fmt.Errorf("CSV header must exactly match %s", strings.Join(requiredHeader, ","))
	}
	return &Source{file: file, gzip: gzipReader, csv: reader, hash: digest}, nil
}

func (source *Source) Next(ctx context.Context) (lifecycle.PaymentRow, bool, error) {
	if source.closed {
		return lifecycle.PaymentRow{}, false, errors.New("payment source is closed")
	}
	if source.exhausted {
		return lifecycle.PaymentRow{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return lifecycle.PaymentRow{}, false, err
	}
	record, err := source.csv.Read()
	if errors.Is(err, io.EOF) {
		source.exhausted = true
		return lifecycle.PaymentRow{}, false, nil
	}
	source.rowNumber++
	if err != nil {
		return lifecycle.PaymentRow{}, false, fmt.Errorf("source record %d: %w", source.rowNumber, err)
	}
	if !validUTF8(record) {
		return lifecycle.PaymentRow{}, false, fmt.Errorf("source record %d: invalid UTF-8", source.rowNumber)
	}
	bytes := 0
	for _, field := range record {
		bytes += len(field)
	}
	if bytes > MaxRecordBytes {
		return lifecycle.PaymentRow{}, false, fmt.Errorf("source record %d: exceeds %d bytes", source.rowNumber, MaxRecordBytes)
	}
	return lifecycle.PaymentRow{
		SourceRowNumber:   source.rowNumber,
		ClientID:          record[0],
		VendorName:        record[1],
		VendorTIN:         record[2],
		PaymentDate:       record[3],
		Amount:            record[4],
		PaymentMethod:     record[5],
		BackupWithholding: record[6],
		Memo:              record[7],
	}, true, nil
}

func (source *Source) SHA256() ([32]byte, error) {
	if !source.exhausted {
		return [32]byte{}, errors.New("decompressed SHA-256 is unavailable before EOF")
	}
	var result [32]byte
	copy(result[:], source.hash.Sum(nil))
	return result, nil
}

func (source *Source) Close() error {
	if source.closed {
		return nil
	}
	source.closed = true
	gzipErr := source.gzip.Close()
	fileErr := source.file.Close()
	if gzipErr != nil {
		return gzipErr
	}
	return fileErr
}

func validUTF8(fields []string) bool {
	for _, field := range fields {
		if !utf8.ValidString(field) {
			return false
		}
	}
	return true
}
