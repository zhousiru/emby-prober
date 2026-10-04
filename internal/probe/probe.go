package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhousiru/emby-prober/internal/config"
	"github.com/zhousiru/emby-prober/internal/emby"
)

type Result struct {
	Node     string  `json:"node"`
	Sample   int     `json:"sample"`
	Status   int     `json:"http_status"`
	Bytes    int64   `json:"bytes"`
	Seconds  float64 `json:"seconds"`
	TTFB     float64 `json:"ttfb_seconds"`
	Mbps     float64 `json:"mbps"`
	Valid    bool    `json:"valid"`
	TimedOut bool    `json:"timed_out"`
	Error    string  `json:"error,omitempty"`
	// FinalURL is used only to determine whether a 401 came from Emby or a CDN.
	FinalURL string `json:"-"`
}

func Measure(ctx context.Context, proxyURL string, target emby.Target, cfg config.Probe) Result {
	var result Result
	transport, err := emby.NewTransport(proxyURL)
	if err != nil {
		result.Error = "invalid test proxy"
		return result
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: emby.RedirectPolicy}
	startByte := cfg.Offset
	if target.Size > 0 && startByte+cfg.MaxBytes > target.Size {
		startByte = 0
	}
	maxBytes := cfg.MaxBytes
	if target.Size > 0 && target.Size-startByte < maxBytes {
		maxBytes = target.Size - startByte
	}
	if maxBytes < cfg.MinBytes {
		result.Error = "video is smaller than min_bytes"
		return result
	}
	sampleCtx, cancel := context.WithTimeout(ctx, cfg.Timeout.Value())
	defer cancel()
	started := time.Now()
	var firstByte atomic.Int64
	sampleCtx = httptrace.WithClientTrace(sampleCtx, &httptrace.ClientTrace{GotFirstResponseByte: func() { firstByte.Store(time.Since(started).Nanoseconds()) }})
	req, err := http.NewRequestWithContext(sampleCtx, http.MethodGet, target.URL, nil)
	if err != nil {
		result.Error = "invalid stream URL"
		return result
	}
	req.Header = target.Headers.Clone()
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", startByte, startByte+maxBytes-1))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		result.Seconds = time.Since(started).Seconds()
		result.TimedOut = errors.Is(sampleCtx.Err(), context.DeadlineExceeded)
		result.Error = "stream request failed (network, timeout or redirect)"
		return result
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode
	result.FinalURL = resp.Request.URL.String()
	result.TTFB = float64(firstByte.Load()) / 1e9
	finish := func() {
		result.Seconds = time.Since(started).Seconds()
		if result.Seconds > 0 {
			result.Mbps = float64(result.Bytes) * 8 / result.Seconds / 1e6
		}
	}
	if resp.StatusCode != http.StatusPartialContent {
		finish()
		result.Error = fmt.Sprintf("expected HTTP 206, got %d", resp.StatusCode)
		return result
	}
	var lo, hi int64
	var total string
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%s", &lo, &hi, &total); err != nil || lo != startByte || hi < lo || hi >= startByte+maxBytes {
		finish()
		result.Error = "invalid or mismatched Content-Range"
		return result
	}
	expected := hi - lo + 1
	if resp.ContentLength >= 0 && resp.ContentLength != expected {
		finish()
		result.Error = "Content-Length disagrees with Content-Range"
		return result
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/") || strings.Contains(contentType, "json") || strings.Contains(contentType, "mpegurl") {
		finish()
		result.Error = "response is not a video byte stream"
		return result
	}
	result.Bytes, err = io.Copy(io.Discard, io.LimitReader(resp.Body, expected))
	finish()
	if ctx.Err() != nil {
		result.Error = "probe canceled"
		return result
	}
	if err != nil {
		if !errors.Is(sampleCtx.Err(), context.DeadlineExceeded) {
			result.Error = "stream interrupted before sample finished"
			return result
		}
		result.TimedOut = true
	} else if result.Bytes != expected {
		result.Error = "stream ended before advertised byte range"
		return result
	}
	if result.Bytes < cfg.MinBytes {
		result.Error = "insufficient data before sample deadline"
		return result
	}
	result.Valid = true
	return result
}
