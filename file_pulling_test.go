package common_go

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var mockHashMutex sync.Mutex

func newMockDataFileServer(timeout time.Duration) *httptest.Server {
	s := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				// Open the file for reading
				mockHashMutex.Lock()
				file, err := os.Open("./mock_hash.gz")
				if err != nil {
					log.Printf("Failed to open file: %v", err)
					return
				}
				defer mockHashMutex.Unlock()
				defer file.Close()

				buffer := bytes.NewBuffer(make([]byte, 0))
				_, err = io.Copy(buffer, file)
				if err != nil {
					log.Printf("Failed to read file: %v", err)
					return
				}

				w.Header().Add("Content-MD5", "daebfa89ddefac8e6c4325c38f129504")
				w.Header().Add("Content-Length", strconv.Itoa(buffer.Len()))
				w.Header().Add("Content-Type", "application/octet-stream")

				w.WriteHeader(http.StatusOK)

				// Write the file to response
				_, err = io.Copy(w, buffer)
				if err != nil {
					log.Printf("Failed to write file: %v", err)
					return
				}

			},
		),
	)

	s.URL = strings.ReplaceAll(s.URL, "127.0.0.1", "localhost")

	return awaitServer(s, timeout)
}

func awaitServer(s *httptest.Server, timeout time.Duration) *httptest.Server {
	readyChan := make(chan struct{}, 1)
	go func(s *httptest.Server) {
		for {
			resp, err := http.Get(s.URL)
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				readyChan <- struct{}{}
				close(readyChan)
				return
			}
			resp.Body.Close()
			time.Sleep(50 * time.Millisecond)
		}
	}(s)

	select {
	case <-readyChan:
		return s
	case <-time.After(timeout):
		log.Fatalf("Failed to start server in %v", timeout)
	}

	return nil
}

func newMockUncompressedDataFileServer(timeout time.Duration) *httptest.Server {
	s := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				// Open the file for reading
				mockHashMutex.Lock()
				file, err := os.Open("./mock_hash.gz")
				if err != nil {
					log.Printf("Failed to open file: %v", err)
					return
				}
				defer mockHashMutex.Unlock()
				defer file.Close()
				w.Header().Add("Content-Type", "application/octet-stream")

				// uncompressed the file
				fileBytes, err := gzip.NewReader(file)
				if err != nil {
					log.Printf("Failed to decompress file: %v", err)
					return
				}

				w.WriteHeader(http.StatusOK)

				// Write the file to response
				_, err = io.Copy(w, fileBytes)
				if err != nil {
					log.Printf("Failed to write file: %v", err)
					return
				}

			},
		),
	)

	s.URL = strings.ReplaceAll(s.URL, "127.0.0.1", "localhost")

	return awaitServer(s, timeout)
}

// Mock implementations for testing
type mockFileUpdater struct {
	dataFile               string
	dataFileUrl            string
	dataFilePullEveryMs    int
	isUpdateOnStartEnabled bool
	isFileWatcherEnabled   bool
	logger                 *mockLogWrapper
	totalFilePulls         int
	fileSynced             bool
	lastModificationTime   *time.Time
	randomization          int
}

func (m *mockFileUpdater) GetDataFile() string {
	return m.dataFile
}

func (m *mockFileUpdater) GetDataFileUrl() string {
	return m.dataFileUrl
}

func (m *mockFileUpdater) GetDataFilePullEveryMs() int {
	return m.dataFilePullEveryMs
}

func (m *mockFileUpdater) SetLastModificationTimestamp(t *time.Time) {
	m.lastModificationTime = t
}

func (m *mockFileUpdater) getFilePath() string {
	return m.dataFile
}

// FileUpdater interface for testing
type FileUpdaterI interface {
	GetDataFile() string
	GetDataFileUrl() string
	GetDataFilePullEveryMs() int
	SetLastModificationTimestamp(*time.Time)
	getFilePath() string
}

// Test data creation helpers
func createTestFile(t *testing.T, content string) string {
	tempFile, err := os.CreateTemp("", "test_data_*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer tempFile.Close()

	_, err = tempFile.WriteString(content)
	if err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	return tempFile.Name()
}

func createGzipFile(t *testing.T, content string) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write([]byte(content))
	if err != nil {
		t.Fatalf("Failed to write to gzip: %v", err)
	}
	gw.Close()
	return buf.Bytes()
}

func calculateMD5(data []byte) string {
	h := md5.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Mock HTTP servers for testing
func createMockServer(statusCode int, headers map[string]string, body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(statusCode)
		if body != nil {
			w.Write(body)
		}
	}))
}

func TestIsGzip(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		expected    bool
	}{
		{"gzip content type", "application/gzip", true},
		{"x-gzip content type", "application/x-gzip", true},
		{"plain text", "text/plain", false},
		{"json", "application/json", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isGzip(tt.contentType)
			if result != tt.expected {
				t.Errorf("isGzip(%s) = %v, want %v", tt.contentType, result, tt.expected)
			}
		})
	}
}

func TestValidateMd5(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		hash     string
		expected bool
		hasError bool
	}{
		{
			name:     "valid hash",
			data:     []byte("test data"),
			hash:     calculateMD5([]byte("test data")),
			expected: true,
			hasError: false,
		},
		{
			name:     "invalid hash",
			data:     []byte("test data"),
			hash:     "invalid_hash",
			expected: false,
			hasError: false,
		},
		{
			name:     "empty data",
			data:     []byte(""),
			hash:     calculateMD5([]byte("")),
			expected: true,
			hasError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := validateMd5(tt.data, tt.hash)

			if tt.hasError && err == nil {
				t.Errorf("validateMd5() expected error but got none")
			}
			if !tt.hasError && err != nil {
				t.Errorf("validateMd5() unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("validateMd5() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestDoDataFileRequest_Success(t *testing.T) {
	testData := "test file content"
	headers := map[string]string{
		"Content-Type": "application/octet-stream",
		"Content-MD5":  calculateMD5([]byte(testData)),
	}

	server := createMockServer(http.StatusOK, headers, []byte(testData))
	defer server.Close()

	logger := &LogWrapper{}

	result, err := doDataFileRequest(server.URL, logger, nil)

	if err != nil {
		t.Errorf("doDataFileRequest() unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("doDataFileRequest() returned nil result")
	}
	if result.buffer.String() != testData {
		t.Errorf("doDataFileRequest() content = %s, want %s", result.buffer.String(), testData)
	}
	if result.retryAfter != 0 {
		t.Errorf("doDataFileRequest() retryAfter = %d, want 0", result.retryAfter)
	}
}

func TestDoDataFileRequest_GzipContent(t *testing.T) {
	testData := "compressed test content"
	gzipData := createGzipFile(t, testData)

	headers := map[string]string{
		"Content-Type": "application/gzip",
		"Content-MD5":  calculateMD5(gzipData),
	}

	server := createMockServer(http.StatusOK, headers, gzipData)
	defer server.Close()

	logger := &LogWrapper{}

	result, err := doDataFileRequest(server.URL, logger, nil)

	if err != nil {
		t.Errorf("doDataFileRequest() unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("doDataFileRequest() returned nil result")
	}
	if result.buffer.String() != testData {
		t.Errorf("doDataFileRequest() content = %s, want %s", result.buffer.String(), testData)
	}
}

func TestDoDataFileRequest_NotModified(t *testing.T) {
	server := createMockServer(http.StatusNotModified, nil, nil)
	defer server.Close()

	logger := &LogWrapper{}
	timestamp := time.Now()

	result, err := doDataFileRequest(server.URL, logger, &timestamp)

	if !errors.Is(err, ErrFileNotModified) {
		t.Errorf("doDataFileRequest() error = %v, want ErrFileNotModified", err)
	}
	if result != nil {
		t.Errorf("doDataFileRequest() result should be nil for not modified")
	}
}
func TestDoDataFileRequest_InvalidRetryAfter(t *testing.T) {
	headers := map[string]string{
		"Retry-After": "invalid",
	}

	server := createMockServer(http.StatusTooManyRequests, headers, nil)
	defer server.Close()

	logger := &LogWrapper{}

	result, err := doDataFileRequest(server.URL, logger, nil)

	if err == nil {
		t.Errorf("doDataFileRequest() expected error for invalid Retry-After")
	}
	if result != nil {
		t.Errorf("doDataFileRequest() result should be nil for invalid Retry-After")
	}
}

func TestDoDataFileRequest_ServerError(t *testing.T) {
	server := createMockServer(http.StatusInternalServerError, nil, nil)
	defer server.Close()

	logger := &LogWrapper{}

	result, err := doDataFileRequest(server.URL, logger, nil)

	if err == nil {
		t.Errorf("doDataFileRequest() expected error for server error")
	}
	if result != nil {
		t.Errorf("doDataFileRequest() result should be nil for server error")
	}
}

func TestDoDataFileRequest_InvalidMD5(t *testing.T) {
	testData := "test content"
	headers := map[string]string{
		"Content-Type": "application/octet-stream",
		"Content-MD5":  "invalid_md5_hash",
	}

	server := createMockServer(http.StatusOK, headers, []byte(testData))
	defer server.Close()

	logger := &LogWrapper{}

	result, err := doDataFileRequest(server.URL, logger, nil)

	if err == nil {
		t.Errorf("doDataFileRequest() expected error for invalid MD5")
	}
	if result != nil {
		t.Errorf("doDataFileRequest() result should be nil for invalid MD5")
	}
}

func TestDoDataFileRequest_NoMD5Header(t *testing.T) {
	testData := "test content"
	headers := map[string]string{
		"Content-Type": "application/octet-stream",
	}

	server := createMockServer(http.StatusOK, headers, []byte(testData))
	defer server.Close()

	logger := &LogWrapper{}

	result, err := doDataFileRequest(server.URL, logger, nil)

	if err != nil {
		t.Errorf("doDataFileRequest() unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("doDataFileRequest() returned nil result")
	}
}

func TestDoDataFileRequest_InvalidURL(t *testing.T) {
	logger := &LogWrapper{}

	result, err := doDataFileRequest("invalid://url", logger, nil)

	if err == nil {
		t.Errorf("doDataFileRequest() expected error for invalid URL")
	}
	if result != nil {
		t.Errorf("doDataFileRequest() result should be nil for invalid URL")
	}
}
