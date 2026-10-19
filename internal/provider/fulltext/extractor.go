package fulltext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"paperassistant/internal/config"
)

const maxTextBytes = 3_500_000

type Extractor struct {
	directory string
	binary    string
	maxBytes  int64
	http      *http.Client
}

func New(cfg config.FullTextConfig) *Extractor {
	extractor := &Extractor{directory: cfg.Directory, binary: cfg.PDFToText, maxBytes: cfg.MaxPDFBytes}
	extractor.http = &http.Client{
		Timeout: 90 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many PDF redirects")
			}
			return validateRemoteURL(req.Context(), req.URL)
		},
	}
	return extractor
}

func (e *Extractor) Fetch(ctx context.Context, paperID uint64, pdfURL string) (string, string, error) {
	parsed, err := url.Parse(pdfURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid PDF URL: %w", err)
	}
	if err = validateRemoteURL(ctx, parsed); err != nil {
		return "", "", err
	}
	if err = os.MkdirAll(e.directory, 0o755); err != nil {
		return "", "", fmt.Errorf("create fulltext directory: %w", err)
	}
	base := filepath.Join(e.directory, fmt.Sprintf("%d", paperID))
	pdfPath, textPath := base+".pdf", base+".txt"
	pdfTemp, err := os.CreateTemp(e.directory, fmt.Sprintf("%d-*.pdf.tmp", paperID))
	if err != nil {
		return "", "", err
	}
	pdfTempPath := pdfTemp.Name()
	_ = pdfTemp.Close()
	defer os.Remove(pdfTempPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", "", err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("download PDF: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("download PDF status %d", resp.StatusCode)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "pdf") && !strings.Contains(contentType, "octet-stream") {
		return "", "", fmt.Errorf("unexpected PDF content type %q", contentType)
	}
	header := make([]byte, 5)
	if _, err = io.ReadFull(resp.Body, header); err != nil || !bytes.Equal(header, []byte("%PDF-")) {
		return "", "", errors.New("downloaded file has no PDF signature")
	}
	file, err := os.Create(pdfTempPath)
	if err != nil {
		return "", "", err
	}
	written, copyErr := io.Copy(file, io.LimitReader(io.MultiReader(bytes.NewReader(header), resp.Body), e.maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return "", "", fmt.Errorf("save PDF: %w", copyErr)
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	if written > e.maxBytes {
		return "", "", fmt.Errorf("PDF exceeds %d bytes", e.maxBytes)
	}
	_ = os.Remove(pdfPath)
	if err = os.Rename(pdfTempPath, pdfPath); err != nil {
		return "", "", fmt.Errorf("commit PDF: %w", err)
	}
	defer os.Remove(pdfPath)

	textTemp := textPath + ".tmp"
	_ = os.Remove(textTemp)
	defer os.Remove(textTemp)
	text, err := e.extractText(ctx, pdfPath, textTemp)
	if err != nil {
		return "", "", err
	}
	if err = validate(text); err != nil {
		return "", text, err
	}
	_ = os.Remove(textPath)
	if err = os.Rename(textTemp, textPath); err != nil {
		return "", "", fmt.Errorf("commit extracted text: %w", err)
	}
	return textPath, text, nil
}

var (
	// ErrNotPDF means the upload does not start with the PDF signature.
	ErrNotPDF = errors.New("uploaded file is not a PDF")
	// ErrPDFTooLarge means the upload exceeds MAX_PDF_BYTES.
	ErrPDFTooLarge = errors.New("uploaded PDF is too large")
	// ErrUnreadable means pdftotext failed or the text failed the full-text validation gate.
	ErrUnreadable = errors.New("uploaded PDF has no verifiable paper text")
)

// ExtractUploaded runs an uploaded PDF through the same signature, size, pdftotext and
// validation gates as Fetch. Neither the PDF nor the text is kept on disk; the caller decides
// where verified text is stored (account-private full text, ADR 0019).
func (e *Extractor) ExtractUploaded(ctx context.Context, body io.Reader) (string, error) {
	if err := os.MkdirAll(e.directory, 0o755); err != nil {
		return "", fmt.Errorf("create fulltext directory: %w", err)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(body, header); err != nil || !bytes.Equal(header, []byte("%PDF-")) {
		return "", ErrNotPDF
	}
	pdfFile, err := os.CreateTemp(e.directory, "upload-*.pdf.tmp")
	if err != nil {
		return "", err
	}
	pdfPath := pdfFile.Name()
	defer os.Remove(pdfPath)
	written, copyErr := io.Copy(pdfFile, io.LimitReader(io.MultiReader(bytes.NewReader(header), body), e.maxBytes+1))
	closeErr := pdfFile.Close()
	if copyErr != nil {
		return "", fmt.Errorf("save uploaded PDF: %w", copyErr)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > e.maxBytes {
		return "", ErrPDFTooLarge
	}
	textFile, err := os.CreateTemp(e.directory, "upload-*.txt.tmp")
	if err != nil {
		return "", err
	}
	textPath := textFile.Name()
	_ = textFile.Close()
	defer os.Remove(textPath)
	text, err := e.extractText(ctx, pdfPath, textPath)
	if err != nil {
		return "", errors.Join(ErrUnreadable, err)
	}
	if err = validate(text); err != nil {
		return "", errors.Join(ErrUnreadable, err)
	}
	return text, nil
}

// extractText runs pdftotext on pdfPath into textPath and returns the bounded, trimmed text.
func (e *Extractor) extractText(ctx context.Context, pdfPath, textPath string) (string, error) {
	extractCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(extractCtx, e.binary, "-layout", "-nopgbrk", pdfPath, textPath)
	if output, commandErr := cmd.CombinedOutput(); commandErr != nil {
		return "", fmt.Errorf("pdftotext: %w: %s", commandErr, strings.TrimSpace(string(output)))
	}
	file, err := os.Open(textPath)
	if err != nil {
		return "", fmt.Errorf("open extracted text: %w", err)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maxTextBytes+1))
	_ = file.Close()
	if readErr != nil {
		return "", fmt.Errorf("read extracted text: %w", readErr)
	}
	if len(content) > maxTextBytes {
		return "", fmt.Errorf("extracted text exceeds %d bytes", maxTextBytes)
	}
	return strings.TrimSpace(string(content)), nil
}

func validateRemoteURL(ctx context.Context, value *url.URL) error {
	if value == nil || !strings.EqualFold(value.Scheme, "https") || value.Hostname() == "" {
		return errors.New("PDF URL must be HTTPS with a hostname")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, value.Hostname())
	if err != nil {
		return fmt.Errorf("resolve PDF host: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("PDF host resolved to no addresses")
	}
	for _, address := range addresses {
		ip := address.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("PDF host resolves to a non-public address: %s", ip)
		}
	}
	return nil
}

var wordPattern = regexp.MustCompile(`[A-Za-z\p{Han}]{2,}`)

func validate(text string) error {
	if len([]rune(text)) < 3000 {
		return errors.New("extracted full text is too short")
	}
	lower := strings.ToLower(text)
	markers := 0
	for _, marker := range []string{"abstract", "introduction", "references", "摘要", "引言", "参考文献"} {
		if strings.Contains(lower, marker) {
			markers++
		}
	}
	if markers < 2 {
		return errors.New("extracted text has no recognizable paper structure")
	}
	if len(wordPattern.FindAllString(text, -1)) < 500 {
		return errors.New("extracted text appears unreadable")
	}
	return nil
}
