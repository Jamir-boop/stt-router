package upstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	cases := []struct {
		name   string
		typ    string
		status int
		header http.Header
		body   string
		class  Class
		retry  time.Duration
	}{
		{"ok", "openai", 200, nil, "", OK, 0},
		{"429 seconds", "openai", 429, h("120"), "", RateLimited, 120 * time.Second},
		{"429 date", "openai", 429, h(now.Add(90 * time.Second).Format(http.TimeFormat)), "", RateLimited, 90 * time.Second},
		{"429 body", "openai", 429, nil, `{"error":{"message":"Rate limit reached. Please try again in 1m2.5s. Visit"}}`, RateLimited, 62500 * time.Millisecond},
		{"429 body ms", "openai", 429, nil, `try again in 450ms`, RateLimited, 450 * time.Millisecond},
		{"429 none", "openai", 429, nil, `slow down`, RateLimited, 0},
		{"401", "openai", 401, nil, "", Invalid, 0},
		{"403", "openai", 403, nil, "", Invalid, 0},
		{"402", "openai", 402, nil, "", Quota, 0},
		{"eleven quota", "elevenlabs", 401, nil, `{"detail":{"status":"quota_exceeded","message":"no credits"}}`, Quota, 0},
		{"500", "openai", 500, nil, "", ServerError, 0},
		{"503", "elevenlabs", 503, nil, "", ServerError, 0},
		{"408", "openai", 408, nil, "", ServerError, 0},
		{"400", "openai", 400, nil, "", ClientError, 0},
		{"413", "openai", 413, nil, "", ClientError, 0},
	}
	for _, tc := range cases {
		c, r := Classify(tc.typ, tc.status, tc.header, []byte(tc.body), now)
		if c != tc.class || r != tc.retry {
			t.Errorf("%s: got %v %v, want %v %v", tc.name, c, r, tc.class, tc.retry)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"bad file","type":"invalid_request_error"}}`: "bad file",
		`{"detail":{"status":"invalid_audio","message":"cannot decode"}}`: "invalid_audio: cannot decode",
		`{"detail":"Not found"}`: "Not found",
		`plain text`:             "plain text",
		``:                       "upstream returned HTTP 418",
	}
	for body, want := range cases {
		if got := ErrorMessage(418, []byte(body)); got != want {
			t.Errorf("ErrorMessage(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestRequestBodyLengthAndReplay(t *testing.T) {
	data := []byte("RIFF-audio-bytes")
	r := &Request{Model: "m", Language: "en", File: File{Name: `a"b.wav`, Size: int64(len(data)), Open: func() io.Reader { return bytes.NewReader(data) }}}
	req, err := NewHTTPRequest(context.Background(), "openai", "http://x/v1", "k", "stt-router/test", r)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(req.Body)
	if int64(len(b1)) != req.ContentLength {
		t.Fatalf("ContentLength %d, body %d", req.ContentLength, len(b1))
	}
	rc, _ := req.GetBody()
	b2, _ := io.ReadAll(rc)
	if !bytes.Equal(b1, b2) {
		t.Fatal("GetBody gives a different body")
	}
	if req.URL.String() != "http://x/v1/audio/transcriptions" || req.Header.Get("Authorization") != "Bearer k" {
		t.Fatalf("url %s auth %q", req.URL, req.Header.Get("Authorization"))
	}
	req.Body, _ = req.GetBody()
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	if req.FormValue("response_format") != "verbose_json" || req.MultipartForm.File["file"][0].Filename != `a"b.wav` {
		t.Fatalf("form %v", req.MultipartForm.Value)
	}
}

func TestParseElevenLabsDurationFallback(t *testing.T) {
	tr, err := Parse("elevenlabs", []byte(`{"text":"a b","language_code":"eng","words":[
		{"text":"a","start":0,"end":0.5,"type":"word"},{"text":" ","start":0.5,"end":0.6,"type":"spacing"},
		{"text":"(laughs)","start":0.6,"end":1,"type":"audio_event"},{"text":"b","start":1,"end":2.25,"type":"word"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Duration != 2.25 || len(tr.Words) != 2 || tr.Language != "eng" {
		t.Fatalf("got %+v", tr)
	}
	if len(tr.Segments) != 1 || tr.Segments[0].Text != "a (laughs)b" {
		t.Fatalf("segments %+v", tr.Segments)
	}
}
