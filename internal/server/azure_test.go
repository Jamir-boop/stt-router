package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Jamir-boop/stt-router/internal/config"
)

const azureOK = `{"durationMilliseconds":4200,
"combinedPhrases":[{"channel":0,"text":"Hola, Ana. Hola, Bob."}],
"phrases":[
{"channel":0,"speaker":1,"offsetMilliseconds":80,"durationMilliseconds":1200,"text":"Hola, Ana.","locale":"es-ES","confidence":0.9,
 "words":[{"text":"Hola,","offsetMilliseconds":80,"durationMilliseconds":400},{"text":"Ana.","offsetMilliseconds":560,"durationMilliseconds":720}]},
{"channel":0,"speaker":2,"offsetMilliseconds":2000,"durationMilliseconds":1500,"text":"Hola, Bob.","locale":"es-ES","confidence":0.9,
 "words":[{"text":"Hola,","offsetMilliseconds":2000,"durationMilliseconds":500},{"text":"Bob.","offsetMilliseconds":2600,"durationMilliseconds":900}]}]}`

var (
	wavBytes = []byte("RIFF\x24\x00\x00\x00WAVEfmt audio")
	oggBytes = []byte("OggS\x00\x02\x00\x00opus-audio")
)

// azureEnv has a model "mai" with only the azure group and a model "chain"
// with azure first and groq second.
func azureEnv(t *testing.T, convert string, azure, groq *fake) *env {
	t.Helper()
	cfg := baseConfig(groq.srv.URL, "g1")
	cfg.Providers["azure"] = &config.Provider{Type: config.TypeAzureSpeech, BaseURL: azure.srv.URL, Convert: convert,
		Accounts: []*config.Account{{Name: "z1", APIKey: key("z1"), Enabled: true}}}
	cfg.Models["mai"] = []config.Target{{Provider: "azure", Model: "MAI-Transcribe-2"}}
	cfg.Models["chain"] = []config.Target{{Provider: "azure", Model: "MAI-Transcribe-2"}, {Provider: "groq", Model: "whisper-large-v3-turbo"}}
	return newEnv(t, cfg, "127.0.0.1:8330")
}

func (e *env) send(model string, file []byte, fields ...[2]string) (int, string) {
	e.t.Helper()
	resp := e.transcribe(form{fields: append([][2]string{{"model", model}}, fields...), file: file}, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// untouched fails the test when the account has any request, error or cooldown.
func (e *env) untouched(group, name string) {
	e.t.Helper()
	e.srv.mu.Lock()
	defer e.srv.mu.Unlock()
	a := e.srv.acct(group, name)
	if a.Requests != 0 || a.Errors != 0 || !a.CooldownUntil.IsZero() || a.inflight != 0 {
		e.t.Errorf("%s/%s was judged: %+v inflight %d", group, name, a.accountState, a.inflight)
	}
}

func TestAzureMapping(t *testing.T) {
	az := newFake(t, func(captured) (int, http.Header, string) { return 200, nil, azureOK })
	e := azureEnv(t, "", az, newFake(t, ok))

	status, body := e.send("mai", wavBytes, [2]string{"language", "es"}, [2]string{"diarize", "true"}, [2]string{"prompt", "x"})
	if status != 200 || body != `{"text":"Hola, Ana. Hola, Bob."}` {
		t.Fatalf("json: %d %s", status, body)
	}
	c := az.last()
	if c.Path != "/speechtotext/transcriptions:transcribe" || c.Query != "api-version=2025-10-15" || c.Account != key("z1") {
		t.Errorf("path %s query %s account %s", c.Path, c.Query, c.Account)
	}
	if c.FileName != "audio.wav" || c.FileType != "audio/wav" || string(c.File) != string(wavBytes) {
		t.Errorf("file %q %q %q", c.FileName, c.FileType, c.File)
	}
	if got := c.Fields["definition"]; len(got) != 1 || got[0] != `{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"segment"}},"locales":["es"],"diarization":{"enabled":true},"profanityFilterMode":"None"}` {
		t.Errorf("definition %v", got)
	}
	if len(c.Fields) != 1 {
		t.Errorf("fields %v, want only definition", c.Fields)
	}

	if status, body := e.send("mai", wavBytes, [2]string{"response_format", "text"}); status != 200 || body != "Hola, Ana. Hola, Bob.\n" {
		t.Errorf("text: %d %q", status, body)
	}
	status, body = e.send("mai", wavBytes, [2]string{"response_format", "srt"})
	if want := "1\n00:00:00,080 --> 00:00:01,280\nHola, Ana.\n\n2\n00:00:02,000 --> 00:00:03,500\nHola, Bob.\n\n"; status != 200 || body != want {
		t.Errorf("srt: %d %q, want %q", status, body, want)
	}
	if !strings.Contains(az.last().Fields["definition"][0], `"timestamps":"word"`) {
		t.Errorf("srt must ask for word timestamps: %v", az.last().Fields["definition"])
	}
	_, body = e.send("mai", wavBytes, [2]string{"response_format", "verbose_json"})
	var v struct {
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Segments []struct {
			Start, End    float64
			Text, Speaker string
		} `json:"segments"`
		Words []struct {
			Word       string
			Start, End float64
			Speaker    string
		} `json:"words"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Language != "es-ES" || v.Duration != 4.2 || len(v.Segments) != 2 || v.Segments[1].Speaker != "speaker_2" || v.Segments[1].Start != 2 {
		t.Errorf("verbose_json %s", body)
	}
	if len(v.Words) != 4 || v.Words[1].Word != "Ana." || v.Words[1].Start != 0.56 || v.Words[1].End != 1.28 || v.Words[3].Speaker != "speaker_2" {
		t.Errorf("words %+v", v.Words)
	}
	status, body = e.send("mai", wavBytes, [2]string{"response_format", "vtt"})
	if want := "WEBVTT\n\n00:00:00.080 --> 00:00:01.280\nHola, Ana.\n\n00:00:02.000 --> 00:00:03.500\nHola, Bob.\n\n"; status != 200 || body != want {
		t.Errorf("vtt: %d %q", status, body)
	}
	status, body, _ = e.simple("mai", "") // the bytes "audio" are no known format
	if status != http.StatusUnsupportedMediaType || !strings.Contains(body, "unknown") {
		t.Errorf("unknown format: %d %s", status, body)
	}
}

func TestAzureConvertOff(t *testing.T) {
	az := newFake(t, func(captured) (int, http.Header, string) { return 200, nil, azureOK })
	groq := newFake(t, ok)
	e := azureEnv(t, "off", az, groq)

	if status, body := e.send("chain", oggBytes); status != 200 || !strings.Contains(body, "Hello world") {
		t.Fatalf("chain: %d %s", status, body)
	}
	if len(az.accounts()) != 0 {
		t.Error("the azure target must not get an Ogg upload")
	}
	if string(groq.last().File) != string(oggBytes) {
		t.Errorf("groq got %q, want the original bytes", groq.last().File)
	}
	e.untouched("azure", "z1")

	status, body := e.send("mai", oggBytes)
	if status != http.StatusUnsupportedMediaType || !strings.Contains(body, "unsupported_media_type") ||
		!strings.Contains(body, "WAV, MP3 or FLAC") || !strings.Contains(body, "ogg") {
		t.Fatalf("mai: %d %s", status, body)
	}
	e.untouched("azure", "z1")
}

// fakeFFmpeg puts an ffmpeg script first on PATH. It records its arguments
// and writes a small FLAC header to the last argument, or fails when
// FAKE_FFMPEG_FAIL is set.
func fakeFFmpeg(t *testing.T) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ffmpeg is a shell script")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsFile + `"
if [ -n "$FAKE_FFMPEG_FAIL" ]; then echo "Invalid data found when processing input" >&2; exit 1; fi
for a; do out=$a; done
printf 'fLaC\000\000\000\042converted' > "$out"
`
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argsFile
}

func TestAzureConvertFFmpeg(t *testing.T) {
	argsFile := fakeFFmpeg(t)
	az := newFake(t, func(captured) (int, http.Header, string) { return 200, nil, azureOK })
	groq := newFake(t, ok)
	e := azureEnv(t, "ffmpeg", az, groq)
	tmp := e.srv.opts.TempDir

	if status, body := e.send("chain", oggBytes); status != 200 || !strings.Contains(body, "Hola") {
		t.Fatalf("ogg: %d %s", status, body)
	}
	c := az.last()
	if c.FileName != "audio.flac" || c.FileType != "audio/flac" || string(c.File) != "fLaC\x00\x00\x00\x22converted" {
		t.Errorf("azure got %q %q %q", c.FileName, c.FileType, c.File)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{"-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe", "-i", "IN",
		"-map", "0:a:0", "-ac", "1", "-ar", "16000", "-c:a", "flac", "OUT"}
	if len(args) != len(want) {
		t.Fatalf("args %q", args)
	}
	for i := range want {
		if want[i] == "IN" || want[i] == "OUT" {
			if filepath.Dir(filepath.Dir(args[i])) != tmp {
				t.Errorf("arg %d %q is not in the spool directory %s", i, args[i], tmp)
			}
			continue
		}
		if args[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, args[i], want[i])
		}
	}
	if !strings.HasSuffix(args[len(args)-1], ".flac") {
		t.Errorf("output %q", args[len(args)-1])
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	// A WAV goes as it is, unless diarization needs mono.
	os.Remove(argsFile)
	e.send("mai", wavBytes)
	if c := az.last(); c.FileName != "audio.wav" || string(c.File) != string(wavBytes) {
		t.Errorf("wav: %q %q", c.FileName, c.File)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("ffmpeg must not run for a WAV without diarization")
	}
	e.send("mai", wavBytes, [2]string{"diarize", "true"})
	if c := az.last(); c.FileName != "audio.flac" {
		t.Errorf("wav with diarize: %q, want a converted FLAC", c.FileName)
	}

	// A failing conversion goes to the next target with the original bytes.
	t.Setenv("FAKE_FFMPEG_FAIL", "1")
	before := len(az.accounts())
	if status, body := e.send("chain", oggBytes); status != 200 || !strings.Contains(body, "Hello world") {
		t.Fatalf("failing ffmpeg: %d %s", status, body)
	}
	if len(az.accounts()) != before || string(groq.last().File) != string(oggBytes) {
		t.Error("a failed conversion must skip azure and send the original upload to groq")
	}
	status, body := e.send("mai", oggBytes)
	if status != 400 || !strings.Contains(body, "invalid_audio") || !strings.Contains(body, "Invalid data found") {
		t.Errorf("failing ffmpeg, no fallback: %d %s", status, body)
	}
	e.srv.mu.Lock()
	a := e.srv.acct("azure", "z1")
	if !a.CooldownUntil.IsZero() || a.Errors != 0 {
		t.Errorf("a failed conversion must not judge the account: %+v", a.accountState)
	}
	e.srv.mu.Unlock()
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temp files left after a failure: %v", left)
	}
}

func TestAzureTestButton(t *testing.T) {
	az := newFake(t, func(captured) (int, http.Header, string) { return 200, nil, azureOK })
	e := azureEnv(t, "", az, newFake(t, ok))
	status, body := e.api("POST", "/api/providers/azure/accounts/z1/test", "")
	if status != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("test: %d %s", status, body)
	}
	if c := az.last(); c.FileType != "audio/wav" || !strings.Contains(c.Fields["definition"][0], "MAI-Transcribe-2") {
		t.Errorf("test request %q %v", c.FileType, c.Fields)
	}
}
