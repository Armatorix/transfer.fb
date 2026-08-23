/*
The MIT License (MIT)

Copyright (c) 2014-2017 DutchCoders [https://github.com/dutchcoders/]
Copyright (c) 2018-2020 Andrea Spacca.
Copyright (c) 2020- Andrea Spacca and Stefan Benten.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dutchcoders/transfer.sh/server/storage"
)

const (
	// YtDlpDefaultBinary is the yt-dlp executable looked up in $PATH by default
	YtDlpDefaultBinary = "yt-dlp"

	// YtDlpDefaultTimeout is the maximum duration of a single yt-dlp run
	YtDlpDefaultTimeout = 10 * time.Minute

	// YtDlpDefaultMaxConcurrent is the number of yt-dlp downloads running at
	// the same time, the requests over that limit wait for a free slot
	YtDlpDefaultMaxConcurrent = 1

	// YtDlpDefaultAudioFormat is used when only the audio track is requested
	YtDlpDefaultAudioFormat = "mp3"

	// the request body is only holding a url and a few options
	ytDlpMaxRequestBody = 1 << 16

	// only the tail of the yt-dlp stderr is reported back
	ytDlpMaxErrorOutput = 2048
)

// ytDlpRequest holds the parameters of a yt-dlp download request
type ytDlpRequest struct {
	// URL is the address yt-dlp is asked to download from
	URL string `json:"url"`
	// Filename overrides the name yt-dlp derived from the media metadata
	Filename string `json:"filename"`
	// Format is a yt-dlp format selector, e.g. "bestvideo+bestaudio"
	Format string `json:"format"`
	// AudioOnly extracts the audio track instead of storing the full media
	AudioOnly bool `json:"audio_only"`
	// AudioFormat is the container the audio track is converted to
	AudioFormat string `json:"audio_format"`
}

// ytDlpResponse is returned for clients asking for a json answer
type ytDlpResponse struct {
	URL         string `json:"url"`
	DeleteURL   string `json:"delete_url"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// applyYtDlpValues overrides the request with the values found in a form
func applyYtDlpValues(req *ytDlpRequest, values url.Values) error {
	for name, target := range map[string]*string{
		"url":          &req.URL,
		"filename":     &req.Filename,
		"format":       &req.Format,
		"audio_format": &req.AudioFormat,
	} {
		if v := values.Get(name); v != "" {
			*target = v
		}
	}

	if v := values.Get("audio_only"); v != "" {
		audioOnly, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("invalid audio_only value")
		}

		req.AudioOnly = audioOnly
	}

	return nil
}

// parseYtDlpRequest reads the download parameters from the query string,
// overridden by the request body when one is provided. The body is either a
// json object, a form or the bare url to download
func parseYtDlpRequest(r *http.Request) (ytDlpRequest, error) {
	var req ytDlpRequest

	if err := applyYtDlpValues(&req, r.URL.Query()); err != nil {
		return req, err
	}

	defer storage.CloseCheck(r.Body)

	body, err := io.ReadAll(io.LimitReader(r.Body, ytDlpMaxRequestBody))
	if err != nil {
		return req, fmt.Errorf("could not read body: %w", err)
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		mediaType = ""
	}

	switch mediaType {
	case "application/json":
		if len(bytes.TrimSpace(body)) == 0 {
			break
		}

		if err := json.Unmarshal(body, &req); err != nil {
			return req, fmt.Errorf("could not decode json body: %w", err)
		}
	case "multipart/form-data":
		r.Body = io.NopCloser(bytes.NewReader(body))

		if err := r.ParseMultipartForm(_24K); err != nil {
			return req, fmt.Errorf("could not parse form: %w", err)
		}

		if err := applyYtDlpValues(&req, r.PostForm); err != nil {
			return req, err
		}
	default:
		// a form when it holds an url field, the bare url to download otherwise
		if values, err := url.ParseQuery(string(body)); err == nil && values.Get("url") != "" {
			if err := applyYtDlpValues(&req, values); err != nil {
				return req, err
			}
		} else if v := strings.TrimSpace(string(body)); v != "" {
			req.URL = v
		}
	}

	req.URL = strings.TrimSpace(req.URL)

	return req, nil
}

// validateYtDlpURL makes sure only remote http(s) locations are passed to yt-dlp
func validateYtDlpURL(rawURL string) error {
	if rawURL == "" {
		return errors.New("no url provided")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("could not parse url")
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http and https urls are supported")
	}

	if u.Host == "" {
		return errors.New("url is missing a host")
	}

	return nil
}

// ytDlpArgs builds the yt-dlp command line for a single download into dir
func (s *Server) ytDlpArgs(dir string, req ytDlpRequest) []string {
	args := []string{
		"--no-playlist",
		"--no-progress",
		"--no-continue",
		"--restrict-filenames",
		"--paths", dir,
		"--output", "%(title).150B.%(ext)s",
		"--print", "after_move:filepath",
	}

	format := req.Format
	if format == "" {
		format = s.ytDlpFormat
	}

	if format != "" {
		args = append(args, "--format", format)
	}

	if req.AudioOnly {
		audioFormat := req.AudioFormat
		if audioFormat == "" {
			audioFormat = YtDlpDefaultAudioFormat
		}

		args = append(args, "--extract-audio", "--audio-format", audioFormat)
	}

	if s.ytDlpMaxFilesize != "" {
		args = append(args, "--max-filesize", s.ytDlpMaxFilesize)
	}

	// everything after the separator is treated as an url, never as a flag
	return append(args, "--", req.URL)
}

// ytDlpDownload runs yt-dlp into a temporary directory and returns the path of
// the downloaded file, together with the directory the caller has to remove
func (s *Server) ytDlpDownload(ctx context.Context, req ytDlpRequest) (filePath string, dir string, err error) {
	dir, err = os.MkdirTemp(s.tempPath, "transfer-ytdlp-")
	if err != nil {
		return "", "", fmt.Errorf("could not create temp dir: %w", err)
	}

	timeout := s.ytDlpTimeout
	if timeout <= 0 {
		timeout = YtDlpDefaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	binary := s.ytDlpPath
	if binary == "" {
		binary = YtDlpDefaultBinary
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	cmd := exec.CommandContext(ctx, binary, s.ytDlpArgs(dir, req)...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// yt-dlp writes its cache next to $HOME, keep it inside the temp dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CACHE_HOME="+dir)

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			return "", dir, fmt.Errorf("yt-dlp timed out after %s", timeout)
		}

		return "", dir, fmt.Errorf("yt-dlp failed: %s", ytDlpErrorOutput(stderr.String(), err))
	}

	filePath, err = ytDlpResultFile(dir, stdout.String())
	if err != nil {
		return "", dir, err
	}

	return filePath, dir, nil
}

// ytDlpErrorOutput picks the most meaningful part of a failed yt-dlp run
func ytDlpErrorOutput(stderr string, err error) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err.Error()
	}

	if len(stderr) > ytDlpMaxErrorOutput {
		stderr = stderr[len(stderr)-ytDlpMaxErrorOutput:]
	}

	lines := strings.Split(stderr, "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}

// ytDlpResultFile resolves the downloaded file, preferring the path yt-dlp
// printed and falling back to the biggest file left in the temp dir
func ytDlpResultFile(dir, stdout string) (string, error) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if !filepath.IsAbs(line) {
			line = filepath.Join(dir, line)
		}

		if fi, err := os.Stat(line); err == nil && fi.Mode().IsRegular() {
			return line, nil
		}
	}

	var (
		biggest     string
		biggestSize int64 = -1
	)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("could not read download dir: %w", err)
	}

	for _, entry := range entries {
		fi, err := entry.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}

		if fi.Size() > biggestSize {
			biggest, biggestSize = filepath.Join(dir, entry.Name()), fi.Size()
		}
	}

	if biggest == "" {
		return "", errors.New("yt-dlp did not produce any file")
	}

	return biggest, nil
}

// acquireYtDlpSlot limits the number of yt-dlp processes running at the same
// time. The requests over that limit queue up, waiting for a running download
// to release its slot, and only give up when ctx is done, i.e. when the client
// closed the connection or the server is shutting down
func (s *Server) acquireYtDlpSlot(ctx context.Context) error {
	select {
	case s.ytDlpSlots <- struct{}{}:
		return nil
	default:
	}

	s.logger.Print("Waiting for a free yt-dlp slot")

	select {
	case s.ytDlpSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseYtDlpSlot hands the slot taken by acquireYtDlpSlot to the next waiter
func (s *Server) releaseYtDlpSlot() {
	<-s.ytDlpSlots
}

// ytDlpHandler downloads the media behind the provided url with yt-dlp, stores
// it like any other upload and answers with its temporary download url
func (s *Server) ytDlpHandler(w http.ResponseWriter, r *http.Request) {
	if !s.ytDlpEnabled {
		http.Error(w, "yt-dlp support is not enabled", http.StatusNotImplemented)
		return
	}

	req, err := parseYtDlpRequest(r)
	if err != nil {
		s.logger.Printf("%s", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := validateYtDlpURL(req.URL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.acquireYtDlpSlot(r.Context()); err != nil {
		s.logger.Printf("Gave up waiting for a yt-dlp slot: %s", err)
		http.Error(w, "gave up waiting for a free yt-dlp slot", http.StatusRequestTimeout)
		return
	}
	defer s.releaseYtDlpSlot()

	s.logger.Printf("Downloading with yt-dlp: %s", req.URL)

	filePath, dir, err := s.ytDlpDownload(r.Context(), req)
	if dir != "" {
		defer func() {
			if err := os.RemoveAll(dir); err != nil {
				s.logger.Printf("Error removing yt-dlp temp dir: %s (%s)", err, dir)
			}
		}()
	}

	if err != nil {
		s.logger.Printf("%s", err.Error())
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		s.logger.Printf("%s", err.Error())
		http.Error(w, "Could not stat downloaded file", http.StatusInternalServerError)
		return
	}

	contentLength := fi.Size()

	if contentLength == 0 {
		s.logger.Print("yt-dlp produced an empty file")
		http.Error(w, "Could not upload empty file", http.StatusBadGateway)
		return
	}

	if s.maxUploadSize > 0 && contentLength > s.maxUploadSize {
		s.logger.Print("Entity too large")
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}

	if s.performClamavPrescan {
		status, err := s.performScan(filePath)
		if err != nil {
			s.logger.Printf("%s", err.Error())
			http.Error(w, "Could not perform prescan", http.StatusInternalServerError)
			return
		}

		if status != clamavScanStatusOK {
			s.logger.Printf("prescan positive: %s", status)
			http.Error(w, "Clamav prescan found a virus", http.StatusPreconditionFailed)
			return
		}
	}

	filename := req.Filename
	if filename == "" {
		filename = filepath.Base(filePath)
	}

	filename = sanitize(filename)

	contentType := mime.TypeByExtension(filepath.Ext(filename))

	token := token(s.randomTokenLength)

	metadata := metadataForRequest(contentType, contentLength, s.randomTokenLength, r)

	buffer := &bytes.Buffer{}
	if err := json.NewEncoder(buffer).Encode(metadata); err != nil {
		s.logger.Printf("%s", err.Error())
		http.Error(w, "Could not encode metadata", http.StatusInternalServerError)
		return
	} else if !metadata.MaxDate.IsZero() && time.Now().After(metadata.MaxDate) {
		s.logger.Print("Invalid MaxDate")
		http.Error(w, "Invalid MaxDate, make sure Max-Days is smaller than 290 years", http.StatusBadRequest)
		return
	} else if err := s.storage.Put(r.Context(), token, fmt.Sprintf("%s.metadata", filename), buffer, "text/json", uint64(buffer.Len())); err != nil {
		s.logger.Printf("%s", err.Error())
		http.Error(w, "Could not save metadata", http.StatusInternalServerError)
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		s.logger.Printf("%s", err.Error())
		http.Error(w, "Could not open downloaded file", http.StatusInternalServerError)
		return
	}
	defer storage.CloseCheck(file)

	s.logger.Printf("Uploading %s %s %d %s", token, filename, contentLength, contentType)

	reader, err := attachEncryptionReader(file, r.Header.Get("X-Encrypt-Password"))
	if err != nil {
		http.Error(w, "Could not crypt file", http.StatusInternalServerError)
		return
	}

	if err := s.storage.Put(r.Context(), token, filename, reader, contentType, uint64(contentLength)); err != nil {
		s.logger.Printf("Error putting new file: %s", err.Error())
		http.Error(w, "Could not save file", http.StatusInternalServerError)
		return
	}

	escapedFilename := url.PathEscape(filename)
	relativeURL, _ := url.Parse(path.Join(s.proxyPath, token, escapedFilename))
	deleteURL, _ := url.Parse(path.Join(s.proxyPath, token, escapedFilename, metadata.DeletionToken))

	downloadURL := resolveURL(r, relativeURL, s.proxyPort)
	deletionURL := resolveURL(r, deleteURL, s.proxyPort)

	w.Header().Set("X-Url-Delete", deletionURL)

	if acceptsJSON(r.Header) {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(ytDlpResponse{
			URL:         downloadURL,
			DeleteURL:   deletionURL,
			Filename:    filename,
			ContentType: contentType,
			Size:        contentLength,
		}); err != nil {
			s.logger.Printf("%s", err.Error())
		}

		return
	}

	w.Header().Set("Content-Type", "text/plain")

	_, _ = w.Write([]byte(downloadURL))
}
