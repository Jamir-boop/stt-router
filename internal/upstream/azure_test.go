package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func azureRequest(t *testing.T, r *Request) *http.Request {
	t.Helper()
	data := []byte("RIFF....WAVEfmt ")
	if r.File.Open == nil {
		r.File = File{Name: "audio.wav", ContentType: "audio/wav", Size: int64(len(data)), Open: func() io.Reader { return bytes.NewReader(data) }}
	}
	if r.Model == "" {
		r.Model = "MAI-Transcribe-2"
	}
	req, err := NewHTTPRequest(context.Background(), "azure-speech", "https://res.example.com", "az-key", "stt-router/test", r)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestAzureRequest(t *testing.T) {
	req := azureRequest(t, &Request{})
	if got := req.URL.String(); got != "https://res.example.com/speechtotext/transcriptions:transcribe?api-version=2025-10-15" {
		t.Errorf("url %s", got)
	}
	if req.Header.Get("Ocp-Apim-Subscription-Key") != "az-key" || req.Header.Get("Authorization") != "" {
		t.Errorf("headers %v", req.Header)
	}
	b, _ := io.ReadAll(req.Body)
	if int64(len(b)) != req.ContentLength {
		t.Fatalf("ContentLength %d, body %d", req.ContentLength, len(b))
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	mr, err := req.MultipartReader()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(p)
		names = append(names, p.FormName())
		switch p.FormName() {
		case "audio":
			if p.FileName() != "audio.wav" || p.Header.Get("Content-Type") != "audio/wav" || string(body) != "RIFF....WAVEfmt " {
				t.Errorf("audio part %q %q %q", p.FileName(), p.Header.Get("Content-Type"), body)
			}
		case "definition":
			if p.Header.Get("Content-Type") != "application/json" || p.FileName() != "" {
				t.Errorf("definition part headers %v", p.Header)
			}
		}
	}
	if len(names) != 2 || names[0] != "audio" || names[1] != "definition" {
		t.Errorf("parts %v", names)
	}
}

func TestAzureDefinition(t *testing.T) {
	cases := []struct {
		name string
		r    Request
		want string
	}{
		{"defaults", Request{Prompt: "names", Temperature: "0.2"},
			`{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"segment"}},"profanityFilterMode":"None"}`},
		{"language and words", Request{Language: "es", Words: true},
			`{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"word"}},"locales":["es"],"profanityFilterMode":"None"}`},
		{"timestamp format", Request{Timestamps: true},
			`{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"word"}},"profanityFilterMode":"None"}`},
		{"diarization", Request{Extra: []Field{{"diarize", "true"}, {"num_speakers", "3"}}},
			`{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"segment"}},"diarization":{"enabled":true,"maxSpeakers":3},"profanityFilterMode":"None"}`},
		{"diarization without count", Request{Extra: []Field{{"diarize", "true"}}},
			`{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"segment"}},"diarization":{"enabled":true},"profanityFilterMode":"None"}`},
		{"keyterms and style", Request{Prompt: "a, b", Extra: []Field{{"keyterms", "Ana"}, {"transcribe_style", "clean"}, {"keyterms", "Bob"}, {"tag_audio_events", "true"}}},
			`{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"segment","transcribeStyle":"clean"}},"phraseList":{"phrases":["Ana","Bob"]},"profanityFilterMode":"None"}`},
	}
	for _, tc := range cases {
		req := azureRequest(t, &tc.r)
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got := req.FormValue("definition"); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
	_, err := NewHTTPRequest(context.Background(), "azure-speech", "https://x", "k", "ua", &Request{Extra: []Field{{"num_speakers", "two"}}, File: File{Open: func() io.Reader { return nil }}})
	var re *RequestError
	if !errors.As(err, &re) {
		t.Errorf("bad num_speakers: err %v, want a RequestError", err)
	}
}

func TestParseAzureTolerant(t *testing.T) {
	// No words, speaker as a string, no durationMilliseconds, no combinedPhrases.
	tr, err := Parse("azure-speech", []byte(`{"phrases":[
		{"offsetMilliseconds":0,"durationMilliseconds":1000,"text":"uno","locale":"es-ES","speaker":"1"},
		{"offsetMilliseconds":1500,"durationMilliseconds":750,"text":" dos ","locale":"es-ES","speaker":"Guest-2"},
		{"offsetMilliseconds":2500,"durationMilliseconds":500,"text":"tres","speaker":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "uno dos tres" || tr.Language != "es-ES" || tr.Duration != 3 || len(tr.Words) != 0 {
		t.Fatalf("got %+v", tr)
	}
	if len(tr.Segments) != 3 || tr.Segments[0].Speaker != "speaker_1" || tr.Segments[1].Speaker != "Guest-2" ||
		tr.Segments[2].Speaker != "" || tr.Segments[1].Text != "dos" || tr.Segments[1].End != 2.25 {
		t.Fatalf("segments %+v", tr.Segments)
	}
	// Silence: no phrases at all.
	tr, err = Parse("azure-speech", []byte(`{"durationMilliseconds":500,"combinedPhrases":[],"phrases":[]}`))
	if err != nil || tr.Text != "" || tr.Duration != 0.5 {
		t.Fatalf("empty: %+v %v", tr, err)
	}
	if _, err := Parse("azure-speech", []byte(`{`)); err == nil {
		t.Fatal("invalid JSON must fail")
	}
}

func TestDetectAudio(t *testing.T) {
	cases := map[string][]byte{
		"wav":  []byte("RIFF\x24\x00\x00\x00WAVEfmt "),
		"flac": []byte("fLaC\x00\x00\x00\x22"),
		"mp3":  []byte("ID3\x04\x00\x00\x00\x00\x00\x00"),
		"ogg":  []byte("OggS\x00\x02\x00\x00"),
		"webm": {0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81},
		"mp4":  []byte("\x00\x00\x00\x20ftypM4A "),
		"":     []byte("RIFF\x24\x00\x00\x00AVI "),
	}
	for want, head := range cases {
		if got := DetectAudio(head); got != want {
			t.Errorf("DetectAudio(%q) = %q, want %q", head, got, want)
		}
	}
	if got := DetectAudio([]byte{0xFF, 0xFB, 0x90, 0x64}); got != "mp3" { // MPEG-1 Layer III frame
		t.Errorf("bare mp3 frame: %q", got)
	}
	if got := DetectAudio([]byte{0xFF, 0xF1, 0x50, 0x80}); got != "" { // ADTS AAC, layer 0
		t.Errorf("ADTS AAC: %q, want unknown", got)
	}
	if got := DetectAudio(nil); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestAzureErrors(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		status int
		header http.Header
		body   string
		class  Class
		retry  time.Duration
		msg    string
	}{
		{401, nil, `{"error":{"code":"401","message":"Access denied due to invalid subscription key or wrong API endpoint."}}`,
			Invalid, 0, "Access denied due to invalid subscription key or wrong API endpoint."},
		{429, http.Header{"Retry-After": {"7"}}, `{"error":{"code":"429","message":"Rate limit is exceeded. Try again in 7 seconds."}}`,
			RateLimited, 7 * time.Second, "Rate limit is exceeded. Try again in 7 seconds."},
		{400, nil, `{"error":{"code":"InvalidRequest","message":"Audio format is not supported.","innererror":{"code":"InvalidAudioFormat","message":"x"}}}`,
			ClientError, 0, "Audio format is not supported."},
		{500, nil, `{"error":{"code":"InternalServerError","message":"An internal error occurred."}}`,
			ServerError, 0, "An internal error occurred."},
	}
	for _, tc := range cases {
		c, r := Classify("azure-speech", tc.status, tc.header, []byte(tc.body), now)
		if c != tc.class || r != tc.retry {
			t.Errorf("%d: got %v %v, want %v %v", tc.status, c, r, tc.class, tc.retry)
		}
		if got := ErrorMessage(tc.status, []byte(tc.body)); got != tc.msg {
			t.Errorf("%d: message %q, want %q", tc.status, got, tc.msg)
		}
	}
}
