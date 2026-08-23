package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "gopkg.in/check.v1"
)

var _ = Suite(&suiteYtDlp{})

type suiteYtDlp struct{}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func (s *suiteYtDlp) TestParseBareURLBody(c *C) {
	req := httptest.NewRequest("POST", "http://test/ytdlp", strings.NewReader("https://example.com/watch?v=1\n"))

	parsed, err := parseYtDlpRequest(req)
	c.Assert(err, IsNil)
	c.Assert(parsed.URL, Equals, "https://example.com/watch?v=1")
}

func (s *suiteYtDlp) TestParseJSONBody(c *C) {
	body := `{"url":"https://example.com/v","filename":"song.mp3","audio_only":true,"format":"bestaudio"}`
	req := httptest.NewRequest("POST", "http://test/ytdlp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	parsed, err := parseYtDlpRequest(req)
	c.Assert(err, IsNil)
	c.Assert(parsed.URL, Equals, "https://example.com/v")
	c.Assert(parsed.Filename, Equals, "song.mp3")
	c.Assert(parsed.Format, Equals, "bestaudio")
	c.Assert(parsed.AudioOnly, Equals, true)
}

func (s *suiteYtDlp) TestParseFormBody(c *C) {
	body := "url=https%3A%2F%2Fexample.com%2Fv&filename=clip.mp4&audio_only=false"
	req := httptest.NewRequest("POST", "http://test/ytdlp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	parsed, err := parseYtDlpRequest(req)
	c.Assert(err, IsNil)
	c.Assert(parsed.URL, Equals, "https://example.com/v")
	c.Assert(parsed.Filename, Equals, "clip.mp4")
	c.Assert(parsed.AudioOnly, Equals, false)
}

func (s *suiteYtDlp) TestBodyOverridesQuery(c *C) {
	req := httptest.NewRequest("POST", "http://test/ytdlp?url=https://example.com/query&audio_only=true", strings.NewReader("https://example.com/body"))

	parsed, err := parseYtDlpRequest(req)
	c.Assert(err, IsNil)
	c.Assert(parsed.URL, Equals, "https://example.com/body")
	c.Assert(parsed.AudioOnly, Equals, true)
}

func (s *suiteYtDlp) TestParseInvalidAudioOnly(c *C) {
	req := httptest.NewRequest("POST", "http://test/ytdlp?audio_only=maybe", nil)

	_, err := parseYtDlpRequest(req)
	c.Assert(err, NotNil)
}

func (s *suiteYtDlp) TestValidateURL(c *C) {
	c.Assert(validateYtDlpURL("https://example.com/v"), IsNil)
	c.Assert(validateYtDlpURL("http://example.com/v"), IsNil)
	c.Assert(validateYtDlpURL(""), NotNil)
	c.Assert(validateYtDlpURL("file:///etc/passwd"), NotNil)
	c.Assert(validateYtDlpURL("--version"), NotNil)
	c.Assert(validateYtDlpURL("https://"), NotNil)
}

func (s *suiteYtDlp) TestArgs(c *C) {
	srvr, err := New(YtDlpFormat("worst"), YtDlpMaxFilesize("10M"))
	c.Assert(err, IsNil)

	args := srvr.ytDlpArgs("/tmp/dl", ytDlpRequest{URL: "https://example.com/v"})

	c.Assert(args[len(args)-2], Equals, "--")
	c.Assert(args[len(args)-1], Equals, "https://example.com/v")
	c.Assert(strings.Join(args, " "), Matches, ".*--format worst.*")
	c.Assert(strings.Join(args, " "), Matches, ".*--max-filesize 10M.*")
	c.Assert(strings.Join(args, " "), Matches, ".*--paths /tmp/dl.*")

	// a per request format wins over the configured default
	args = srvr.ytDlpArgs("/tmp/dl", ytDlpRequest{URL: "https://example.com/v", Format: "best", AudioOnly: true})
	c.Assert(strings.Join(args, " "), Matches, ".*--format best.*")
	c.Assert(strings.Join(args, " "), Matches, ".*--extract-audio --audio-format mp3.*")
}

func (s *suiteYtDlp) TestResultFileFromPrintedPath(c *C) {
	dir := c.MkDir()

	expected := filepath.Join(dir, "video.mp4")
	c.Assert(os.WriteFile(expected, []byte("data"), 0600), IsNil)

	found, err := ytDlpResultFile(dir, "[generic] extracting\n"+expected+"\n")
	c.Assert(err, IsNil)
	c.Assert(found, Equals, expected)
}

func (s *suiteYtDlp) TestResultFileFallsBackToBiggestFile(c *C) {
	dir := c.MkDir()

	small := filepath.Join(dir, "thumb.jpg")
	big := filepath.Join(dir, "video.mp4")
	c.Assert(os.WriteFile(small, []byte("s"), 0600), IsNil)
	c.Assert(os.WriteFile(big, []byte("bigger content"), 0600), IsNil)

	found, err := ytDlpResultFile(dir, "")
	c.Assert(err, IsNil)
	c.Assert(found, Equals, big)
}

func (s *suiteYtDlp) TestResultFileWithoutAnyFile(c *C) {
	_, err := ytDlpResultFile(c.MkDir(), "")
	c.Assert(err, NotNil)
}

func (s *suiteYtDlp) TestHandlerDisabled(c *C) {
	srvr, err := New(Logger(discardLogger()))
	c.Assert(err, IsNil)

	req := httptest.NewRequest("POST", "http://test/ytdlp", strings.NewReader("https://example.com/v"))
	w := httptest.NewRecorder()

	srvr.ytDlpHandler(w, req)

	c.Assert(w.Result().StatusCode, Equals, http.StatusNotImplemented)
}

func (s *suiteYtDlp) TestHandlerRejectsInvalidURL(c *C) {
	srvr, err := New(EnableYtDlp(), Logger(discardLogger()))
	c.Assert(err, IsNil)

	req := httptest.NewRequest("POST", "http://test/ytdlp", strings.NewReader("file:///etc/passwd"))
	w := httptest.NewRecorder()

	srvr.ytDlpHandler(w, req)

	c.Assert(w.Result().StatusCode, Equals, http.StatusBadRequest)
}

func (s *suiteYtDlp) TestAcquireSlotWaitsForARelease(c *C) {
	srvr, err := New(YtDlpMaxConcurrent(1), Logger(discardLogger()))
	c.Assert(err, IsNil)

	c.Assert(srvr.acquireYtDlpSlot(context.Background()), IsNil)

	acquired := make(chan error, 1)
	go func() {
		acquired <- srvr.acquireYtDlpSlot(context.Background())
	}()

	select {
	case <-acquired:
		c.Fatal("second download did not wait for the running one")
	case <-time.After(50 * time.Millisecond):
	}

	srvr.releaseYtDlpSlot()

	select {
	case err := <-acquired:
		c.Assert(err, IsNil)
		srvr.releaseYtDlpSlot()
	case <-time.After(time.Second):
		c.Fatal("second download was not started after the slot was released")
	}
}

func (s *suiteYtDlp) TestAcquireSlotGivesUpWithTheClient(c *C) {
	srvr, err := New(YtDlpMaxConcurrent(1), Logger(discardLogger()))
	c.Assert(err, IsNil)

	c.Assert(srvr.acquireYtDlpSlot(context.Background()), IsNil)
	defer srvr.releaseYtDlpSlot()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.Assert(srvr.acquireYtDlpSlot(ctx), Equals, context.Canceled)
}

func (s *suiteYtDlp) TestHandlerGivesUpWhenTheClientIsGone(c *C) {
	srvr, err := New(EnableYtDlp(), YtDlpMaxConcurrent(1), Logger(discardLogger()))
	c.Assert(err, IsNil)

	c.Assert(srvr.acquireYtDlpSlot(context.Background()), IsNil)
	defer srvr.releaseYtDlpSlot()

	ctx, cancel := context.WithCancel(context.Background())

	req := httptest.NewRequest("POST", "http://test/ytdlp", strings.NewReader("https://example.com/v")).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srvr.ytDlpHandler(w, req)
	}()

	select {
	case <-done:
		c.Fatal("handler did not wait for a free slot")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-done:
		c.Assert(w.Result().StatusCode, Equals, http.StatusRequestTimeout)
	case <-time.After(time.Second):
		c.Fatal("handler kept waiting after the client was gone")
	}
}
