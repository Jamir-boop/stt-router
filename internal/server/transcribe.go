package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
	"github.com/Jamir-boop/stt-router/internal/spool"
	"github.com/Jamir-boop/stt-router/internal/transcript"
	"github.com/Jamir-boop/stt-router/internal/upstream"
)

const (
	formOverhead  = 1 << 20 // room for the non-file form fields
	maxFieldBytes = 64 << 10
	maxFields     = 200
	maxErrorBody  = 1 << 20
	maxResultBody = 64 << 20
)

var readerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 64<<10) }}

type clientForm struct {
	model, format string
	words         bool
	req           upstream.Request
	file          *spool.Spool
}

type formError struct {
	status int
	param  string
	msg    string
}

func (e *formError) Error() string { return e.msg }

func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	cfg := s.cfg.Load()
	keyName, ok := s.clientAuth(cfg, r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "missing or invalid client key")
		return
	}
	entry := LogEntry{Time: start, ClientKey: keyName}
	defer func() {
		entry.LatencyMs = s.now().Sub(start).Milliseconds()
		s.recordRequest(entry)
		s.opts.Logger.Printf("transcribe model=%q provider=%q account=%q status=%d attempts=%d latency=%dms bytes=%d",
			entry.Model, entry.Provider, entry.Account, entry.Status, entry.Attempts, entry.LatencyMs, entry.UploadBytes)
	}()
	fail := func(status int, typ, code, msg string) {
		entry.Status, entry.Error = status, msg
		writeError(w, status, typ, code, msg)
	}

	maxBytes := int64(cfg.MaxUploadMB) << 20
	if r.ContentLength > maxBytes+formOverhead {
		fail(http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large",
			fmt.Sprintf("upload is larger than max_upload_mb (%d MB)", cfg.MaxUploadMB))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+formOverhead)
	form, err := s.readForm(r, cfg, maxBytes)
	if form != nil && form.file != nil {
		defer form.file.Close() // also runs when the client aborts
	}
	if err != nil {
		var fe *formError
		if !errors.As(err, &fe) {
			fe = &formError{status: http.StatusBadRequest, msg: err.Error()}
		}
		code := "invalid_request"
		if fe.status == http.StatusRequestEntityTooLarge {
			code = "file_too_large"
		}
		entry.Error = fe.msg
		entry.Status = fe.status
		var e apiError
		e.Error.Message, e.Error.Type, e.Error.Code = fe.msg, "invalid_request_error", code
		if fe.param != "" {
			e.Error.Param = &fe.param
		}
		writeJSON(w, fe.status, e)
		return
	}
	entry.Model = form.model
	entry.UploadBytes = form.file.Size()

	targets, ok := cfg.Models[form.model]
	if !ok {
		fail(http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("model %q is not served by this router", form.model))
		return
	}

	needTS := transcript.NeedsTimestamps(form.format)
	var sk skips
	var prep audioPrep
	defer prep.close()
	lastErr := ""
	for _, t := range targets {
		p := cfg.Providers[t.Provider]
		req := &form.req
		if p.Type == config.TypeAzureSpeech {
			file, err := s.azureFile(r.Context(), cfg, p, form, &prep)
			if r.Context().Err() != nil {
				entry.Status, entry.Error = 499, "client closed the request"
				return
			}
			if err != nil {
				// The target cannot take this audio; no account is judged.
				if errors.Is(err, errUnsupported) {
					sk.unsupported = true
				} else {
					sk.badAudio = true
				}
				lastErr = fmt.Sprintf("%s: %v", t.Provider, err)
				continue
			}
			rq := form.req
			rq.File = file
			req = &rq
		}
		tried := map[string]bool{}
		for {
			acc := s.pick(t.Provider, p, req.File.Size, needTS, tried, &sk)
			if acc == nil {
				break
			}
			tried[acc.Name] = true
			entry.Attempts++
			entry.Provider, entry.Account, entry.UpstreamModel = t.Provider, acc.Name, t.Model
			res := s.attempt(r.Context(), cfg, t.Provider, p, acc, t.Model, req)
			if res.canceled {
				s.free(t.Provider, acc.Name)
				entry.Status, entry.Error = 499, "client closed the request"
				return
			}
			audio := 0.0
			if res.tr != nil {
				audio = res.tr.Duration
			}
			s.release(t.Provider, acc.Name, res.class, res.retry, audio, res.msg)
			switch res.class {
			case upstream.OK:
				entry.Status, entry.AudioSeconds = http.StatusOK, audio
				body, ct := res.tr.Render(form.format)
				w.Header().Set("Content-Type", ct)
				w.WriteHeader(http.StatusOK)
				w.Write(body)
				return
			case upstream.ClientError:
				entry.Status, entry.Error = res.status, res.msg
				writeUpstreamError(w, res.status, res.body, res.msg)
				return
			}
			lastErr = fmt.Sprintf("%s/%s: %s", t.Provider, acc.Name, res.msg)
		}
	}

	suffix := ""
	if lastErr != "" {
		suffix = "; last error: " + lastErr
	}
	switch {
	case !sk.soonest.IsZero() || sk.busy:
		wait := time.Second
		if !sk.soonest.IsZero() {
			wait = max(sk.soonest.Sub(s.now()), time.Second)
		}
		secs := int64(math.Ceil(wait.Seconds()))
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
		fail(http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
			fmt.Sprintf("all accounts for model %q are cooling down or busy; retry in %d s%s", form.model, secs, suffix))
	case sk.tooBig:
		fail(http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large",
			fmt.Sprintf("upload is larger than max_file_mb of every account for model %q%s", form.model, suffix))
	case sk.format:
		fail(http.StatusBadRequest, "invalid_request_error", "unsupported_response_format",
			fmt.Sprintf("response_format %q needs timestamps, and every account for model %q has plain_json set%s", form.format, form.model, suffix))
	case sk.unsupported:
		fail(http.StatusUnsupportedMediaType, "invalid_request_error", "unsupported_media_type",
			fmt.Sprintf("no target of model %q can take this audio format: azure-speech takes WAV, MP3 or FLAC%s", form.model, suffix))
	case sk.badAudio:
		fail(http.StatusBadRequest, "invalid_request_error", "invalid_audio",
			fmt.Sprintf("the audio could not be converted for model %q%s", form.model, suffix))
	default:
		fail(http.StatusServiceUnavailable, "server_error", "no_account",
			fmt.Sprintf("no enabled account for model %q%s", form.model, suffix))
	}
}

func writeUpstreamError(w http.ResponseWriter, status int, body []byte, msg string) {
	var probe struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Error != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	writeError(w, status, "invalid_request_error", "upstream_error", msg)
}

// readForm parses the multipart form as a stream and spools the file.
func (s *Server) readForm(r *http.Request, cfg *config.Config, maxBytes int64) (f *clientForm, err error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, &formError{status: http.StatusBadRequest, msg: "body must be multipart/form-data with a boundary"}
	}
	// multipart.NewReader reuses a big enough *bufio.Reader, so the socket is
	// read in large chunks instead of its default 4 KB.
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(r.Body)
	defer func() { br.Reset(nil); readerPool.Put(br) }()
	mr := multipart.NewReader(br, params["boundary"])
	f = &clientForm{format: transcript.FormatJSON}
	tooLarge := func(err error) error {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || errors.Is(err, spool.ErrTooLarge) {
			return &formError{status: http.StatusRequestEntityTooLarge, param: "file",
				msg: fmt.Sprintf("upload is larger than max_upload_mb (%d MB)", cfg.MaxUploadMB)}
		}
		return &formError{status: http.StatusBadRequest, msg: "read form: " + err.Error()}
	}
	nfields := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return f, tooLarge(err)
		}
		name := part.FormName()
		if name == "file" {
			if f.file != nil {
				return f, &formError{status: http.StatusBadRequest, param: "file", msg: "only one file is allowed"}
			}
			sp, err := spool.New(part, r.ContentLength, int64(cfg.SpoolMemoryMB)<<20, maxBytes, s.opts.TempDir)
			if err != nil {
				return f, tooLarge(err)
			}
			f.file = sp
			f.req.File = upstream.File{Name: part.FileName(), ContentType: part.Header.Get("Content-Type"), Size: sp.Size(), Open: sp.Open}
			continue
		}
		if name == "" {
			continue
		}
		if nfields++; nfields > maxFields {
			return f, &formError{status: http.StatusBadRequest, msg: "too many form fields"}
		}
		b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
		if err != nil {
			return f, tooLarge(err)
		}
		if len(b) > maxFieldBytes {
			return f, &formError{status: http.StatusBadRequest, param: name, msg: "form field " + name + " is too long"}
		}
		v := string(b)
		switch name {
		case "model":
			f.model = v
		case "language":
			f.req.Language = v
		case "prompt":
			f.req.Prompt = v
		case "temperature":
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return f, &formError{status: http.StatusBadRequest, param: name, msg: "temperature must be a number"}
			}
			f.req.Temperature = v
		case "response_format":
			if !transcript.ValidFormat(v) {
				return f, &formError{status: http.StatusBadRequest, param: name,
					msg: fmt.Sprintf("response_format %q is not supported; use json, text, verbose_json, srt or vtt", v)}
			}
			f.format = v
		case "timestamp_granularities[]", "timestamp_granularities":
			switch v {
			case "word":
				f.words = true
			case "segment":
			default:
				return f, &formError{status: http.StatusBadRequest, param: name, msg: "timestamp_granularities must be segment or word"}
			}
		default:
			f.req.Extra = append(f.req.Extra, upstream.Field{Name: name, Value: v})
		}
	}
	if f.file == nil {
		return f, &formError{status: http.StatusBadRequest, param: "file", msg: "file is required"}
	}
	if f.model == "" {
		return f, &formError{status: http.StatusBadRequest, param: "model", msg: "model is required"}
	}
	f.req.Words = f.words
	f.req.Timestamps = transcript.NeedsTimestamps(f.format)
	return f, nil
}

type result struct {
	class    upstream.Class
	retry    time.Duration
	status   int
	body     []byte
	msg      string
	tr       *transcript.Transcript
	canceled bool
}

// attempt sends one request to one account.
func (s *Server) attempt(ctx context.Context, cfg *config.Config, group string, p *config.Provider, acc *config.Account, model string, base *upstream.Request) result {
	req := *base
	req.Model = model
	req.Plain = acc.PlainJSON
	actx, cancel := context.WithTimeout(ctx, time.Duration(cfg.RequestTimeout))
	defer cancel()
	apiKey := config.Resolve(acc.APIKey)
	hreq, err := upstream.NewHTTPRequest(actx, p.Type, p.BaseURL, apiKey, "stt-router/"+s.opts.Version, &req)
	if err != nil {
		var re *upstream.RequestError
		if errors.As(err, &re) {
			return result{class: upstream.ClientError, status: http.StatusBadRequest, msg: re.Msg}
		}
		return result{class: upstream.ServerError, msg: err.Error()}
	}
	resp, err := (&http.Client{Transport: s.transport(group)}).Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return result{canceled: true}
		}
		msg := "network error: " + err.Error()
		if upstream.IsTimeout(err) {
			msg = "upstream timeout: " + err.Error()
		}
		return result{class: upstream.ServerError, msg: msg}
	}
	defer resp.Body.Close()
	limit := int64(maxErrorBody)
	if resp.StatusCode/100 == 2 {
		limit = maxResultBody
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		if ctx.Err() != nil {
			return result{canceled: true}
		}
		return result{class: upstream.ServerError, msg: "read upstream response: " + err.Error()}
	}
	class, retry := upstream.Classify(p.Type, resp.StatusCode, resp.Header, body, s.now())
	if class != upstream.OK {
		// Some providers echo the key in their error text; it must not reach the client or the dashboard.
		if apiKey != "" {
			body = bytes.ReplaceAll(body, []byte(apiKey), []byte(MaskKey(apiKey)))
		}
		return result{class: class, retry: retry, status: resp.StatusCode, body: body,
			msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, upstream.ErrorMessage(resp.StatusCode, body))}
	}
	tr, err := upstream.Parse(p.Type, body)
	if err != nil {
		return result{class: upstream.ServerError, msg: err.Error()}
	}
	return result{class: upstream.OK, tr: tr}
}
