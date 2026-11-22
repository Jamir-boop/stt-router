package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Jamir-boop/stt-router/internal/config"
)

// benchUpstream reads the whole body, like a real upstream, and answers a fixed transcript.
func benchUpstream(b *testing.B) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, groqOK)
	}))
	b.Cleanup(srv.Close)
	return srv
}

func benchBody(size int) ([]byte, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "whisper")
	mw.WriteField("response_format", "srt")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write(make([]byte, size))
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func post(b *testing.B, client *http.Client, url string, body []byte, ct string) {
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		b.Fatalf("status %d", resp.StatusCode)
	}
}

// BenchmarkDirect posts straight to the fake upstream: the baseline.
// BenchmarkRouter posts through the router to the same fake upstream.
// The difference is the router overhead.
func BenchmarkDirect(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 16 << 20} {
		b.Run(sizeName(size), func(b *testing.B) {
			up := benchUpstream(b)
			body, ct := benchBody(size)
			client := &http.Client{}
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				post(b, client, up.URL+"/audio/transcriptions", body, ct)
			}
		})
	}
}

func BenchmarkRouter(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 16 << 20} {
		b.Run(sizeName(size), func(b *testing.B) {
			up := benchUpstream(b)
			cfg := baseConfig(up.URL, "a", "b")
			dir := b.TempDir()
			s, err := New(cfg, Options{StatePath: filepath.Join(dir, "state.json"), Listen: config.DefaultListen,
				TempDir: dir, Logger: log.New(io.Discard, "", 0)})
			if err != nil {
				b.Fatal(err)
			}
			srv := httptest.NewServer(s.Handler())
			b.Cleanup(func() { srv.Close(); s.Close() })
			body, ct := benchBody(size)
			client := &http.Client{}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				post(b, client, srv.URL+"/v1/audio/transcriptions", body, ct)
			}
		})
	}
}

func sizeName(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%dMB", n>>20)
	}
	return fmt.Sprintf("%dKB", n>>10)
}
