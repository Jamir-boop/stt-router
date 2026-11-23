// Package upstream builds provider requests, parses provider responses and
// classifies provider errors.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
	"github.com/Jamir-boop/stt-router/internal/transcript"
)

// Field is one multipart form value.
type Field struct{ Name, Value string }

// File is the replayable upload.
type File struct {
	Name        string
	ContentType string
	Size        int64
	Open        func() io.Reader // each call returns a fresh reader from the start
}

// Request is a provider-neutral transcription request.
type Request struct {
	Model       string
	Language    string
	Prompt      string
	Temperature string
	Words       bool    // the client asked for word timestamps
	Timestamps  bool    // the response format needs timestamps
	Plain       bool    // the account supports only json, so ask for json
	Extra       []Field // other client fields; each provider keeps the ones it knows
	File        File
}

// elevenLabsExtra lists the extra client fields that ElevenLabs understands.
var elevenLabsExtra = map[string]bool{
	"diarize": true, "num_speakers": true, "diarization_threshold": true,
	"tag_audio_events": true, "keyterms": true, "seed": true, "file_format": true,
}

// AzureAPIVersion is the Azure Speech fast transcription API version.
const AzureAPIVersion = "2025-10-15"

// RequestError is a client field that the upstream type cannot take.
type RequestError struct{ Msg string }

func (e *RequestError) Error() string { return e.Msg }

// NewHTTPRequest builds the upstream request. The body streams the upload
// from File.Open with a known length, so no second copy is built in memory.
func NewHTTPRequest(ctx context.Context, typ, baseURL, apiKey, userAgent string, r *Request) (*http.Request, error) {
	var fields []Field
	add := func(name, value string) {
		if value != "" {
			fields = append(fields, Field{name, value})
		}
	}
	var url string
	fileField, definition := "file", []byte(nil)
	switch typ {
	case config.TypeOpenAI:
		url = baseURL + "/audio/transcriptions"
		add("model", r.Model)
		add("language", r.Language)
		add("prompt", r.Prompt)
		add("temperature", r.Temperature)
		if r.Plain {
			add("response_format", "json")
		} else {
			add("response_format", "verbose_json")
			add("timestamp_granularities[]", "segment")
			if r.Words {
				add("timestamp_granularities[]", "word")
			}
		}
	case config.TypeElevenLabs:
		url = baseURL + "/v1/speech-to-text"
		add("model_id", r.Model)
		add("language_code", r.Language)
		add("temperature", r.Temperature)
		add("timestamps_granularity", "word")
		for _, f := range r.Extra {
			if elevenLabsExtra[f.Name] {
				fields = append(fields, f)
			}
		}
	case config.TypeAzureSpeech:
		url = baseURL + "/speechtotext/transcriptions:transcribe?api-version=" + AzureAPIVersion
		def, err := azureDefinition(r)
		if err != nil {
			return nil, err
		}
		fileField, definition = "audio", def
	default:
		return nil, fmt.Errorf("unknown provider type %q", typ)
	}

	contentType, length, open := multipartBody(fields, fileField, r.File, definition)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, open())
	if err != nil {
		return nil, err
	}
	req.ContentLength = length
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(open()), nil }
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	switch typ {
	case config.TypeElevenLabs:
		req.Header.Set("xi-api-key", apiKey)
	case config.TypeAzureSpeech:
		req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)
	default:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

// multipartBody writes fields before the file part and an optional JSON
// "definition" part after it.
func multipartBody(fields []Field, fileField string, f File, definition []byte) (string, int64, func() io.Reader) {
	var head, tail bytes.Buffer
	mw := multipart.NewWriter(&head)
	for _, fl := range fields {
		_ = mw.WriteField(fl.Name, fl.Value) // writes to a bytes.Buffer cannot fail
	}
	name := f.Name
	if name == "" {
		name = "audio"
	}
	ct := f.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fileField, quoteEscaper.Replace(name)))
	h.Set("Content-Type", ct)
	_, _ = mw.CreatePart(h)
	// A second writer with the same boundary writes the parts after the file and the closing delimiter.
	tw := multipart.NewWriter(&tail)
	_ = tw.SetBoundary(mw.Boundary())
	if definition != nil {
		dh := textproto.MIMEHeader{}
		dh.Set("Content-Disposition", `form-data; name="definition"`)
		dh.Set("Content-Type", "application/json")
		tail.WriteString("\r\n") // CreatePart omits the CRLF before a writer's first boundary
		pw, _ := tw.CreatePart(dh)
		_, _ = pw.Write(definition)
	}
	_ = tw.Close()
	hb, tb := head.Bytes(), tail.Bytes()
	open := func() io.Reader {
		return io.MultiReader(bytes.NewReader(hb), f.Open(), bytes.NewReader(tb))
	}
	return mw.FormDataContentType(), int64(len(hb)) + f.Size + int64(len(tb)), open
}

// Parse converts a successful upstream body to a transcript.
func Parse(typ string, body []byte) (*transcript.Transcript, error) {
	switch typ {
	case config.TypeOpenAI:
		return parseOpenAI(body)
	case config.TypeElevenLabs:
		return parseElevenLabs(body)
	case config.TypeAzureSpeech:
		return parseAzure(body)
	}
	return nil, fmt.Errorf("unknown provider type %q", typ)
}

func parseOpenAI(body []byte) (*transcript.Transcript, error) {
	var t transcript.Transcript
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("upstream returned invalid JSON: %w", err)
	}
	if t.Duration == 0 && len(t.Segments) > 0 {
		t.Duration = t.Segments[len(t.Segments)-1].End
	}
	return &t, nil
}

type elevenLabsResponse struct {
	LanguageCode      string  `json:"language_code"`
	Text              string  `json:"text"`
	AudioDurationSecs float64 `json:"audio_duration_secs"`
	Words             []struct {
		Text      string  `json:"text"`
		Start     float64 `json:"start"`
		End       float64 `json:"end"`
		Type      string  `json:"type"`
		SpeakerID string  `json:"speaker_id"`
	} `json:"words"`
}

func parseElevenLabs(body []byte) (*transcript.Transcript, error) {
	var r elevenLabsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("upstream returned invalid JSON: %w", err)
	}
	t := &transcript.Transcript{Text: r.Text, Language: r.LanguageCode, Duration: r.AudioDurationSecs}
	tokens := make([]transcript.Token, 0, len(r.Words))
	for _, w := range r.Words {
		isWord := w.Type == "word" || w.Type == ""
		tokens = append(tokens, transcript.Token{Text: w.Text, Start: w.Start, End: w.End, Speaker: w.SpeakerID, IsWord: isWord || w.Type == "audio_event"})
		if isWord {
			t.Words = append(t.Words, transcript.Word{Word: w.Text, Start: w.Start, End: w.End, Speaker: w.SpeakerID})
		}
	}
	if t.Duration == 0 {
		for _, w := range t.Words {
			t.Duration = max(t.Duration, w.End)
		}
	}
	t.Segments = transcript.SegmentsFromTokens(tokens)
	return t, nil
}

// Class is the kind of an upstream result.
type Class int

const (
	OK          Class = iota
	RateLimited       // 429: cool down and try the next account
	Invalid           // 401 or 403: the key is bad
	Quota             // 402 or quota exceeded
	ServerError       // 5xx, timeout or network error
	ClientError       // other 4xx: return to the client, do not rotate
)

func (c Class) String() string {
	return [...]string{"ok", "rate_limited", "invalid", "quota", "server_error", "client_error"}[c]
}

var tryAgainRe = regexp.MustCompile(`try again in ((?:[0-9]+(?:\.[0-9]+)?(?:ms|h|m|s))+)`)

// Classify sorts an HTTP status into a Class. For RateLimited it also returns
// the wait the upstream asked for, or 0 when it gave none.
func Classify(typ string, status int, header http.Header, body []byte, now time.Time) (Class, time.Duration) {
	if status >= 200 && status < 300 {
		return OK, 0
	}
	// ElevenLabs reports an exhausted quota as 401 with "quota_exceeded".
	if status == http.StatusPaymentRequired || (typ == config.TypeElevenLabs && bytes.Contains(body, []byte("quota_exceeded"))) {
		return Quota, 0
	}
	switch {
	case status == http.StatusTooManyRequests:
		return RateLimited, retryAfter(header.Get("Retry-After"), body, now)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Invalid, 0
	case status == http.StatusRequestTimeout || status >= 500:
		return ServerError, 0
	}
	return ClientError, 0
}

func retryAfter(h string, body []byte, now time.Time) time.Duration {
	if h = strings.TrimSpace(h); h != "" {
		if secs, err := strconv.ParseFloat(h, 64); err == nil && secs >= 0 {
			return time.Duration(secs * float64(time.Second))
		}
		if t, err := http.ParseTime(h); err == nil && t.After(now) {
			return t.Sub(now)
		}
	}
	if m := tryAgainRe.FindSubmatch(body); m != nil {
		if d, err := time.ParseDuration(string(m[1])); err == nil {
			return d
		}
	}
	return 0
}

// ErrorMessage extracts a readable message from an upstream error body.
func ErrorMessage(status int, body []byte) string {
	var v struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
		Msg    string          `json:"message"`
	}
	if json.Unmarshal(body, &v) == nil {
		for _, raw := range []json.RawMessage{v.Error, v.Detail} {
			if len(raw) == 0 {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return s
			}
			var o struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			}
			if json.Unmarshal(raw, &o) == nil && (o.Message != "" || o.Status != "") {
				if o.Status == "" || o.Message == "" {
					return o.Status + o.Message
				}
				return o.Status + ": " + o.Message
			}
		}
		if v.Msg != "" {
			return v.Msg
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300]
	}
	if s == "" {
		return fmt.Sprintf("upstream returned HTTP %d", status)
	}
	return s
}

// IsTimeout reports whether err is a timeout from the transport or context.
func IsTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}
