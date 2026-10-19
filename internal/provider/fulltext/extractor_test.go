package fulltext

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"paperassistant/internal/config"
)

func TestExtractUploadedRejectsBeforeRunningPDFToText(t *testing.T) {
	directory := t.TempDir()
	extractor := New(config.FullTextConfig{Directory: directory, PDFToText: "pdftotext-not-installed", MaxPDFBytes: 64})

	if _, err := extractor.ExtractUploaded(context.Background(), strings.NewReader("<html>not a pdf</html>")); !errors.Is(err, ErrNotPDF) {
		t.Fatalf("non-PDF upload: %v", err)
	}
	if _, err := extractor.ExtractUploaded(context.Background(), bytes.NewReader(append([]byte("%PDF-"), make([]byte, 100)...))); !errors.Is(err, ErrPDFTooLarge) {
		t.Fatalf("oversized upload: %v", err)
	}
	if _, err := extractor.ExtractUploaded(context.Background(), strings.NewReader("%PDF-1.7 tiny")); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("unextractable upload: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("uploads must leave no PDF or text on disk, found %d entries", len(entries))
	}
}
