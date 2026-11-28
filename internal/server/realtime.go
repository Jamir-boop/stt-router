package server

// GET /v1/realtime: an OpenAI Realtime transcription WebSocket for live audio.
// It cuts the stream into turns with an energy VAD and sends each turn through
// the normal batch routing, so models, accounts, cooldowns, the request log and
// the totals are the same as for /v1/audio/transcriptions.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
)

const (
	rtQueueLen    = 8                // pending turns per connection
	rtReadTimeout = 60 * time.Second // the client pings every 15 s
	rtMaxNullTurn = 60 * time.Second // force-cut with turn_detection null
)

var rtItemSeq atomic.Uint64

type rtSession struct {
	rate      int
	language  string
	model     string // router model name
	vadOn     bool
	threshold float64
	silenceMs int
	prefixMs  int
}

func (s *rtSession) vadConfig() vadConfig {
	return vadConfig{rate: s.rate, threshold: s.threshold, silenceMs: s.silenceMs, prefixMs: s.prefixMs}
}

type rtConn struct {
	s      *Server
	nc     net.Conn
	ws     *wsConn
	ctx    context.Context
	cancel context.CancelFunc
	auth   http.Header // the upgrade request headers, so the in-process handler sees the same key
	query  string      // ?model= from the URL, resolved once

	sess   rtSession
	vad    *vad
	raw    []byte // audio with turn_detection null
	openID string // item of the turn the VAD is in

	jobs chan *rtJob
	wg   sync.WaitGroup
}

// rtJob is one turn. It carries the session settings of the moment it was cut,
// so the worker never reads state the reader may change.
type rtJob struct {
	id  string
	pcm []byte
	ses rtSession
}

// rtWriter is the in-memory ResponseWriter for the in-process handler call.
type rtWriter struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (w *rtWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *rtWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *rtWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	if _, ok := s.clientAuth(cfg, r); !ok {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "missing or invalid client key")
		return
	}
	if !headerHas(r.Header, "Upgrade", "websocket") || !headerHas(r.Header, "Connection", "upgrade") || r.Header.Get("Sec-WebSocket-Key") == "" {
		writeError(w, http.StatusUpgradeRequired, "invalid_request_error", "upgrade_required",
			"this endpoint needs a WebSocket upgrade; connect to /v1/realtime?intent=transcription")
		return
	}
	nc, br, err := wsUpgrade(w, r)
	if err != nil {
		s.opts.Logger.Printf("realtime: upgrade: %v", err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess := rtSession{rate: 24000, vadOn: true, threshold: 0.6, silenceMs: 600, prefixMs: 500}
	rt := &rtConn{s: s, nc: nc, ws: &wsConn{rwc: nc, br: br, rd: rtReadTimeout},
		ctx: ctx, cancel: cancel, auth: r.Header.Clone(), query: r.URL.Query().Get("model"),
		sess: sess, vad: newVAD(sess.vadConfig()),
		jobs: make(chan *rtJob, rtQueueLen)}
	if !s.addRT(rt) {
		nc.Close()
		cancel()
		return
	}
	defer s.rtWG.Done()
	rt.wg.Add(1)
	go rt.worker()
	defer func() {
		close(rt.jobs)
		cancel() // aborts a turn in flight
		rt.wg.Wait()
		s.removeRT(rt)
		nc.Close()
	}()
	rt.run(cfg)
}

func (s *Server) addRT(c *rtConn) bool {
	s.rtMu.Lock()
	defer s.rtMu.Unlock()
	if s.rtClosed {
		return false
	}
	s.rts[c] = struct{}{}
	s.rtWG.Add(1)
	return true
}

func (s *Server) removeRT(c *rtConn) {
	s.rtMu.Lock()
	delete(s.rts, c)
	s.rtMu.Unlock()
}

// closeRealtime closes the open realtime connections, without a close frame:
// the server is stopping and the sockets are about to be lost anyway.
func (s *Server) closeRealtime() {
	s.rtMu.Lock()
	// The flag is set under the lock that addRT holds, so no Add can come
	// after Close starts to wait.
	s.rtClosed = true
	conns := make([]*rtConn, 0, len(s.rts))
	for c := range s.rts {
		conns = append(conns, c)
	}
	s.rtMu.Unlock()
	for _, c := range conns {
		c.nc.Close()
	}
}

// rtResolveModel picks the router model for a client model name: the client
// name when the router serves it, else "auto" when configured.
func rtResolveModel(cfg *config.Config, requested string) (string, bool) {
	if _, ok := cfg.Models[requested]; ok {
		return requested, true
	}
	if _, ok := cfg.Models["auto"]; ok {
		return "auto", true
	}
	return "", false
}

// run sends the opening events and then reads client messages.
func (rt *rtConn) run(cfg *config.Config) {
	if rt.query != "" {
		rt.sess.model = rt.query
	} else if m, ok := rtResolveModel(cfg, ""); ok {
		rt.sess.model = m
	}
	rt.sendSession("session.created")
	if _, ok := cfg.Models[rt.sess.model]; !ok {
		msg := fmt.Sprintf("model %q is not served by this router", rt.sess.model)
		if rt.query == "" {
			msg += fmt.Sprintf(", and %q is not configured", "auto")
		}
		rt.sendError("invalid_request_error", "model_not_found", msg)
		return
	}
	for {
		op, msg, err := rt.ws.readMessage()
		if err != nil {
			return
		}
		if op == opText {
			rt.handle(msg)
		}
	}
}

func (rt *rtConn) handle(msg []byte) {
	var m struct {
		Type    string          `json:"type"`
		Audio   string          `json:"audio"`
		Session json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		rt.sendError("invalid_request_error", "invalid_event", "each message must be a JSON object with a type")
		return
	}
	switch m.Type {
	case "session.update":
		rt.applySession(m.Session)
	case "input_audio_buffer.append":
		pcm, err := base64.StdEncoding.DecodeString(m.Audio)
		if err != nil {
			rt.sendError("invalid_request_error", "invalid_audio", "audio must be base64 of 16-bit PCM")
			return
		}
		rt.audio(pcm)
	case "input_audio_buffer.commit":
		rt.commit()
	}
	// Other event types are ignored, as the API allows events this router does not need.
}

func (rt *rtConn) applySession(raw json.RawMessage) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var up struct {
		Audio *struct {
			Input *struct {
				Format *struct {
					Rate int `json:"rate"`
				} `json:"format"`
				Transcription *struct {
					Model    string `json:"model"`
					Language string `json:"language"`
				} `json:"transcription"`
				TurnDetection json.RawMessage `json:"turn_detection"`
			} `json:"input"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(raw, &up); err != nil {
		rt.sendError("invalid_request_error", "invalid_session", "session must be a JSON object")
		return
	}
	sess := rt.sess
	if up.Audio != nil && up.Audio.Input != nil {
		in := up.Audio.Input
		if in.Format != nil && in.Format.Rate != 0 {
			if in.Format.Rate < 8000 || in.Format.Rate > 48000 {
				rt.sendError("invalid_request_error", "invalid_sample_rate", "the audio rate must be 8000 to 48000 Hz")
				return
			}
			sess.rate = in.Format.Rate
		}
		if in.Transcription != nil {
			if in.Transcription.Language != "" {
				sess.language = in.Transcription.Language
			}
			if rt.query == "" {
				m, ok := rtResolveModel(rt.s.cfg.Load(), in.Transcription.Model)
				if !ok {
					rt.sendError("invalid_request_error", "model_not_found",
						fmt.Sprintf("model %q is not served by this router, and %q is not configured", in.Transcription.Model, "auto"))
					rt.nc.Close()
					return
				}
				sess.model = m
			}
		}
		if len(in.TurnDetection) > 0 {
			if string(in.TurnDetection) == "null" {
				sess.vadOn = false
			} else {
				var td struct {
					Threshold *float64 `json:"threshold"`
					SilenceMS *int     `json:"silence_duration_ms"`
					PrefixMS  *int     `json:"prefix_padding_ms"`
				}
				if err := json.Unmarshal(in.TurnDetection, &td); err != nil {
					rt.sendError("invalid_request_error", "invalid_session", "turn_detection must be a server_vad object or null")
					return
				}
				sess.vadOn = true
				if td.Threshold != nil && *td.Threshold >= 0 && *td.Threshold <= 1 {
					sess.threshold = *td.Threshold
				}
				if td.SilenceMS != nil && *td.SilenceMS >= 0 {
					sess.silenceMs = *td.SilenceMS
				}
				if td.PrefixMS != nil && *td.PrefixMS >= 0 {
					sess.prefixMs = *td.PrefixMS
				}
			}
		}
	}
	switch {
	case !sess.vadOn:
		rt.vad, rt.openID = nil, ""
	case rt.vad == nil || !rt.sess.vadOn || sess.rate != rt.sess.rate:
		// A rate change restarts the detector, because its frames are in samples.
		rt.vad, rt.openID = newVAD(sess.vadConfig()), ""
	default:
		rt.vad.cfg = sess.vadConfig()
	}
	if sess.vadOn {
		rt.raw = nil
	}
	rt.sess = sess
	rt.sendSession("session.updated")
}

func (rt *rtConn) sessionObj() map[string]any {
	var td any
	if rt.sess.vadOn {
		td = map[string]any{"type": "server_vad", "threshold": rt.sess.threshold,
			"silence_duration_ms": rt.sess.silenceMs, "prefix_padding_ms": rt.sess.prefixMs}
	}
	tr := map[string]any{"model": rt.sess.model}
	if rt.sess.language != "" {
		tr["language"] = rt.sess.language
	}
	return map[string]any{
		"type": "transcription",
		"audio": map[string]any{"input": map[string]any{
			"format":         map[string]any{"type": "audio/pcm", "rate": rt.sess.rate},
			"transcription":  tr,
			"turn_detection": td,
		}},
	}
}

func (rt *rtConn) sendSession(typ string) {
	rt.send(map[string]any{"type": typ, "session": rt.sessionObj()})
}

func (rt *rtConn) sendError(typ, code, msg string) {
	rt.send(map[string]any{"type": "error",
		"error": map[string]any{"type": typ, "code": code, "message": msg}})
}

func (rt *rtConn) sendCompleted(id, transcript string) {
	rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.completed",
		"item_id": id, "transcript": transcript})
}

func (rt *rtConn) sendFailed(id, code, msg string) {
	rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.failed",
		"item_id": id, "error": map[string]any{"type": "server_error", "code": code, "message": msg}})
}

func (rt *rtConn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = rt.ws.write(opText, b) // a failed write ends the read loop on the next frame
}

// audio adds PCM from one append event.
func (rt *rtConn) audio(pcm []byte) {
	if !rt.sess.vadOn {
		limit := int(rtMaxNullTurn.Seconds()) * rt.sess.rate * 2
		for len(pcm) > 0 {
			n := min(limit-len(rt.raw), len(pcm))
			rt.raw = append(rt.raw, pcm[:n]...)
			pcm = pcm[n:]
			if len(rt.raw) >= limit {
				rt.commitRaw()
			}
		}
		return
	}
	for _, ev := range rt.vad.feed(pcm) {
		rt.vadEvent(ev)
	}
}

func (rt *rtConn) vadEvent(ev vadEvent) {
	switch {
	case ev.started:
		rt.openID = newItemID()
		rt.send(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": rt.openID})
	case ev.stopped:
		rt.send(map[string]any{"type": "input_audio_buffer.speech_stopped", "item_id": rt.openID})
		if ev.turn != nil {
			rt.send(map[string]any{"type": "input_audio_buffer.committed", "item_id": rt.openID})
			rt.enqueue(rt.openID, ev.turn.pcm)
		}
		rt.openID = "" // a short turn is dropped without an upstream request
	}
}

func newItemID() string { return fmt.Sprintf("item_%d", rtItemSeq.Add(1)) }

// commit handles input_audio_buffer.commit.
func (rt *rtConn) commit() {
	if !rt.sess.vadOn {
		if len(rt.raw) < rt.sess.rate/10*2 { // less than 100 ms
			rt.commitEmpty()
			return
		}
		rt.commitRaw()
		return
	}
	if rt.vad == nil {
		rt.commitEmpty()
		return
	}
	t := rt.vad.flush()
	if t == nil {
		rt.commitEmpty()
		return
	}
	id := rt.openID
	if id == "" {
		id = newItemID()
	}
	rt.openID = ""
	rt.send(map[string]any{"type": "input_audio_buffer.committed", "item_id": id})
	rt.enqueue(id, t.pcm)
}

// commitRaw ends a turn with turn_detection null.
func (rt *rtConn) commitRaw() {
	id := newItemID()
	rt.send(map[string]any{"type": "input_audio_buffer.committed", "item_id": id})
	rt.enqueue(id, rt.raw)
	rt.raw = nil
}

func (rt *rtConn) commitEmpty() {
	rt.sendError("invalid_request_error", "input_audio_buffer_commit_empty",
		"audio buffer too small to transcribe (less than 300 ms of speech)")
}

// enqueue sends a turn to the worker. With rtQueueLen pending turns the oldest
// is dropped with a failed event, so the newest speech is never lost.
func (rt *rtConn) enqueue(id string, pcm []byte) {
	job := &rtJob{id: id, pcm: pcm, ses: rt.sess}
	select {
	case rt.jobs <- job:
		return
	default:
	}
	select {
	case old := <-rt.jobs:
		rt.sendFailed(old.id, "queue_full", "the connection has 8 turns pending; the oldest pending turn was dropped")
	default:
	}
	rt.jobs <- job // the reader alone enqueues, and a slot is free now
}

func (rt *rtConn) worker() {
	defer rt.wg.Done()
	for job := range rt.jobs {
		if rt.ctx.Err() != nil {
			continue
		}
		text, code, msg := rt.transcribe(job)
		if rt.ctx.Err() != nil {
			return // the client is gone; its answers go nowhere
		}
		if msg != "" {
			rt.sendFailed(job.id, code, msg)
			continue
		}
		rt.sendCompleted(job.id, text)
	}
}

// transcribe runs one turn through the batch handler in-process, with the
// connection's Authorization header and context. There is no network hop.
func (rt *rtConn) transcribe(job *rtJob) (text, code, msg string) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", job.ses.model)
	_ = mw.WriteField("response_format", "json")
	if job.ses.language != "" {
		_ = mw.WriteField("language", job.ses.language)
	}
	fw, err := mw.CreateFormFile("file", "turn.wav")
	if err != nil {
		return "", "internal_error", err.Error()
	}
	if _, err := fw.Write(wavPCM(job.pcm, job.ses.rate)); err != nil {
		return "", "internal_error", err.Error()
	}
	if err := mw.Close(); err != nil {
		return "", "internal_error", err.Error()
	}
	req, err := http.NewRequestWithContext(rt.ctx, http.MethodPost, "/v1/audio/transcriptions", &body)
	if err != nil {
		return "", "internal_error", err.Error()
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", rt.auth.Get("Authorization"))
	rec := &rtWriter{}
	rt.s.handleTranscribe(rec, req)
	if rec.status == http.StatusOK {
		var out struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(rec.body.Bytes(), &out); err != nil {
			return "", "upstream_error", "the router returned an invalid transcription"
		}
		return strings.TrimSpace(out.Text), "", ""
	}
	var e apiError
	if json.Unmarshal(rec.body.Bytes(), &e) == nil && e.Error.Message != "" {
		if e.Error.Code == "" {
			return "", "transcription_failed", e.Error.Message
		}
		return "", e.Error.Code, e.Error.Message
	}
	return "", "transcription_failed", fmt.Sprintf("the router returned HTTP %d", rec.status)
}

// wavPCM wraps 16-bit mono little-endian PCM in a WAV header.
func wavPCM(pcm []byte, rate int) []byte {
	b := make([]byte, 44, 44+len(pcm))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(pcm)))
	return append(b, pcm...)
}
