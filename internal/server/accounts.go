package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
	"github.com/Jamir-boop/stt-router/internal/upstream"
)

// Cooldown rules.
const (
	rateBase       = 30 * time.Second
	rateCap        = time.Hour
	serverBase     = 5 * time.Second
	serverCap      = 5 * time.Minute
	invalidPenalty = 24 * time.Hour
	saveDebounce   = 2 * time.Second
)

// accountState is the part of an account's runtime state that survives a restart.
type accountState struct {
	CooldownUntil time.Time `json:"cooldown_until,omitzero"`
	Invalid       bool      `json:"invalid,omitempty"`
	RateFails     int       `json:"rate_fails,omitempty"`
	ServerFails   int       `json:"server_fails,omitempty"`
	Requests      int64     `json:"requests"`
	Errors        int64     `json:"errors"`
	AudioSeconds  float64   `json:"audio_seconds"`
	LastUsed      time.Time `json:"last_used,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
}

type account struct {
	accountState
	inflight int
	vt       float64 // weighted virtual time; the lowest goes next
}

// Counter is a set of request totals.
type Counter struct {
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	AudioSeconds float64 `json:"audio_seconds"`
	LatencyMs    int64   `json:"latency_ms_sum"`
}

// Totals are the request totals for today and for all time.
type Totals struct {
	Day   string  `json:"day"`
	Today Counter `json:"today"`
	All   Counter `json:"all"`
}

// LogEntry is one line of the request log.
type LogEntry struct {
	Time          time.Time `json:"time"`
	ClientKey     string    `json:"client_key"`
	Model         string    `json:"model"`
	Provider      string    `json:"provider"`
	Account       string    `json:"account"`
	UpstreamModel string    `json:"upstream_model"`
	AudioSeconds  float64   `json:"audio_seconds"`
	UploadBytes   int64     `json:"upload_bytes"`
	LatencyMs     int64     `json:"latency_ms"`
	Attempts      int       `json:"attempts"`
	Status        int       `json:"status"`
	Error         string    `json:"error,omitempty"`
}

type stateFile struct {
	Accounts map[string]accountState `json:"accounts"`
	Totals   Totals                  `json:"totals"`
	Log      []LogEntry              `json:"log"`
}

func accountKey(group, name string) string { return group + "/" + name }

// acct returns the runtime state for an account, creating it. Call with s.mu held.
func (s *Server) acct(group, name string) *account {
	k := accountKey(group, name)
	a := s.accounts[k]
	if a == nil {
		a = &account{vt: s.clocks[group]}
		s.accounts[k] = a
	}
	return a
}

// skips records why accounts were not usable, to pick the final error.
type skips struct {
	soonest  time.Time // earliest cooldown end
	busy     bool
	tooBig   bool
	format   bool
	disabled bool
}

func (k *skips) cooldown(t time.Time) {
	if k.soonest.IsZero() || t.Before(k.soonest) {
		k.soonest = t
	}
}

// pick chooses the usable account with the lowest weighted virtual time;
// with equal weights that is the least recently used one. It reserves a
// concurrency slot, which release frees.
func (s *Server) pick(group string, p *config.Provider, size int64, needTS bool, tried map[string]bool, sk *skips) *config.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	clock := s.clocks[group]
	var best *config.Account
	var bestState *account
	bestVT := 0.0
	for _, cfgA := range p.Accounts {
		a := s.acct(group, cfgA.Name)
		switch {
		case !cfgA.Enabled:
			sk.disabled = true
			continue
		case cfgA.MaxFileMB > 0 && size > int64(cfgA.MaxFileMB)<<20:
			sk.tooBig = true
			continue
		case cfgA.PlainJSON && needTS:
			sk.format = true
			continue
		case now.Before(a.CooldownUntil):
			sk.cooldown(a.CooldownUntil)
			continue
		case tried[cfgA.Name]:
			continue
		case cfgA.MaxConcurrent > 0 && a.inflight >= cfgA.MaxConcurrent:
			sk.busy = true
			continue
		}
		vt := max(a.vt, clock)
		if best == nil || vt < bestVT || (vt == bestVT && a.LastUsed.Before(bestState.LastUsed)) {
			best, bestState, bestVT = cfgA, a, vt
		}
	}
	if best == nil {
		return nil
	}
	w := best.Weight
	if w <= 0 {
		w = 1
	}
	s.clocks[group] = bestVT
	bestState.vt = bestVT + 1/float64(w)
	bestState.inflight++
	bestState.LastUsed = now
	bestState.Requests++
	return best
}

// release records an attempt's outcome and frees its concurrency slot.
func (s *Server) release(group, name string, class upstream.Class, retry time.Duration, audio float64, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.acct(group, name)
	a.inflight--
	s.applyOutcome(a, class, retry, audio, msg)
	s.markDirty()
}

// free frees a concurrency slot without judging the account, after a client abort.
func (s *Server) free(group, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acct(group, name).inflight--
}

// applyOutcome updates cooldowns and counters. Call with s.mu held.
func (s *Server) applyOutcome(a *account, class upstream.Class, retry time.Duration, audio float64, msg string) {
	now := s.now()
	if class == upstream.OK {
		a.RateFails, a.ServerFails, a.Invalid = 0, 0, false
		a.CooldownUntil = time.Time{}
		a.AudioSeconds += audio
		return
	}
	a.Errors++
	a.LastError, a.LastErrorAt = msg, now
	var d time.Duration
	switch class {
	case upstream.RateLimited:
		a.RateFails++
		d = retry
		if d <= 0 {
			d = backoff(rateBase, a.RateFails, rateCap)
		}
	case upstream.Invalid:
		a.Invalid = true
		d = invalidPenalty
	case upstream.Quota:
		d = time.Duration(s.cfg.Load().QuotaCooldown)
	case upstream.ServerError:
		a.ServerFails++
		d = backoff(serverBase, a.ServerFails, serverCap)
	default:
		return
	}
	a.CooldownUntil = now.Add(d)
}

func backoff(base time.Duration, n int, limit time.Duration) time.Duration {
	d := base
	for i := 1; i < n && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func (s *Server) transport(group string) *http.Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.transports[group]
	if t == nil {
		timeout := time.Duration(s.cfg.Load().RequestTimeout)
		t = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout, // the upstream sends headers only after it has transcribed
			ExpectContinueTimeout: time.Second,
			// Dictation sends a request every few minutes; a warm connection saves the TLS handshake each time.
			IdleConnTimeout:     15 * time.Minute,
			HTTP2:               &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second},
			MaxIdleConnsPerHost: 16,
			WriteBufferSize:     256 << 10, // the 4 KB default costs a syscall per 4 KB of upload
		}
		s.transports[group] = t
	}
	return t
}

// recordRequest adds a finished client request to the totals and the log.
func (s *Server) recordRequest(e LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := e.Time.Local().Format(time.DateOnly)
	if s.totals.Day != day {
		s.totals.Day, s.totals.Today = day, Counter{}
	}
	for _, c := range []*Counter{&s.totals.Today, &s.totals.All} {
		c.Requests++
		if e.Status == http.StatusOK {
			c.Success++
		}
		c.AudioSeconds += e.AudioSeconds
		c.LatencyMs += e.LatencyMs
	}
	size := s.cfg.Load().RequestLogSize
	s.reqLog = append(s.reqLog, e)
	if len(s.reqLog) > size {
		// Copy only when the slice doubles, so trimming stays cheap.
		if len(s.reqLog) >= 2*size {
			s.reqLog = append([]LogEntry(nil), s.reqLog[len(s.reqLog)-size:]...)
		}
	}
	s.markDirty()
}

// logTail returns the newest entries, at most the configured log size.
func (s *Server) logTail() []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.reqLog
	if size := s.cfg.Load().RequestLogSize; len(l) > size {
		l = l[len(l)-size:]
	}
	return append([]LogEntry(nil), l...)
}

func (s *Server) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Server) persistLoop() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-s.dirty:
		}
		select {
		case <-s.stop:
			return
		case <-time.After(saveDebounce):
		}
		if err := s.saveState(); err != nil {
			s.opts.Logger.Printf("save state: %v", err)
		}
	}
}

func (s *Server) loadState() error {
	b, err := os.ReadFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("state file %s: %w", s.statePath, err)
	}
	for k, v := range st.Accounts {
		s.accounts[k] = &account{accountState: v}
	}
	s.totals = st.Totals
	s.reqLog = st.Log
	return nil
}

func (s *Server) saveState() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	st := stateFile{Accounts: make(map[string]accountState, len(s.accounts)), Totals: s.totals}
	for k, a := range s.accounts {
		st.Accounts[k] = a.accountState
	}
	l := s.reqLog
	if size := s.cfg.Load().RequestLogSize; len(l) > size {
		l = l[len(l)-size:]
	}
	st.Log = l
	b, err := json.Marshal(st)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.statePath, b)
}
