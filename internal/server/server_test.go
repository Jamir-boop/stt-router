package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// captured is what a fake upstream saw.
type captured struct {
	Path    string
	Header  http.Header
	Fields  map[string][]string
	File    []byte
	Account string
}

// fake is a fake upstream. handle returns status, headers and body per request.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []captured
	handle func(c captured) (int, http.Header, string)
}

func newFake(t *testing.T, handle func(c captured) (int, http.Header, string)) *fake {
	f := &fake{t: t, handle: handle}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("fake upstream: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}
		c := captured{Path: r.URL.Path, Header: r.Header.Clone(), Fields: r.MultipartForm.Value}
		if fh := r.MultipartForm.File["file"]; len(fh) == 1 {
			fr, _ := fh[0].Open()
			c.File, _ = io.ReadAll(fr)
			fr.Close()
		}
		c.Account = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if c.Account == "" {
			c.Account = r.Header.Get("xi-api-key")
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		status, h, body := f.handle(c)
		for k, v := range h {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) accounts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Account)
	}
	return out
}

func (f *fake) last() captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

const groqOK = `{"task":"transcribe","language":"english","duration":3.5,"text":" Hello world. How are you?",
"segments":[{"id":0,"seek":0,"start":0,"end":1.5,"text":" Hello world.","tokens":[1],"temperature":0,"avg_logprob":-0.1,"compression_ratio":1,"no_speech_prob":0.01},
{"id":1,"seek":0,"start":1.5,"end":3.5,"text":" How are you?","tokens":[2],"temperature":0,"avg_logprob":-0.1,"compression_ratio":1,"no_speech_prob":0.01}]}`

func ok(captured) (int, http.Header, string) { return 200, nil, groqOK }

// Test keys are long enough to be masked, so the tests can look for them in responses.
func key(name string) string { return "key-" + name + "-0123456789abcdef" }

type env struct {
	t     *testing.T
	srv   *Server
	http  *httptest.Server
	clock *clock
	cfg   *config.Config
	path  string
	state string
}

func baseConfig(groqURL string, groqAccounts ...string) *config.Config {
	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"groq": {Type: config.TypeOpenAI, BaseURL: groqURL},
		},
		Models: map[string][]config.Target{
			"whisper": {{Provider: "groq", Model: "whisper-large-v3-turbo"}},
		},
	}
	for _, a := range groqAccounts {
		cfg.Providers["groq"].Accounts = append(cfg.Providers["groq"].Accounts, &config.Account{Name: a, APIKey: key(a), Enabled: true})
	}
	cfg.ApplyDefaults()
	return cfg
}

func newEnv(t *testing.T, cfg *config.Config, listen string) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, clock: &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}, cfg: cfg,
		path: filepath.Join(dir, "config.json"), state: filepath.Join(dir, "state.json")}
	if cfg.ManagementKey == "" {
		cfg.ManagementKey = "mgmt-key-0123456789"
	}
	if err := config.Save(e.path, cfg); err != nil {
		t.Fatal(err)
	}
	e.start(listen)
	return e
}

func (e *env) start(listen string) {
	e.t.Helper()
	s, err := New(e.cfg, Options{ConfigPath: e.path, StatePath: e.state, Listen: listen, Version: "test",
		Now: e.clock.Now, TempDir: e.t.TempDir(), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv = s
	e.http = httptest.NewServer(s.Handler())
	e.t.Cleanup(func() { e.http.Close(); s.Close() })
}

// restart stops the server, flushes state and starts a new one from the same files.
func (e *env) restart() {
	e.t.Helper()
	e.http.Close()
	if err := e.srv.Close(); err != nil {
		e.t.Fatal(err)
	}
	cfg, _, err := config.Load(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.cfg = cfg
	e.start("127.0.0.1:8330")
}

type form struct {
	fields [][2]string
	file   []byte
}

func (e *env) transcribe(f form, auth string) *http.Response {
	e.t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, kv := range f.fields {
		mw.WriteField(kv[0], kv[1])
	}
	if f.file != nil {
		fw, _ := mw.CreateFormFile("file", "audio.wav")
		fw.Write(f.file)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", e.http.URL+"/v1/audio/transcriptions", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) simple(model, format string) (int, string, http.Header) {
	e.t.Helper()
	fields := [][2]string{{"model", model}}
	if format != "" {
		fields = append(fields, [2]string{"response_format", format})
	}
	resp := e.transcribe(form{fields: fields, file: []byte("audio")}, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b), resp.Header
}

func (e *env) api(method, path, body string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.http.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+config.Resolve(e.srv.cfg.Load().ManagementKey))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestOpenAIPassThroughAndFormats(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, baseConfig(up.srv.URL+"/openai/v1", "a"), "127.0.0.1:8330")

	resp := e.transcribe(form{fields: [][2]string{
		{"model", "whisper"}, {"language", "en"}, {"prompt", "names: Ana"}, {"temperature", "0.2"},
		{"timestamp_granularities[]", "word"}, {"timestamp_granularities[]", "segment"}, {"diarize", "true"},
	}, file: []byte("RIFFfake")}, "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	c := up.last()
	if c.Path != "/openai/v1/audio/transcriptions" {
		t.Errorf("path %s", c.Path)
	}
	want := map[string][]string{
		"model": {"whisper-large-v3-turbo"}, "language": {"en"}, "prompt": {"names: Ana"}, "temperature": {"0.2"},
		"response_format": {"verbose_json"}, "timestamp_granularities[]": {"segment", "word"},
	}
	for k, v := range want {
		if fmt.Sprint(c.Fields[k]) != fmt.Sprint(v) {
			t.Errorf("field %s = %v, want %v", k, c.Fields[k], v)
		}
	}
	if _, ok := c.Fields["diarize"]; ok {
		t.Error("diarize must not be sent to an openai-type upstream")
	}
	if string(c.File) != "RIFFfake" {
		t.Errorf("file %q", c.File)
	}
	if ua := c.Header.Get("User-Agent"); ua != "stt-router/test" {
		t.Errorf("User-Agent %q", ua)
	}

	formats := map[string]string{
		"":     `{"text":" Hello world. How are you?"}` + "\n",
		"json": `{"text":" Hello world. How are you?"}` + "\n",
		"text": "Hello world. How are you?\n",
		"srt":  "1\n00:00:00,000 --> 00:00:01,500\nHello world.\n\n2\n00:00:01,500 --> 00:00:03,500\nHow are you?\n\n",
		"vtt":  "WEBVTT\n\n00:00:00.000 --> 00:00:01.500\nHello world.\n\n00:00:01.500 --> 00:00:03.500\nHow are you?\n\n",
	}
	for f, want := range formats {
		status, body, _ := e.simple("whisper", f)
		if status != 200 || strings.TrimRight(body, "\n") != strings.TrimRight(want, "\n") {
			t.Errorf("format %q: status %d body %q, want %q", f, status, body, want)
		}
		if got := up.last().Fields["timestamp_granularities[]"]; fmt.Sprint(got) != "[segment]" {
			t.Errorf("format %q: granularities %v", f, got)
		}
	}
	_, body, _ := e.simple("whisper", "verbose_json")
	var v struct {
		Task     string  `json:"task"`
		Duration float64 `json:"duration"`
		Language string  `json:"language"`
		Segments []struct {
			Text string `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Task != "transcribe" || v.Duration != 3.5 || v.Language != "english" || len(v.Segments) != 2 {
		t.Errorf("verbose_json %s (err %v)", body, err)
	}
}

func TestPlainJSONAccount(t *testing.T) {
	up := newFake(t, func(captured) (int, http.Header, string) { return 200, nil, `{"text":"hi there"}` })
	cfg := baseConfig(up.srv.URL, "plain")
	cfg.Providers["groq"].Accounts[0].PlainJSON = true
	e := newEnv(t, cfg, "127.0.0.1:8330")
	status, body, _ := e.simple("whisper", "text")
	if status != 200 || body != "hi there\n" {
		t.Fatalf("text: %d %q", status, body)
	}
	if got := up.last().Fields["response_format"]; fmt.Sprint(got) != "[json]" {
		t.Errorf("response_format %v", got)
	}
	status, body, _ = e.simple("whisper", "srt")
	if status != 400 || !strings.Contains(body, "plain_json") {
		t.Fatalf("srt: %d %s", status, body)
	}
	if len(up.accounts()) != 1 {
		t.Errorf("srt must not reach the upstream")
	}
}

const elevenOK = `{"language_code":"eng","language_probability":0.99,"text":"Hi there. Yes.","audio_duration_secs":4.0,"words":[
{"text":"Hi","start":0.0,"end":0.3,"type":"word","speaker_id":"speaker_0"},
{"text":" ","start":0.3,"end":0.35,"type":"spacing","speaker_id":"speaker_0"},
{"text":"there.","start":0.35,"end":0.8,"type":"word","speaker_id":"speaker_0"},
{"text":" ","start":0.8,"end":1.0,"type":"spacing","speaker_id":"speaker_0"},
{"text":"Yes.","start":2.5,"end":3.0,"type":"word","speaker_id":"speaker_1"}]}`

func TestElevenLabsMapping(t *testing.T) {
	up := newFake(t, func(captured) (int, http.Header, string) { return 200, nil, elevenOK })
	cfg := baseConfig("http://unused.invalid")
	cfg.Providers["eleven"] = &config.Provider{Type: config.TypeElevenLabs, BaseURL: up.srv.URL,
		Accounts: []*config.Account{{Name: "e1", APIKey: key("e1"), Enabled: true}}}
	cfg.Models["scribe"] = []config.Target{{Provider: "eleven", Model: "scribe_v3"}}
	e := newEnv(t, cfg, "127.0.0.1:8330")

	resp := e.transcribe(form{fields: [][2]string{
		{"model", "scribe"}, {"language", "en"}, {"diarize", "true"}, {"num_speakers", "2"},
		{"tag_audio_events", "false"}, {"keyterms", "Ana"}, {"keyterms", "Bob"}, {"unknown_x", "1"},
	}, file: []byte("audio")}, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != `{"text":"Hi there. Yes."}` {
		t.Fatalf("json: %d %s", resp.StatusCode, b)
	}
	c := up.last()
	if c.Path != "/v1/speech-to-text" || c.Header.Get("xi-api-key") != key("e1") || c.Header.Get("Authorization") != "" {
		t.Errorf("path %s headers %v", c.Path, c.Header)
	}
	want := map[string]string{
		"model_id": "[scribe_v3]", "language_code": "[en]", "diarize": "[true]", "num_speakers": "[2]",
		"tag_audio_events": "[false]", "keyterms": "[Ana Bob]", "timestamps_granularity": "[word]",
	}
	for k, v := range want {
		if fmt.Sprint(c.Fields[k]) != v {
			t.Errorf("field %s = %v, want %s", k, c.Fields[k], v)
		}
	}
	for _, k := range []string{"model", "unknown_x", "response_format"} {
		if _, ok := c.Fields[k]; ok {
			t.Errorf("field %s must not be sent", k)
		}
	}

	_, body, _ := e.simple("scribe", "verbose_json")
	var v struct {
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Words    []struct {
			Word    string  `json:"word"`
			Start   float64 `json:"start"`
			End     float64 `json:"end"`
			Speaker string  `json:"speaker"`
		} `json:"words"`
		Segments []struct {
			Text    string  `json:"text"`
			Start   float64 `json:"start"`
			End     float64 `json:"end"`
			Speaker string  `json:"speaker"`
		} `json:"segments"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Language != "eng" || v.Duration != 4 || len(v.Words) != 3 || v.Words[2].Speaker != "speaker_1" || v.Words[1].Word != "there." {
		t.Errorf("verbose_json %s", body)
	}
	if len(v.Segments) != 2 || v.Segments[0].Text != "Hi there." || v.Segments[1].Speaker != "speaker_1" || v.Segments[1].Start != 2.5 {
		t.Errorf("segments %+v", v.Segments)
	}
	_, body, _ = e.simple("scribe", "srt")
	if want := "1\n00:00:00,000 --> 00:00:00,800\nHi there.\n\n2\n00:00:02,500 --> 00:00:03,000\nYes.\n\n"; body != want {
		t.Errorf("srt %q, want %q", body, want)
	}
}

func TestRotation(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, baseConfig(up.srv.URL, "a", "b"), "127.0.0.1:8330")
	for i := 0; i < 4; i++ {
		e.clock.Add(time.Second)
		if status, body, _ := e.simple("whisper", ""); status != 200 {
			t.Fatalf("status %d %s", status, body)
		}
	}
	got := up.accounts()
	want := []string{key("a"), key("b"), key("a"), key("b")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("accounts %v, want %v", got, want)
	}
}

func TestWeight(t *testing.T) {
	up := newFake(t, ok)
	cfg := baseConfig(up.srv.URL, "a", "b")
	cfg.Providers["groq"].Accounts[0].Weight = 2
	e := newEnv(t, cfg, "127.0.0.1:8330")
	for i := 0; i < 6; i++ {
		e.simple("whisper", "")
	}
	n := 0
	for _, a := range up.accounts() {
		if a == key("a") {
			n++
		}
	}
	if n != 4 {
		t.Errorf("weight 2 account got %d of 6 requests, want 4", n)
	}
}

// rateLimitFirst makes account "a" answer 429 with the given header and body.
func rateLimitFirst(h http.Header, body string) func(c captured) (int, http.Header, string) {
	return func(c captured) (int, http.Header, string) {
		if c.Account == key("a") {
			return 429, h, body
		}
		return 200, nil, groqOK
	}
}

func TestRateLimitCooldown(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		body   string
		wait   time.Duration
	}{
		{"header seconds", http.Header{"Retry-After": {"120"}}, `{"error":{"message":"rate limited"}}`, 120 * time.Second},
		{"body text", nil, `{"error":{"message":"Rate limit reached. Please try again in 1m2.5s."}}`, 62500 * time.Millisecond},
		{"backoff", nil, `{"error":{"message":"rate limited"}}`, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFake(t, rateLimitFirst(tc.header, tc.body))
			e := newEnv(t, baseConfig(up.srv.URL, "a", "b"), "127.0.0.1:8330")
			if status, body, _ := e.simple("whisper", ""); status != 200 {
				t.Fatalf("status %d %s", status, body)
			}
			if got := fmt.Sprint(up.accounts()); got != fmt.Sprint([]string{key("a"), key("b")}) {
				t.Fatalf("first request: %v", got)
			}
			e.srv.mu.Lock()
			until := e.srv.accounts["groq/a"].CooldownUntil
			e.srv.mu.Unlock()
			if want := e.clock.Now().Add(tc.wait); !until.Equal(want) {
				t.Fatalf("cooldown until %v, want %v", until, want)
			}
			// While a cools down, only b is used.
			e.clock.Add(tc.wait - time.Millisecond)
			e.simple("whisper", "")
			if got := up.accounts()[2]; got != key("b") {
				t.Fatalf("cooling account used: %v", up.accounts())
			}
			e.clock.Add(time.Millisecond)
			e.simple("whisper", "")
			if got := up.accounts()[3]; got != key("a") {
				t.Fatalf("account a not used after cooldown: %v", up.accounts())
			}
		})
	}
}

func TestBackoffDoublesAndSuccessClears(t *testing.T) {
	fail := true
	var mu sync.Mutex
	up := newFake(t, func(c captured) (int, http.Header, string) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return 429, nil, "busy"
		}
		return 200, nil, groqOK
	})
	e := newEnv(t, baseConfig(up.srv.URL, "a"), "127.0.0.1:8330")
	for i, want := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		status, _, h := e.simple("whisper", "")
		if status != 429 || h.Get("Retry-After") != fmt.Sprint(int(want.Seconds())) {
			t.Fatalf("round %d: status %d Retry-After %q, want %v", i, status, h.Get("Retry-After"), want)
		}
		e.clock.Add(want)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if status, _, _ := e.simple("whisper", ""); status != 200 {
		t.Fatal("success expected")
	}
	e.srv.mu.Lock()
	rf := e.srv.accounts["groq/a"].RateFails
	e.srv.mu.Unlock()
	if rf != 0 {
		t.Errorf("RateFails = %d after success", rf)
	}
}

func TestInvalidServerAndClientErrors(t *testing.T) {
	t.Run("401 invalid", func(t *testing.T) {
		up := newFake(t, func(c captured) (int, http.Header, string) {
			if c.Account == key("a") {
				return 401, nil, `{"error":{"message":"Invalid API Key"}}`
			}
			return 200, nil, groqOK
		})
		e := newEnv(t, baseConfig(up.srv.URL, "a", "b"), "127.0.0.1:8330")
		if status, _, _ := e.simple("whisper", ""); status != 200 {
			t.Fatal("want fallback to b")
		}
		_, body := e.api("GET", "/api/status", "")
		if !strings.Contains(body, `"state":"invalid"`) || !strings.Contains(body, "Invalid API Key") {
			t.Errorf("status %s", body)
		}
		e.clock.Add(time.Hour)
		e.simple("whisper", "")
		if n := len(up.accounts()); n != 3 || up.accounts()[2] != key("b") {
			t.Errorf("invalid account used again: %v", up.accounts())
		}
	})
	t.Run("echoed key is masked", func(t *testing.T) {
		up := newFake(t, func(c captured) (int, http.Header, string) {
			return 400, nil, `{"error":{"message":"bad request for key ` + c.Account + `"}}`
		})
		e := newEnv(t, baseConfig(up.srv.URL, "a"), "127.0.0.1:8330")
		_, body, _ := e.simple("whisper", "")
		_, status := e.api("GET", "/api/status", "")
		_, logs := e.api("GET", "/api/logs", "")
		if strings.Contains(body+status+logs, key("a")) || !strings.Contains(body, MaskKey(key("a"))) {
			t.Errorf("key not masked: %s", body)
		}
	})
	t.Run("5xx rotates", func(t *testing.T) {
		up := newFake(t, func(c captured) (int, http.Header, string) {
			if c.Account == key("a") {
				return 503, nil, `upstream down`
			}
			return 200, nil, groqOK
		})
		e := newEnv(t, baseConfig(up.srv.URL, "a", "b"), "127.0.0.1:8330")
		if status, _, _ := e.simple("whisper", ""); status != 200 || len(up.accounts()) != 2 {
			t.Fatalf("status %d calls %v", status, up.accounts())
		}
		e.srv.mu.Lock()
		until := e.srv.accounts["groq/a"].CooldownUntil
		e.srv.mu.Unlock()
		if !until.Equal(e.clock.Now().Add(5 * time.Second)) {
			t.Errorf("5xx cooldown until %v", until)
		}
	})
	t.Run("400 does not rotate", func(t *testing.T) {
		up := newFake(t, func(c captured) (int, http.Header, string) {
			return 400, nil, `{"error":{"message":"could not decode audio","type":"invalid_request_error"}}`
		})
		e := newEnv(t, baseConfig(up.srv.URL, "a", "b"), "127.0.0.1:8330")
		status, body, _ := e.simple("whisper", "")
		if status != 400 || !strings.Contains(body, "could not decode audio") || len(up.accounts()) != 1 {
			t.Fatalf("status %d body %s calls %d", status, body, len(up.accounts()))
		}
		e.srv.mu.Lock()
		until := e.srv.accounts["groq/a"].CooldownUntil
		e.srv.mu.Unlock()
		if !until.IsZero() {
			t.Error("400 must not cool the account")
		}
	})
	t.Run("402 quota", func(t *testing.T) {
		up := newFake(t, func(c captured) (int, http.Header, string) { return 402, nil, `{"error":{"message":"pay"}}` })
		cfg := baseConfig(up.srv.URL, "a")
		cfg.QuotaCooldown = config.Duration(2 * time.Hour)
		e := newEnv(t, cfg, "127.0.0.1:8330")
		status, _, h := e.simple("whisper", "")
		if status != 429 || h.Get("Retry-After") != "7200" {
			t.Fatalf("status %d Retry-After %q", status, h.Get("Retry-After"))
		}
	})
}

func TestFallbackAndExhausted(t *testing.T) {
	groq := newFake(t, func(captured) (int, http.Header, string) {
		return 429, http.Header{"Retry-After": {"300"}}, `{"error":{"message":"rate"}}`
	})
	eleven := newFake(t, func(captured) (int, http.Header, string) {
		return 429, http.Header{"Retry-After": {"60"}}, `{"detail":{"status":"too_many_concurrent_requests"}}`
	})
	cfg := baseConfig(groq.srv.URL, "a")
	cfg.Providers["eleven"] = &config.Provider{Type: config.TypeElevenLabs, BaseURL: eleven.srv.URL,
		Accounts: []*config.Account{{Name: "e1", APIKey: key("e1"), Enabled: true}}}
	cfg.Models["auto"] = []config.Target{{Provider: "groq", Model: "whisper-large-v3-turbo"}, {Provider: "eleven", Model: "scribe_v3"}}
	e := newEnv(t, cfg, "127.0.0.1:8330")

	status, body, h := e.simple("auto", "")
	if status != 429 || h.Get("Retry-After") != "60" {
		t.Fatalf("status %d Retry-After %q body %s", status, h.Get("Retry-After"), body)
	}
	var apiErr apiError
	if err := json.Unmarshal([]byte(body), &apiErr); err != nil || apiErr.Error.Type != "rate_limit_error" {
		t.Errorf("error shape %s", body)
	}
	if len(groq.accounts()) != 1 || len(eleven.accounts()) != 1 {
		t.Fatalf("calls groq %d eleven %d", len(groq.accounts()), len(eleven.accounts()))
	}
	// Both are cooling down: no upstream call, Retry-After is the soonest end.
	e.clock.Add(10 * time.Second)
	_, _, h = e.simple("auto", "")
	if h.Get("Retry-After") != "50" || len(groq.accounts()) != 1 || len(eleven.accounts()) != 1 {
		t.Fatalf("Retry-After %q, calls %d %d", h.Get("Retry-After"), len(groq.accounts()), len(eleven.accounts()))
	}
}

func TestFallbackWhenGroupHasNoUsableAccount(t *testing.T) {
	groq := newFake(t, ok)
	eleven := newFake(t, func(captured) (int, http.Header, string) { return 200, nil, elevenOK })
	cfg := baseConfig(groq.srv.URL, "a")
	cfg.Providers["groq"].Accounts[0].Enabled = false
	cfg.Providers["eleven"] = &config.Provider{Type: config.TypeElevenLabs, BaseURL: eleven.srv.URL,
		Accounts: []*config.Account{{Name: "e1", APIKey: key("e1"), Enabled: true}}}
	cfg.Models["auto"] = []config.Target{{Provider: "groq", Model: "w"}, {Provider: "eleven", Model: "scribe_v3"}}
	e := newEnv(t, cfg, "127.0.0.1:8330")
	if status, body, _ := e.simple("auto", "text"); status != 200 || body != "Hi there. Yes.\n" {
		t.Fatalf("%d %q", status, body)
	}
	if len(groq.accounts()) != 0 {
		t.Error("disabled account used")
	}
	_, logs := e.api("GET", "/api/logs", "")
	if !strings.Contains(logs, `"provider":"eleven"`) || !strings.Contains(logs, `"upstream_model":"scribe_v3"`) {
		t.Errorf("log %s", logs)
	}
}

func TestMaxFileMB(t *testing.T) {
	up := newFake(t, ok)
	cfg := baseConfig(up.srv.URL, "small", "big")
	cfg.Providers["groq"].Accounts[0].MaxFileMB = 1
	e := newEnv(t, cfg, "127.0.0.1:8330")
	for i := 0; i < 3; i++ {
		resp := e.transcribe(form{fields: [][2]string{{"model", "whisper"}}, file: make([]byte, 1<<20+1)}, "")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	for _, a := range up.accounts() {
		if a != key("big") {
			t.Fatalf("small account got a big file: %v", up.accounts())
		}
	}
	cfg = baseConfig(up.srv.URL, "small")
	cfg.Providers["groq"].Accounts[0].MaxFileMB = 1
	e = newEnv(t, cfg, "127.0.0.1:8330")
	resp := e.transcribe(form{fields: [][2]string{{"model", "whisper"}}, file: make([]byte, 1<<20+1)}, "")
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Errorf("status %d, want 413", resp.StatusCode)
	}
}

func TestMaxConcurrent(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan string, 4)
	up := newFake(t, func(c captured) (int, http.Header, string) {
		entered <- c.Account
		<-release
		return 200, nil, groqOK
	})
	cfg := baseConfig(up.srv.URL, "a", "b")
	cfg.Providers["groq"].Accounts[0].MaxConcurrent = 1
	cfg.Providers["groq"].Accounts[1].MaxConcurrent = 1
	e := newEnv(t, cfg, "127.0.0.1:8330")
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { s, _, _ := e.simple("whisper", ""); results <- s }()
	}
	got := map[string]bool{<-entered: true, <-entered: true}
	if !got[key("a")] || !got[key("b")] {
		t.Fatalf("both accounts must be busy: %v", got)
	}
	status, _, h := e.simple("whisper", "")
	if status != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("third request: status %d, want 429 with Retry-After", status)
	}
	close(release)
	if <-results != 200 || <-results != 200 {
		t.Fatal("busy requests failed")
	}
}

func TestSpoolReplayAndCleanup(t *testing.T) {
	audio := make([]byte, 3<<20)
	for i := range audio {
		audio[i] = byte(i * 7)
	}
	var mu sync.Mutex
	var tmp string
	var spooled []string
	up := newFake(t, func(c captured) (int, http.Header, string) {
		mu.Lock()
		ents, _ := os.ReadDir(tmp)
		for _, en := range ents {
			spooled = append(spooled, en.Name())
		}
		mu.Unlock()
		if c.Account == key("a") {
			return 500, nil, "boom"
		}
		return 200, nil, groqOK
	})
	cfg := baseConfig(up.srv.URL, "a", "b")
	cfg.SpoolMemoryMB = 1
	e := newEnv(t, cfg, "127.0.0.1:8330")
	mu.Lock()
	tmp = e.srv.opts.TempDir
	mu.Unlock()

	resp := e.transcribe(form{fields: [][2]string{{"model", "whisper"}}, file: audio}, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	up.mu.Lock()
	calls := append([]captured(nil), up.calls...)
	up.mu.Unlock()
	if len(calls) != 2 || !bytes.Equal(calls[0].File, audio) || !bytes.Equal(calls[1].File, audio) {
		t.Fatalf("replay differs: %d calls", len(calls))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spooled) == 0 {
		t.Fatal("a 3 MB upload with a 1 MB memory limit must go to a temp file")
	}
	if ents, _ := os.ReadDir(e.srv.opts.TempDir); len(ents) != 0 {
		t.Fatalf("temp file left: %v", ents)
	}
}

func TestClientAbortRemovesSpool(t *testing.T) {
	entered := make(chan struct{}, 1)
	up := newFake(t, func(c captured) (int, http.Header, string) {
		entered <- struct{}{}
		time.Sleep(300 * time.Millisecond)
		return 200, nil, groqOK
	})
	cfg := baseConfig(up.srv.URL, "a")
	cfg.SpoolMemoryMB = 1
	e := newEnv(t, cfg, "127.0.0.1:8330")
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("model", "whisper")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write(make([]byte, 2<<20))
	mw.Close()
	req, _ := http.NewRequest("POST", e.http.URL+"/v1/audio/transcriptions", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	tr.CloseIdleConnections()
	e.http.CloseClientConnections()
	<-done
	deadline := time.Now().Add(3 * time.Second)
	for {
		ents, _ := os.ReadDir(e.srv.opts.TempDir)
		if len(ents) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temp file left after abort: %v", ents)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The slot is free again and the account is not penalized.
	e.srv.mu.Lock()
	a := e.srv.accounts["groq/a"]
	inflight, errs := a.inflight, a.Errors
	e.srv.mu.Unlock()
	if inflight != 0 || errs != 0 {
		t.Errorf("inflight %d errors %d after abort", inflight, errs)
	}
}

func TestUploadLimits(t *testing.T) {
	up := newFake(t, ok)
	cfg := baseConfig(up.srv.URL, "a")
	cfg.MaxUploadMB = 1
	e := newEnv(t, cfg, "127.0.0.1:8330")
	resp := e.transcribe(form{fields: [][2]string{{"model", "whisper"}}, file: make([]byte, 1<<20+10)}, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 413 || !strings.Contains(string(b), "max_upload_mb") {
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	// A declared length over the limit is refused before the body is read.
	req, _ := http.NewRequest("POST", e.http.URL+"/v1/audio/transcriptions", io.LimitReader(zeros{}, 3<<20))
	req.ContentLength = 3 << 20
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, tc := range []struct {
		f    form
		want string
	}{
		{form{fields: [][2]string{{"model", "whisper"}}}, "file is required"},
		{form{file: []byte("x")}, "model is required"},
		{form{fields: [][2]string{{"model", "whisper"}, {"response_format", "xml"}}, file: []byte("x")}, "not supported"},
		{form{fields: [][2]string{{"model", "nope"}}, file: []byte("x")}, "not served"},
	} {
		resp := e.transcribe(tc.f, "")
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 4 || !strings.Contains(string(b), tc.want) {
			t.Errorf("status %d body %s, want %q", resp.StatusCode, b, tc.want)
		}
	}
	if len(up.accounts()) != 0 {
		t.Error("bad requests must not reach the upstream")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestClientAuth(t *testing.T) {
	up := newFake(t, ok)
	t.Run("loopback without keys", func(t *testing.T) {
		e := newEnv(t, baseConfig(up.srv.URL, "a"), "127.0.0.1:8330")
		if status, _, _ := e.simple("whisper", ""); status != 200 {
			t.Fatalf("status %d", status)
		}
	})
	t.Run("keys set", func(t *testing.T) {
		cfg := baseConfig(up.srv.URL, "a")
		cfg.ClientKeys = []config.ClientKey{{Name: "laptop", Key: "sk-client-0123456789"}}
		e := newEnv(t, cfg, "127.0.0.1:8330")
		for _, auth := range []string{"", "wrong"} {
			resp := e.transcribe(form{fields: [][2]string{{"model", "whisper"}}, file: []byte("x")}, auth)
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("auth %q: status %d", auth, resp.StatusCode)
			}
		}
		resp := e.transcribe(form{fields: [][2]string{{"model", "whisper"}}, file: []byte("x")}, "sk-client-0123456789")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		_, logs := e.api("GET", "/api/logs", "")
		if !strings.Contains(logs, `"client_key":"laptop"`) {
			t.Errorf("logs %s", logs)
		}
	})
	t.Run("non-loopback without keys", func(t *testing.T) {
		e := newEnv(t, baseConfig(up.srv.URL, "a"), "0.0.0.0:8330")
		if status, _, _ := e.simple("whisper", ""); status != 401 {
			t.Fatalf("status %d", status)
		}
	})
}

func TestEnsureStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	cfg := config.Default()
	if _, err := EnsureStartup(path, cfg, "0.0.0.0:8330"); err == nil {
		t.Fatal("non-loopback without keys must fail")
	}
	cfg.ClientKeys = []config.ClientKey{{Name: "a", Key: "k"}}
	if _, err := EnsureStartup(path, cfg, "0.0.0.0:8330"); err == nil {
		t.Fatal("non-loopback without management key must fail")
	}
	cfg.ClientKeys = nil
	k, err := EnsureStartup(path, cfg, "127.0.0.1:8330")
	if err != nil || !strings.HasPrefix(k, "sr-mgmt-") {
		t.Fatalf("key %q err %v", k, err)
	}
	saved, _, err := config.Load(path)
	if err != nil || saved.ManagementKey != k {
		t.Fatalf("saved key %q err %v", saved.ManagementKey, err)
	}
	if k2, _ := EnsureStartup(path, saved, "127.0.0.1:8330"); k2 != "" {
		t.Error("key must be generated only once")
	}
	cfg.ClientKeys = []config.ClientKey{{Name: "a", Key: "k"}}
	if _, err := EnsureStartup(path, cfg, "0.0.0.0:8330"); err != nil {
		t.Fatalf("non-loopback with both keys: %v", err)
	}
}

func TestManagementAPI(t *testing.T) {
	up := newFake(t, func(c captured) (int, http.Header, string) {
		if !strings.HasPrefix(string(c.File), "RIFF") {
			return 400, nil, `{"error":{"message":"not a wav"}}`
		}
		return 200, nil, `{"text":"","segments":[],"duration":0.5}`
	})
	cfg := baseConfig(up.srv.URL, "a")
	cfg.ClientKeys = []config.ClientKey{{Name: "laptop", Key: "sk-client-secret-abcdef"}}
	e := newEnv(t, cfg, "127.0.0.1:8330")

	for _, auth := range []string{"", "Bearer wrong", "Bearer sk-client-secret-abcdef"} {
		req, _ := http.NewRequest("GET", e.http.URL+"/api/status", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("auth %q: status %d", auth, resp.StatusCode)
		}
	}

	secret := "gsk_full_secret_key_value_9876"
	var all strings.Builder
	call := func(method, path, body string, want int) string {
		status, out := e.api(method, path, body)
		if status != want {
			t.Fatalf("%s %s: status %d %s", method, path, status, out)
		}
		all.WriteString(out)
		return out
	}
	call("POST", "/api/providers/groq/accounts", `{"name":"b","api_key":"`+secret+`","max_file_mb":25}`, 201)
	call("POST", "/api/providers/groq/accounts", `{"name":"b","api_key":"x"}`, 400)
	call("PUT", "/api/providers/groq/accounts/b", `{"enabled":false,"weight":3}`, 200)
	out := call("GET", "/api/status", "", 200)
	if !strings.Contains(out, `"key_masked":"gsk_...9876"`) || !strings.Contains(out, `"state":"disabled"`) {
		t.Errorf("status %s", out)
	}
	call("POST", "/api/providers/groq/accounts/b/reset", "", 200)
	test := call("POST", "/api/providers/groq/accounts/a/test", "", 200)
	if !strings.Contains(test, `"ok":true`) {
		t.Errorf("test %s", test)
	}
	call("PUT", "/api/models/fast", `{"targets":[{"provider":"groq","model":"m"}]}`, 200)
	call("PUT", "/api/models/bad", `{"targets":[{"provider":"nope","model":"m"}]}`, 400)
	created := call("POST", "/api/client-keys", `{"name":"phone"}`, 201)
	var ck struct{ Key string }
	json.Unmarshal([]byte(created), &ck)
	if !strings.HasPrefix(ck.Key, "sk-sr-") {
		t.Fatalf("generated key %s", created)
	}
	out = call("GET", "/api/status", "", 200)
	call("GET", "/api/logs", "", 200)
	if strings.Contains(out, ck.Key) || strings.Contains(out, "sk-client-secret-abcdef") {
		t.Error("client key leaked in status")
	}
	// The full provider keys never appear in any management response.
	for _, k := range []string{secret, key("a")} {
		if strings.Contains(all.String(), k) {
			t.Errorf("full key %q found in an API response", k)
		}
	}

	saved, _, err := config.Load(e.path)
	if err != nil {
		t.Fatal(err)
	}
	b := saved.Providers["groq"].Accounts[1]
	if b.APIKey != secret || b.Enabled || b.Weight != 3 || b.MaxFileMB != 25 || len(saved.Models["fast"]) != 1 || len(saved.ClientKeys) != 2 {
		t.Errorf("saved config %+v %+v", b, saved)
	}
	if fi, _ := os.Stat(e.path); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %o", fi.Mode().Perm())
	}

	call("DELETE", "/api/providers/groq/accounts/b", "", 200)
	call("DELETE", "/api/models/fast", "", 200)
	call("DELETE", "/api/client-keys/phone", "", 200)
	call("DELETE", "/api/client-keys/phone", "", 404)
	saved, _, _ = config.Load(e.path)
	if len(saved.Providers["groq"].Accounts) != 1 || len(saved.ClientKeys) != 1 {
		t.Errorf("deletes not saved: %+v", saved)
	}

	resp, err := http.Get(e.http.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "stt-router") {
		t.Errorf("dashboard %d", resp.StatusCode)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	up := newFake(t, rateLimitFirst(http.Header{"Retry-After": {"600"}}, "slow"))
	e := newEnv(t, baseConfig(up.srv.URL, "a", "b"), "127.0.0.1:8330")
	if status, _, _ := e.simple("whisper", ""); status != 200 {
		t.Fatal("first request failed")
	}
	e.restart()
	e.srv.mu.Lock()
	a, b := e.srv.accounts["groq/a"], e.srv.accounts["groq/b"]
	e.srv.mu.Unlock()
	if a == nil || b == nil || !a.CooldownUntil.Equal(e.clock.Now().Add(600*time.Second)) || a.Errors != 1 || b.Requests != 1 || b.AudioSeconds != 3.5 {
		t.Fatalf("state after restart: a=%+v b=%+v", a, b)
	}
	e.clock.Add(time.Minute)
	e.simple("whisper", "")
	if got := up.accounts(); got[len(got)-1] != key("b") {
		t.Errorf("cooling account used after restart: %v", got)
	}
	_, status := e.api("GET", "/api/status", "")
	if !strings.Contains(status, `"requests":2`) {
		t.Errorf("totals not restored: %s", status)
	}
	_, logs := e.api("GET", "/api/logs", "")
	if strings.Count(logs, `"model":"whisper"`) != 2 {
		t.Errorf("log not restored: %s", logs)
	}
	if fi, _ := os.Stat(e.state); fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %o", fi.Mode().Perm())
	}
}

func TestModelsAndHealth(t *testing.T) {
	e := newEnv(t, baseConfig("http://unused.invalid", "a"), "127.0.0.1:8330")
	resp, _ := http.Get(e.http.URL + "/v1/models")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"id":"whisper"`) {
		t.Errorf("models %d %s", resp.StatusCode, b)
	}
	resp, _ = http.Get(e.http.URL + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz %d", resp.StatusCode)
	}
}

func TestNetworkErrorRotates(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	up := newFake(t, ok)
	cfg := baseConfig(dead.URL, "a")
	cfg.Providers["up"] = &config.Provider{Type: config.TypeOpenAI, BaseURL: up.srv.URL,
		Accounts: []*config.Account{{Name: "b", APIKey: key("b"), Enabled: true}}}
	cfg.Models["whisper"] = append(cfg.Models["whisper"], config.Target{Provider: "up", Model: "w"})
	e := newEnv(t, cfg, "127.0.0.1:8330")
	if status, body, _ := e.simple("whisper", ""); status != 200 {
		t.Fatalf("status %d %s", status, body)
	}
	_, logs := e.api("GET", "/api/logs", "")
	if !strings.Contains(logs, `"attempts":2`) {
		t.Errorf("logs %s", logs)
	}
}

func TestMaskKey(t *testing.T) {
	for in, want := range map[string]string{
		"gsk_abcdefghijklmnop": "gsk_...mnop", "short": "****", "env:GROQ_KEY": "env:GROQ_KEY",
	} {
		if got := MaskKey(in); got != want {
			t.Errorf("MaskKey(%q) = %q, want %q", in, got, want)
		}
	}
}
