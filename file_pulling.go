package common_go

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const retryMs = 1000

// ScheduleFilePulling schedules periodic file pulling based on specified intervals and handles stop signals gracefully.
// It optionally performs an immediate pull on startup if the update-on-start feature is enabled.
func (e *FileUpdater) ScheduleFilePulling(stopCh chan *sync.WaitGroup, reloadFileEvents chan struct{}) {
	nextIterationInMs := e.dataFilePullEveryMs

	// if update on start is enabled, perform the pull immediately
	if e.isUpdateOnStartEnabled {
		e.logger.Printf("Doing pull on start")
		nextIterationInMs = e.doFilePulling(reloadFileEvents)
	}

	for {
		select {
		case wg := <-stopCh:
			wg.Done()
			return
		// interval to perform the pull of updated data
		case <-time.After(time.Duration(nextIterationInMs+e.randomization) * time.Millisecond):
			nextIterationInMs = e.doFilePulling(reloadFileEvents)
		}
	}
}

// doFilePulling handles the process of pulling a data file, saving it locally, and triggering reload events if necessary.
// It checks for file modifications, makes an HTTP request to fetch the file, and handles retry logic for failures.
// The method returns the time in milliseconds before the next pull attempt.
func (e *FileUpdater) doFilePulling(reloadFileEvents chan struct{}) int {
	e.logger.Printf("Pulling data from %s", e.GetDataFileUrl())

	var lastModificationTimestamp *time.Time

	if len(e.getFilePath()) > 0 {
		file, err := os.Stat(e.GetDataFile())
		if err != nil {
			e.logger.Printf("failed to get file info: %v", err)
			// retry after 1 second, since we have unhandled error
			// this can happen from file info error or something else
			return retryMs
		}
		modTime := file.ModTime().UTC()
		lastModificationTimestamp = &modTime
	}

	fileResponse, err := doDataFileRequest(e.dataFileUrl, e.logger, lastModificationTimestamp)
	if err != nil {
		e.logger.Printf("failed to pull data file: %v", err)
		if errors.Is(err, ErrFileNotModified) {
			e.logger.Printf("skipping pull, file not modified")
			return e.GetDataFilePullEveryMs()
		} else if fileResponse != nil && fileResponse.retryAfter > 0 {
			e.logger.Printf("received retry-after, retrying after %d seconds", fileResponse.retryAfter)
			// retry after the specified time
			return fileResponse.retryAfter * 1000
		} else {
			e.logger.Printf("retrying after 1 second")
			// retry after 1 second, since we have unhandled error
			// this can happen from network stutter or something else
			return retryMs
		}
	}

	e.logger.Printf("data file pulled successfully: %d bytes", fileResponse.buffer.Len())

	// write the file to disk
	err = os.WriteFile(e.GetDataFile(), fileResponse.buffer.Bytes(), 0644)
	if err != nil {
		e.logger.Printf("failed to write data file: %v", err)
		// retry after 1 second, since we have unhandled error
		// this can happen from disk write error or something else
		return retryMs
	}
	e.logger.Printf("data file written successfully: %d bytes", fileResponse.buffer.Len())

	if !e.isFileWatcherEnabled {
		// use the chan for reload the file and reload manager
		reloadFileEvents <- struct{}{}
	}

	if !e.fileSynced {
		e.fileSynced = true
	}

	timestamp := time.Now().UTC()
	e.SetLastModificationTimestamp(&timestamp)
	e.totalFilePulls++

	// reset nextIterationInMs
	return e.dataFilePullEveryMs
}

// FileResponse represents the response obtained from a file request, including its buffer and retry interval.
type FileResponse struct {
	buffer     *bytes.Buffer
	retryAfter int
}

// doDataFileRequest performs an HTTP GET request to fetch a data file and handles retries, caching, and decompression.
// It accepts a URL, a logger for logging messages, and an optional timestamp for conditional requests.
// Returns a FileResponse containing the fetched data or an error if the request fails or conditions are not met.
func doDataFileRequest(url string, logger *LogWrapper, timestamp *time.Time) (*FileResponse, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	if timestamp != nil {
		req.Header.Set("If-Modified-Since", timestamp.Format(http.TimeFormat))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, ErrFileNotModified
	}

	//handle retry after
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")
		retryAfterSeconds, err := strconv.Atoi(retryAfter)
		if err != nil || len(retryAfter) == 0 {
			return nil, fmt.Errorf("failed to parse Retry-After header: %v", err)
		}

		return &FileResponse{
			retryAfter: retryAfterSeconds,
		}, fmt.Errorf("received 429, retrying after %d seconds", retryAfterSeconds)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to pull data file: %s", resp.Status)
	}

	var (
		fileBytes     io.Reader
		responseBytes []byte
	)

	// check if the response is compressed
	// if it is, decompress it
	buffer := bytes.NewBuffer(make([]byte, 0))
	_, err = buffer.ReadFrom(resp.Body)
	responseBytes = buffer.Bytes()
	contentType := http.DetectContentType(responseBytes)
	if isGzip(contentType) {
		fileBytes, err = gzip.NewReader(bytes.NewBuffer(responseBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to decompress file: %v", err)
		}
		//	if the response is not compressed, read it directly
	} else {
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
		fileBytes = bytes.NewBuffer(responseBytes)
	}

	uncompressedBuffer := bytes.NewBuffer(make([]byte, 0))
	_, err = uncompressedBuffer.ReadFrom(fileBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read decompressed file: %v", err)
	}
	uncompressedBytes := uncompressedBuffer.Bytes()
	contentMD5 := resp.Header.Get("Content-MD5")
	if len(contentMD5) > 0 {
		isValid, err := validateMd5(responseBytes, contentMD5)
		if err != nil {
			return nil, fmt.Errorf("failed to validate MD5: %v", err)
		}

		if !isValid {
			return nil, fmt.Errorf("MD5 validation failed")
		}
	} else {
		logger.Printf("MD5 header not found in response, skipping validation")
	}

	return &FileResponse{
		buffer:     bytes.NewBuffer(uncompressedBytes),
		retryAfter: 0,
	}, nil
}

// isGzip checks if the given content type corresponds to a Gzip-compressed format. Returns true if it matches.
func isGzip(contentType string) bool {
	return contentType == "application/gzip" || contentType == "application/x-gzip"
}

// validateMd5 compares a given MD5 hash for a byte slice and returns true if they match or an error if hashing fails.
func validateMd5(val []byte, hash string) (bool, error) {
	h := md5.New()
	_, err := h.Write(val)
	if err != nil {
		return false, err
	}

	strVal := hex.EncodeToString(h.Sum(nil))

	return strVal == hash, nil
}
