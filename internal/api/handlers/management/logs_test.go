package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDecodeLogCursorRejectsUnsafeFiles(t *testing.T) {
	unsafeNames := []string{
		"",
		".",
		"..",
		"../secret",
		"nested/main.log",
		`nested\main.log`,
		"error.log",
	}

	for _, name := range unsafeNames {
		t.Run(name, func(t *testing.T) {
			raw := mustEncodeRawCursor(t, logCursor{
				Version:     logCursorVersion,
				File:        name,
				Fingerprint: "fingerprint",
			})
			if _, err := decodeLogCursor(raw); err == nil {
				t.Fatalf("decodeLogCursor(%q) succeeded, want error", name)
			}
		})
	}

	for _, name := range []string{defaultLogFileName, defaultLogFileName + ".1", "main-2026-06-15T10-00-00.log"} {
		t.Run("allowed_"+name, func(t *testing.T) {
			raw := mustEncodeRawCursor(t, logCursor{
				Version:     logCursorVersion,
				File:        name,
				Fingerprint: "fingerprint",
			})
			if _, err := decodeLogCursor(raw); err != nil {
				t.Fatalf("decodeLogCursor(%q) error = %v", name, err)
			}
		})
	}
}

func TestGetLogsTailLimitReturnsRecentLinesWithCursor(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		"[2026-06-15 10:00:00] first",
		"[2026-06-15 10:00:01] second",
		"[2026-06-15 10:00:02] third",
		"[2026-06-15 10:00:03] fourth",
	}
	writeMainLog(t, dir, strings.Join(lines, "\n")+"\n")

	resp := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?limit=2")
	wantLines := []string{lines[2], lines[3]}
	if !reflect.DeepEqual(resp.Lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", resp.Lines, wantLines)
	}
	if resp.LineCount != len(wantLines) {
		t.Fatalf("line-count = %d, want returned line count %d", resp.LineCount, len(wantLines))
	}
	if resp.NextCursor == "" {
		t.Fatal("next-cursor is empty")
	}
	wantLatest := time.Date(2026, 6, 15, 10, 0, 3, 0, time.Local).Unix()
	if resp.LatestTimestamp != wantLatest {
		t.Fatalf("latest-timestamp = %d, want %d", resp.LatestTimestamp, wantLatest)
	}
}

func TestGetLogsTailLimitDoesNotScanOlderFilesForLineCount(t *testing.T) {
	dir := t.TempDir()
	rotatedPath := filepath.Join(dir, defaultLogFileName+".1")
	if err := os.WriteFile(rotatedPath, []byte(strings.Repeat("x", logScannerMaxBuffer+1)+"\n"), 0o644); err != nil {
		t.Fatalf("write rotated log: %v", err)
	}
	writeMainLog(t, dir, "[2026-06-15 10:00:00] current\n")

	resp := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?limit=1")
	wantLines := []string{"[2026-06-15 10:00:00] current"}
	if !reflect.DeepEqual(resp.Lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", resp.Lines, wantLines)
	}
	if resp.LineCount != len(wantLines) {
		t.Fatalf("line-count = %d, want returned line count %d", resp.LineCount, len(wantLines))
	}
}

func TestGetLogsCursorReturnsOnlyNewCompleteLines(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		"[2026-06-15 10:00:00] first",
		"[2026-06-15 10:00:01] second",
		"[2026-06-15 10:00:02] third",
	}
	writeMainLog(t, dir, strings.Join(lines, "\n")+"\n")
	initial := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?limit=2")
	if initial.NextCursor == "" {
		t.Fatal("initial next-cursor is empty")
	}

	appendMainLog(t, dir, "[2026-06-15 10:00:03] fourth\n")
	resp := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?cursor="+url.QueryEscape(initial.NextCursor)+"&limit=10")
	wantLines := []string{"[2026-06-15 10:00:03] fourth"}
	if !reflect.DeepEqual(resp.Lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", resp.Lines, wantLines)
	}
	if resp.LineCount != 1 {
		t.Fatalf("line-count = %d, want 1", resp.LineCount)
	}
	if resp.CursorReset {
		t.Fatal("cursor-reset = true, want false")
	}
}

func TestGetLogsCursorResetAfterTruncateTailsLimit(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		"[2026-06-15 10:00:00] first",
		"[2026-06-15 10:00:01] second",
		"[2026-06-15 10:00:02] third",
	}
	writeMainLog(t, dir, strings.Join(lines, "\n")+"\n")
	initial := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?limit=3")

	resetLine := "[2026-06-15 10:00:03] reset"
	writeMainLog(t, dir, resetLine+"\n")
	resp := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?cursor="+url.QueryEscape(initial.NextCursor)+"&limit=1")
	if !resp.CursorReset {
		t.Fatal("cursor-reset = false, want true")
	}
	if !reflect.DeepEqual(resp.Lines, []string{resetLine}) {
		t.Fatalf("lines = %#v, want reset tail", resp.Lines)
	}
	if resp.LineCount != 1 {
		t.Fatalf("line-count = %d, want 1", resp.LineCount)
	}
}

func TestGetLogsCursorReadsAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	line1 := "[2026-06-15 10:00:00] first"
	line2 := "[2026-06-15 10:00:01] second"
	line3 := "[2026-06-15 10:00:02] third"
	writeMainLog(t, dir, line1+"\n")
	initial := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?limit=1")

	appendMainLog(t, dir, line2+"\n")
	if err := os.Rename(filepath.Join(dir, defaultLogFileName), filepath.Join(dir, defaultLogFileName+".1")); err != nil {
		t.Fatalf("rotate main log: %v", err)
	}
	writeMainLog(t, dir, line3+"\n")

	resp := performGetLogs(t, newLogsTestHandler(dir, true), "/v0/management/logs?cursor="+url.QueryEscape(initial.NextCursor)+"&limit=10")
	wantLines := []string{line2, line3}
	if !reflect.DeepEqual(resp.Lines, wantLines) {
		t.Fatalf("lines = %#v, want %#v", resp.Lines, wantLines)
	}
	if resp.CursorReset {
		t.Fatal("cursor-reset = true, want false")
	}
}

func mustEncodeRawCursor(t *testing.T, cursor logCursor) string {
	t.Helper()
	raw, err := json.Marshal(cursor)
	if err != nil {
		t.Fatalf("json.Marshal cursor: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

type logsAPIResponse struct {
	Lines           []string `json:"lines"`
	LineCount       int      `json:"line-count"`
	LatestTimestamp int64    `json:"latest-timestamp"`
	NextCursor      string   `json:"next-cursor"`
	CursorReset     bool     `json:"cursor-reset"`
}

func newLogsTestHandler(dir string, loggingToFile bool) *Handler {
	h := NewHandlerWithoutConfigFilePath(&config.Config{LoggingToFile: loggingToFile}, nil)
	h.SetLogDirectory(dir)
	return h
}

func performGetLogs(t *testing.T, h *Handler, target string) logsAPIResponse {
	t.Helper()
	status, body := performGetLogsRaw(t, h, target)
	if status != http.StatusOK {
		t.Fatalf("GetLogs status = %d, body = %s", status, body)
	}
	var resp logsAPIResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Lines == nil {
		resp.Lines = []string{}
	}
	return resp
}

func performGetLogsRaw(t *testing.T, h *Handler, target string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	h.GetLogs(c)
	return rec.Code, rec.Body.String()
}

func writeMainLog(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, defaultLogFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write main log: %v", err)
	}
}

func appendMainLog(t *testing.T, dir, content string) {
	t.Helper()
	file, errOpen := os.OpenFile(filepath.Join(dir, defaultLogFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if errOpen != nil {
		t.Fatalf("open main log: %v", errOpen)
	}
	if _, errWrite := file.WriteString(content); errWrite != nil {
		_ = file.Close()
		t.Fatalf("append main log: %v", errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		t.Fatalf("close main log: %v", errClose)
	}
}
