package server

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
)

// rtClient is a hand-written WebSocket client for the tests.
type rtClient struct {
	t  *testing.T
	ws *wsConn
}

func dialRealtime(t *testing.T, base, key, query string) *rtClient {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	req := "GET /v1/realtime?intent=transcription" + query + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " +
		base64.StdEncoding.EncodeToString(nonce[:]) + "\r\n"
	if key != "" {
		req += "Authorization: Bearer " + key + "\r\n"
	}
	if _, err := nc.Write([]byte(req + "\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status %d", resp.StatusCode)
	}
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	return &rtClient{t: t, ws: &wsConn{rwc: nc, br: br, client: true}}
}

func (c *rtClient) send(typ string, fields map[string]any) {
	c.t.Helper()
	m := map[string]any{"type": typ}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	if err := c.ws.write(opText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *rtClient) appendAudio(pcm []byte) {
	c.t.Helper()
	c.send("input_audio_buffer.append", map[string]any{"audio": base64.StdEncoding.EncodeToString(pcm)})
}

// next reads one server event.
func (c *rtClient) next() map[string]any {
	c.t.Helper()
	op, msg, err := c.ws.readMessage()
	if err != nil || op != opText {
		c.t.Fatalf("read event: op %d err %v", op, err)
	}
	var ev map[string]any
	if err := json.Unmarshal(msg, &ev); err != nil {
		c.t.Fatalf("event %q: %v", msg, err)
	}
	return ev
}

// until reads events until one has the given type.
func (c *rtClient) until(typ string) map[string]any {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		if ev := c.next(); ev["type"] == typ {
			return ev
		}
	}
	c.t.Fatalf("no %s event", typ)
	return nil
}

func rtConfig(groqURL string) *config.Config {
	cfg := baseConfig(groqURL, "a")
	cfg.Models["gpt-4o-mini-transcribe"] = []config.Target{{Provider: "groq", Model: "whisper-large-v3-turbo"}}
	cfg.Models["auto"] = []config.Target{{Provider: "groq", Model: "whisper-large-v3-turbo"}}
	return cfg
}

func (c *rtClient) update(rate int, td any) {
	c.t.Helper()
	c.send("session.update", map[string]any{"session": map[string]any{
		"type": "transcription",
		"audio": map[string]any{"input": map[string]any{
			"format":         map[string]any{"type": "audio/pcm", "rate": rate},
			"transcription":  map[string]any{"model": "gpt-4o-mini-transcribe", "language": "en"},
			"turn_detection": td,
		}},
	}})
}

func TestRealtimeTurn(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, rtConfig(up.srv.URL), "127.0.0.1:8330")
	c := dialRealtime(t, e.http.URL, "", "&junk=1")
	if ev := c.next(); ev["type"] != "session.created" {
		t.Fatalf("first event %v", ev["type"])
	}
	// Silence then tone must also sound like real audio to the noise floor.
	c.update(16000, map[string]any{"type": "server_vad", "threshold": 0.6, "silence_duration_ms": 600, "prefix_padding_ms": 500})
	if ev := c.next(); ev["type"] != "session.updated" {
		t.Fatalf("second event %v", ev["type"])
	}

	// The many small appends of a real client, in 100 ms pieces: 100 ms of
	// silence, 1 s of tone, 1 s of silence.
	var pcm []byte
	pcm = append(pcm, pcmSilence(16000, 100)...)
	pcm = append(pcm, pcmTone(16000, 440, 1000, 0.5)...)
	pcm = append(pcm, pcmSilence(16000, 1000)...)
	for len(pcm) > 0 {
		n := min(16000/10*2, len(pcm))
		c.appendAudio(pcm[:n])
		pcm = pcm[n:]
	}

	started := c.until("input_audio_buffer.speech_started")
	if started["item_id"] == "" {
		t.Error("speech_started needs an item_id")
	}
	if ev := c.until("input_audio_buffer.speech_stopped"); ev["item_id"] != started["item_id"] {
		t.Errorf("speech_stopped item %v, want %v", ev["item_id"], started["item_id"])
	}
	if ev := c.until("input_audio_buffer.committed"); ev["item_id"] != started["item_id"] {
		t.Errorf("committed item %v, want %v", ev["item_id"], started["item_id"])
	}
	done := c.until("conversation.item.input_audio_transcription.completed")
	if done["transcript"] != "Hello world. How are you?" || done["item_id"] != started["item_id"] {
		t.Errorf("completed %v", done)
	}
	call := up.last()
	// Like any batch request, an openai-type account gets verbose_json for the duration counters.
	if call.Fields["model"][0] != "whisper-large-v3-turbo" || call.Fields["language"][0] != "en" {
		t.Errorf("fields %v", call.Fields)
	}
	if string(call.File[:4]) != "RIFF" || binary.LittleEndian.Uint32(call.File[24:]) != 16000 {
		t.Errorf("the turn must be a 16 kHz WAV, got %q", call.File[:12])
	}
	_, logs := e.api("GET", "/api/logs", "")
	if !strings.Contains(logs, `"upstream_model":"whisper-large-v3-turbo"`) {
		t.Errorf("the realtime request must be in the request log: %s", logs)
	}
}

func TestRealtimeCommitEmpty(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, rtConfig(up.srv.URL), "127.0.0.1:8330")
	c := dialRealtime(t, e.http.URL, "", "")
	c.next()
	c.send("input_audio_buffer.commit", nil)
	ev := c.next()
	errObj, _ := ev["error"].(map[string]any)
	if ev["type"] != "error" || errObj["code"] != "input_audio_buffer_commit_empty" {
		t.Fatalf("event %v", ev)
	}
	if len(up.accounts()) != 0 {
		t.Error("an empty commit must not reach the upstream")
	}
}

func TestRealtimeNoSessionUpdate(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, rtConfig(up.srv.URL), "127.0.0.1:8330")
	c := dialRealtime(t, e.http.URL, "", "")
	c.next()
	pcm := pcmTone(24000, 440, 1000, 0.5)
	for len(pcm) > 0 {
		n := min(2400*2, len(pcm))
		c.appendAudio(pcm[:n])
		pcm = pcm[n:]
	}
	c.send("input_audio_buffer.commit", nil)
	c.until("conversation.item.input_audio_transcription.completed")
	call := up.last()
	if r := binary.LittleEndian.Uint32(call.File[24:]); r != 24000 {
		t.Errorf("default rate must be 24000, got %d", r)
	}
	if m := call.Fields["model"]; len(m) == 0 || m[0] != "whisper-large-v3-turbo" {
		t.Errorf("preconfigured mode must use auto, got %v", m)
	}
}

func TestRealtimeExplicitCommitWithTurnDetectionNull(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, rtConfig(up.srv.URL), "127.0.0.1:8330")
	c := dialRealtime(t, e.http.URL, "", "")
	c.next()
	c.update(16000, nil)
	if ev := c.next(); ev["type"] != "session.updated" {
		t.Fatalf("event %v", ev["type"])
	}
	c.appendAudio(pcmTone(16000, 440, 200, 0.5))
	c.send("input_audio_buffer.commit", nil)
	if ev := c.next(); ev["error"] != nil {
		t.Fatalf("200 ms with turn_detection null must commit: %v", ev)
	}
	c.until("conversation.item.input_audio_transcription.completed")
}

func TestRealtimeUnauthorized(t *testing.T) {
	up := newFake(t, ok)
	cfg := rtConfig(up.srv.URL)
	cfg.ClientKeys = []config.ClientKey{{Name: "laptop", Key: "sk-client-0123456789"}}
	e := newEnv(t, cfg, "127.0.0.1:8330")
	resp, err := http.Get(e.http.URL + "/v1/realtime?intent=transcription")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	upgrade := func(key string) *http.Response {
		nc, err := net.Dial("tcp", strings.TrimPrefix(e.http.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { nc.Close() })
		nc.Write([]byte("GET /v1/realtime?intent=transcription HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer " + key + "\r\n\r\n"))
		resp, err := http.ReadResponse(bufio.NewReader(nc), nil)
		if err != nil {
			t.Fatal(err)
		}
		nc.SetDeadline(time.Now().Add(30 * time.Second))
		return resp
	}
	if resp := upgrade("sk-wrong-key"); resp.StatusCode != 401 {
		t.Fatalf("a wrong key must answer 401 before the upgrade, got %d", resp.StatusCode)
	}
	if resp := upgrade("sk-client-0123456789"); resp.StatusCode != 101 {
		t.Fatalf("a correct key must upgrade, got %d", resp.StatusCode)
	}
}

func TestRealtimeRequiresUpgrade(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, rtConfig(up.srv.URL), "127.0.0.1:8330")
	resp, err := http.Get(e.http.URL + "/v1/realtime?intent=transcription")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 426 {
		t.Fatalf("status %d, want 426", resp.StatusCode)
	}
	var apiErr apiError
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil || apiErr.Error.Code != "upgrade_required" {
		t.Errorf("error shape %v", err)
	}
}

func TestRealtimeUpstreamFailureIsFailed(t *testing.T) {
	up := newFake(t, func(captured) (int, http.Header, string) {
		return 400, nil, `{"error":{"message":"could not decode audio"}}`
	})
	e := newEnv(t, rtConfig(up.srv.URL), "127.0.0.1:8330")
	c := dialRealtime(t, e.http.URL, "", "")
	c.next()
	c.appendAudio(pcmTone(24000, 440, 1000, 0.5))
	c.send("input_audio_buffer.commit", nil)
	ev := c.until("conversation.item.input_audio_transcription.failed")
	errObj, _ := ev["error"].(map[string]any)
	if !strings.Contains(errObj["message"].(string), "could not decode audio") {
		t.Errorf("failed event %v", ev)
	}
}

func TestRealtimeCloseRealtime(t *testing.T) {
	up := newFake(t, ok)
	e := newEnv(t, rtConfig(up.srv.URL), "127.0.0.1:8330")
	c := dialRealtime(t, e.http.URL, "", "")
	c.next()
	e.srv.CloseRealtime()
	c.ws.rwc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := c.ws.readMessage(); err == nil {
		t.Fatal("the connection must close")
	}
}
