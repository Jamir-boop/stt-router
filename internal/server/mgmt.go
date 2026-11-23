package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
	"github.com/Jamir-boop/stt-router/internal/upstream"
)

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func notFound(format string, a ...any) error {
	return &httpError{http.StatusNotFound, fmt.Sprintf(format, a...)}
}

func badRequest(format string, a ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

func (s *Server) mgmtRoutes(mux *http.ServeMux) {
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			key := config.Resolve(s.cfg.Load().ManagementKey)
			if !keyEqual(bearer(r), key) {
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_management_key", "missing or invalid management key")
				return
			}
			if err := fn(w, r); err != nil {
				var he *httpError
				if !errors.As(err, &he) {
					he = &httpError{http.StatusInternalServerError, err.Error()}
				}
				writeError(w, he.status, "invalid_request_error", "", he.msg)
			}
		})
	}
	h("GET /api/status", s.apiStatus)
	h("GET /api/logs", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, http.StatusOK, map[string]any{"entries": s.logTail()})
		return nil
	})
	h("POST /api/providers/{group}/accounts", s.apiAddAccount)
	h("PUT /api/providers/{group}/accounts/{name}", s.apiEditAccount)
	h("DELETE /api/providers/{group}/accounts/{name}", s.apiDeleteAccount)
	h("POST /api/providers/{group}/accounts/{name}/reset", s.apiResetAccount)
	h("POST /api/providers/{group}/accounts/{name}/test", s.apiTestAccount)
	h("PUT /api/models/{name}", s.apiPutModel)
	h("DELETE /api/models/{name}", s.apiDeleteModel)
	h("POST /api/client-keys", s.apiAddClientKey)
	h("DELETE /api/client-keys/{name}", s.apiDeleteClientKey)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "no route for "+r.Method+" "+r.URL.Path)
	})
}

// editConfig applies fn to a copy of the config, validates it, saves it and swaps it in.
func (s *Server) editConfig(fn func(c *config.Config) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	c := s.cfg.Load().Clone()
	if err := fn(c); err != nil {
		return err
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if s.opts.ConfigPath != "" {
		if err := config.Save(s.opts.ConfigPath, c); err != nil {
			return err
		}
	}
	s.cfg.Store(c)
	return nil
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

type accountView struct {
	Name          string    `json:"name"`
	KeyMasked     string    `json:"key_masked"`
	Enabled       bool      `json:"enabled"`
	Weight        int       `json:"weight"`
	MaxConcurrent int       `json:"max_concurrent"`
	MaxFileMB     int       `json:"max_file_mb"`
	PlainJSON     bool      `json:"plain_json"`
	State         string    `json:"state"`
	CooldownUntil time.Time `json:"cooldown_until,omitzero"`
	Inflight      int       `json:"inflight"`
	Requests      int64     `json:"requests"`
	Errors        int64     `json:"errors"`
	AudioSeconds  float64   `json:"audio_seconds"`
	LastUsed      time.Time `json:"last_used,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
}

type providerView struct {
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	BaseURL  string        `json:"base_url"`
	Accounts []accountView `json:"accounts"`
	Next     string        `json:"next"` // the account pick would choose now for a small upload; empty when none is usable
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) error {
	cfg := s.cfg.Load()
	s.mu.Lock()
	now := s.now()
	totals := s.totals
	if totals.Day != now.Local().Format(time.DateOnly) {
		totals.Today = Counter{}
	}
	providers := make([]providerView, 0, len(cfg.Providers))
	for _, name := range sortedNames(cfg.Providers) {
		p := cfg.Providers[name]
		pv := providerView{Name: name, Type: p.Type, BaseURL: p.BaseURL, Accounts: []accountView{}}
		for _, a := range p.Accounts {
			st := s.acct(name, a.Name)
			v := accountView{
				Name: a.Name, KeyMasked: MaskKey(a.APIKey), Enabled: a.Enabled, Weight: max(a.Weight, 1),
				MaxConcurrent: a.MaxConcurrent, MaxFileMB: a.MaxFileMB, PlainJSON: a.PlainJSON,
				Inflight: st.inflight, Requests: st.Requests, Errors: st.Errors, AudioSeconds: st.AudioSeconds,
				LastUsed: st.LastUsed, LastError: st.LastError, LastErrorAt: st.LastErrorAt, State: "ready",
			}
			switch {
			case !a.Enabled:
				v.State = "disabled"
			case now.Before(st.CooldownUntil) && st.Invalid:
				v.State, v.CooldownUntil = "invalid", st.CooldownUntil
			case now.Before(st.CooldownUntil):
				v.State, v.CooldownUntil = "cooldown", st.CooldownUntil
			}
			pv.Accounts = append(pv.Accounts, v)
		}
		if a, _, _ := s.choose(name, p, 0, false, nil, &skips{}); a != nil {
			pv.Next = a.Name
		}
		providers = append(providers, pv)
	}
	s.mu.Unlock()

	type model struct {
		Name    string          `json:"name"`
		Targets []config.Target `json:"targets"`
	}
	models := []model{}
	for _, name := range sortedNames(cfg.Models) {
		models = append(models, model{name, cfg.Models[name]})
	}
	type key struct {
		Name      string `json:"name"`
		KeyMasked string `json:"key_masked"`
	}
	keys := []key{}
	for _, k := range cfg.ClientKeys {
		keys = append(keys, key{k.Name, MaskKey(k.Key)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       s.opts.Version,
		"now":           now,
		"listen":        s.opts.Listen,
		"client_auth":   len(cfg.ClientKeys) > 0 || !config.IsLoopback(s.opts.Listen),
		"totals":        map[string]Counter{"today": totals.Today, "all": totals.All},
		"providers":     providers,
		"models":        models,
		"client_keys":   keys,
		"max_upload_mb": cfg.MaxUploadMB,
	})
	return nil
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type accountInput struct {
	Name          *string `json:"name"`
	APIKey        *string `json:"api_key"`
	Enabled       *bool   `json:"enabled"`
	Weight        *int    `json:"weight"`
	MaxConcurrent *int    `json:"max_concurrent"`
	MaxFileMB     *int    `json:"max_file_mb"`
	PlainJSON     *bool   `json:"plain_json"`
}

func (in *accountInput) apply(a *config.Account) {
	if in.Name != nil {
		a.Name = *in.Name
	}
	if in.APIKey != nil && *in.APIKey != "" {
		a.APIKey = *in.APIKey
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if in.Weight != nil {
		a.Weight = *in.Weight
	}
	if in.MaxConcurrent != nil {
		a.MaxConcurrent = *in.MaxConcurrent
	}
	if in.MaxFileMB != nil {
		a.MaxFileMB = *in.MaxFileMB
	}
	if in.PlainJSON != nil {
		a.PlainJSON = *in.PlainJSON
	}
}

func findAccount(c *config.Config, group, name string) (*config.Provider, int, error) {
	p := c.Providers[group]
	if p == nil {
		return nil, -1, notFound("provider group %q not found", group)
	}
	for i, a := range p.Accounts {
		if a.Name == name {
			return p, i, nil
		}
	}
	return p, -1, notFound("account %q not found in %q", name, group)
}

func (s *Server) apiAddAccount(w http.ResponseWriter, r *http.Request) error {
	var in accountInput
	if err := decode(r, &in); err != nil {
		return err
	}
	group := r.PathValue("group")
	err := s.editConfig(func(c *config.Config) error {
		p := c.Providers[group]
		if p == nil {
			return notFound("provider group %q not found", group)
		}
		a := &config.Account{Enabled: true}
		in.apply(a)
		p.Accounts = append(p.Accounts, a)
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.accounts, accountKey(group, derefOr(in.Name)))
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
	return nil
}

func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Server) apiEditAccount(w http.ResponseWriter, r *http.Request) error {
	var in accountInput
	if err := decode(r, &in); err != nil {
		return err
	}
	group, name := r.PathValue("group"), r.PathValue("name")
	newName, keyChanged := name, false
	err := s.editConfig(func(c *config.Config) error {
		p, i, err := findAccount(c, group, name)
		if err != nil {
			return err
		}
		old := p.Accounts[i].APIKey
		in.apply(p.Accounts[i])
		newName, keyChanged = p.Accounts[i].Name, p.Accounts[i].APIKey != old
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	a := s.acct(group, name)
	if newName != name {
		delete(s.accounts, accountKey(group, name))
		s.accounts[accountKey(group, newName)] = a
	}
	if keyChanged {
		a.Invalid, a.CooldownUntil, a.RateFails, a.ServerFails = false, time.Time{}, 0, 0
	}
	s.markDirty()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	return nil
}

func (s *Server) apiDeleteAccount(w http.ResponseWriter, r *http.Request) error {
	group, name := r.PathValue("group"), r.PathValue("name")
	err := s.editConfig(func(c *config.Config) error {
		p, i, err := findAccount(c, group, name)
		if err != nil {
			return err
		}
		p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.accounts, accountKey(group, name))
	s.markDirty()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	return nil
}

func (s *Server) apiResetAccount(w http.ResponseWriter, r *http.Request) error {
	group, name := r.PathValue("group"), r.PathValue("name")
	if _, _, err := findAccount(s.cfg.Load(), group, name); err != nil {
		return err
	}
	s.mu.Lock()
	a := s.acct(group, name)
	a.Invalid, a.CooldownUntil, a.RateFails, a.ServerFails = false, time.Time{}, 0, 0
	s.markDirty()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	return nil
}

// apiTestAccount sends a short synthetic WAV to one account, even if it is cooling down.
func (s *Server) apiTestAccount(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Model string `json:"model"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			return err
		}
	}
	cfg := s.cfg.Load()
	group, name := r.PathValue("group"), r.PathValue("name")
	p, i, err := findAccount(cfg, group, name)
	if err != nil {
		return err
	}
	acc := p.Accounts[i]
	if in.Model == "" {
		in.Model = defaultModelFor(cfg, group)
	}
	if in.Model == "" {
		return badRequest("no model routes to %q; give a model in the request body", group)
	}
	wav := silentWAV(16000, 500*time.Millisecond)
	req := upstream.Request{File: upstream.File{
		Name: "test.wav", ContentType: "audio/wav", Size: int64(len(wav)),
		Open: func() io.Reader { return bytes.NewReader(wav) },
	}}
	start := s.now()
	res := s.attempt(r.Context(), cfg, group, p, acc, in.Model, &req)
	if res.canceled {
		return nil
	}
	s.mu.Lock()
	s.applyOutcome(s.acct(group, name), res.class, res.retry, 0, res.msg)
	s.markDirty()
	s.mu.Unlock()
	out := map[string]any{
		"ok": res.class == upstream.OK, "model": in.Model, "result": res.class.String(),
		"latency_ms": s.now().Sub(start).Milliseconds(),
	}
	if res.tr != nil {
		out["text"] = res.tr.Text
	} else {
		out["error"] = res.msg
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// defaultModelFor returns the upstream model of the first target (by model name) that uses group.
func defaultModelFor(cfg *config.Config, group string) string {
	for _, name := range sortedNames(cfg.Models) {
		for _, t := range cfg.Models[name] {
			if t.Provider == group {
				return t.Model
			}
		}
	}
	return ""
}

// silentWAV returns a mono 16-bit PCM WAV of silence.
func silentWAV(rate int, d time.Duration) []byte {
	n := int(int64(rate) * int64(d) / int64(time.Second))
	dataLen := uint32(n * 2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, 36+dataLen)
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, struct {
		Size                      uint32
		Format, Channels          uint16
		Rate, ByteRate            uint32
		BlockAlign, BitsPerSample uint16
	}{16, 1, 1, uint32(rate), uint32(rate * 2), 2, 16})
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, dataLen)
	b.Write(make([]byte, dataLen))
	return b.Bytes()
}

func (s *Server) apiPutModel(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Targets []config.Target `json:"targets"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	name := r.PathValue("name")
	if err := s.editConfig(func(c *config.Config) error {
		c.Models[name] = in.Targets
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
	return nil
}

func (s *Server) apiDeleteModel(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	if err := s.editConfig(func(c *config.Config) error {
		if _, ok := c.Models[name]; !ok {
			return notFound("model %q not found", name)
		}
		delete(c.Models, name)
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	return nil
}

// apiAddClientKey adds a client key. A generated key is returned once, in this response only.
func (s *Server) apiAddClientKey(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	generated := in.Key == ""
	if generated {
		in.Key = RandomKey("sk-sr-")
	}
	if err := s.editConfig(func(c *config.Config) error {
		c.ClientKeys = append(c.ClientKeys, config.ClientKey{Name: in.Name, Key: in.Key})
		return nil
	}); err != nil {
		return err
	}
	out := map[string]string{"status": "created", "name": in.Name, "key_masked": MaskKey(in.Key)}
	if generated {
		out["key"] = in.Key
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

func (s *Server) apiDeleteClientKey(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	if err := s.editConfig(func(c *config.Config) error {
		for i, k := range c.ClientKeys {
			if k.Name == name {
				c.ClientKeys = append(c.ClientKeys[:i], c.ClientKeys[i+1:]...)
				return nil
			}
		}
		return notFound("client key %q not found", name)
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	return nil
}
