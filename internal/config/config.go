// Package config loads, validates and saves the stt-router JSON config.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Provider types.
const (
	TypeOpenAI     = "openai"
	TypeElevenLabs = "elevenlabs"
)

// Defaults for optional settings.
const (
	DefaultListen         = "127.0.0.1:8330"
	DefaultMaxUploadMB    = 1024
	DefaultSpoolMemoryMB  = 8
	DefaultRequestTimeout = 10 * time.Minute
	DefaultQuotaCooldown  = time.Hour
	DefaultRequestLogSize = 500
	DefaultElevenLabsURL  = "https://api.elevenlabs.io"
)

// Config is the whole config file.
type Config struct {
	Listen         string               `json:"listen"`
	ManagementKey  string               `json:"management_key"`
	ClientKeys     []ClientKey          `json:"client_keys"`
	StateFile      string               `json:"state_file,omitempty"`
	MaxUploadMB    int                  `json:"max_upload_mb"`
	SpoolMemoryMB  int                  `json:"spool_memory_mb"`
	RequestTimeout Duration             `json:"request_timeout"`
	QuotaCooldown  Duration             `json:"quota_cooldown"`
	RequestLogSize int                  `json:"request_log_size"`
	FrameAncestors []string             `json:"frame_ancestors,omitempty"`
	Providers      map[string]*Provider `json:"providers"`
	Models         map[string][]Target  `json:"models"`
}

// ClientKey is a key that clients send as a Bearer token.
type ClientKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Provider is a group of accounts at one upstream.
type Provider struct {
	Type     string     `json:"type"`
	BaseURL  string     `json:"base_url"`
	Accounts []*Account `json:"accounts"`
}

// Account is one upstream API key.
type Account struct {
	Name          string `json:"name"`
	APIKey        string `json:"api_key"`
	Enabled       bool   `json:"enabled"`
	Weight        int    `json:"weight,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	MaxFileMB     int    `json:"max_file_mb,omitempty"`
	PlainJSON     bool   `json:"plain_json,omitempty"`
}

// UnmarshalJSON makes "enabled" default to true when it is omitted.
func (a *Account) UnmarshalJSON(b []byte) error {
	type plain Account
	p := plain{Enabled: true}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*a = Account(p)
	return nil
}

// Target is one step of a model's fallback chain.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Duration is a time.Duration written as a Go duration string ("10m").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"10m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Default returns the config written at first start.
func Default() *Config {
	c := &Config{
		Listen:     DefaultListen,
		ClientKeys: []ClientKey{},
		Providers: map[string]*Provider{
			"groq":       {Type: TypeOpenAI, BaseURL: "https://api.groq.com/openai/v1", Accounts: []*Account{}},
			"elevenlabs": {Type: TypeElevenLabs, BaseURL: DefaultElevenLabsURL, Accounts: []*Account{}},
		},
		Models: map[string][]Target{
			"whisper-large-v3-turbo": {{Provider: "groq", Model: "whisper-large-v3-turbo"}},
			"scribe_v3":              {{Provider: "elevenlabs", Model: "scribe_v3"}},
			"auto": {
				{Provider: "groq", Model: "whisper-large-v3-turbo"},
				{Provider: "elevenlabs", Model: "scribe_v3"},
			},
		},
	}
	c.ApplyDefaults()
	return c
}

// ApplyDefaults fills zero values with defaults.
func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.MaxUploadMB == 0 {
		c.MaxUploadMB = DefaultMaxUploadMB
	}
	if c.SpoolMemoryMB == 0 {
		c.SpoolMemoryMB = DefaultSpoolMemoryMB
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = Duration(DefaultRequestTimeout)
	}
	if c.QuotaCooldown == 0 {
		c.QuotaCooldown = Duration(DefaultQuotaCooldown)
	}
	if c.RequestLogSize == 0 {
		c.RequestLogSize = DefaultRequestLogSize
	}
	if c.ClientKeys == nil {
		c.ClientKeys = []ClientKey{}
	}
	if c.Providers == nil {
		c.Providers = map[string]*Provider{}
	}
	if c.Models == nil {
		c.Models = map[string][]Target{}
	}
	for _, p := range c.Providers {
		if p == nil {
			continue
		}
		if p.Type == TypeElevenLabs && p.BaseURL == "" {
			p.BaseURL = DefaultElevenLabsURL
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.Accounts == nil {
			p.Accounts = []*Account{}
		}
	}
}

// Resolve returns the key value, reading "env:NAME" references from the environment.
func Resolve(v string) string {
	if name, ok := strings.CutPrefix(v, "env:"); ok {
		return os.Getenv(name)
	}
	return v
}

// Validate checks the config and returns all problems in one error.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		add("listen %q: must be host:port", c.Listen)
	}
	checkKey := func(where, v string) {
		if name, ok := strings.CutPrefix(v, "env:"); ok && os.Getenv(name) == "" {
			add("%s: environment variable %s is not set or empty", where, name)
		}
	}
	if c.ManagementKey != "" {
		checkKey("management_key", c.ManagementKey)
	}
	if c.MaxUploadMB < 1 {
		add("max_upload_mb must be at least 1")
	}
	if c.SpoolMemoryMB < 0 {
		add("spool_memory_mb must not be negative")
	}
	if c.RequestTimeout < 0 {
		add("request_timeout must not be negative")
	}
	if c.QuotaCooldown < 0 {
		add("quota_cooldown must not be negative")
	}
	if c.RequestLogSize < 0 {
		add("request_log_size must not be negative")
	}

	for i, o := range c.FrameAncestors {
		if !validOrigin(o) {
			add("frame_ancestors[%d] %q: must be an origin such as http://127.0.0.1:8790 (http or https, a host, an optional port, no path, no wildcard)", i, o)
		}
	}

	names, keys := map[string]bool{}, map[string]bool{}
	for i, k := range c.ClientKeys {
		where := fmt.Sprintf("client_keys[%d]", i)
		switch {
		case k.Name == "":
			add("%s: name is required", where)
		case names[k.Name]:
			add("%s: duplicate name %q", where, k.Name)
		}
		names[k.Name] = true
		if k.Key == "" {
			add("%s: key is required", where)
		} else if keys[k.Key] {
			add("%s: duplicate key", where)
		}
		keys[k.Key] = true
		checkKey(where+".key", k.Key)
	}

	for _, pname := range sortedKeys(c.Providers) {
		p := c.Providers[pname]
		where := fmt.Sprintf("providers.%s", pname)
		if !validName(pname) {
			add("%s: name must be non-empty and must not contain '/'", where)
		}
		if p == nil {
			add("%s: is null", where)
			continue
		}
		if p.Type != TypeOpenAI && p.Type != TypeElevenLabs {
			add("%s.type %q: must be %q or %q", where, p.Type, TypeOpenAI, TypeElevenLabs)
		}
		if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("%s.base_url %q: must be an absolute http or https URL", where, p.BaseURL)
		}
		seen := map[string]bool{}
		for i, a := range p.Accounts {
			aw := fmt.Sprintf("%s.accounts[%d]", where, i)
			if a == nil {
				add("%s: is null", aw)
				continue
			}
			if !validName(a.Name) {
				add("%s: name must be non-empty and must not contain '/'", aw)
			} else if seen[a.Name] {
				add("%s: duplicate name %q", aw, a.Name)
			}
			seen[a.Name] = true
			if a.APIKey == "" {
				add("%s (%s): api_key is required", aw, a.Name)
			}
			checkKey(aw+".api_key", a.APIKey)
			if a.Weight < 0 || a.MaxConcurrent < 0 || a.MaxFileMB < 0 {
				add("%s (%s): weight, max_concurrent and max_file_mb must not be negative", aw, a.Name)
			}
		}
	}

	for _, m := range sortedKeys(c.Models) {
		where := fmt.Sprintf("models.%s", m)
		if m == "" {
			add("models: model name must not be empty")
		}
		ts := c.Models[m]
		if len(ts) == 0 {
			add("%s: needs at least one target", where)
		}
		for i, t := range ts {
			if _, ok := c.Providers[t.Provider]; !ok {
				add("%s[%d]: unknown provider %q", where, i, t.Provider)
			}
			if t.Model == "" {
				add("%s[%d]: model is required", where, i)
			}
		}
	}

	if len(errs) > 0 {
		return errors.New("invalid config:\n  " + strings.Join(errs, "\n  "))
	}
	return nil
}

// validOrigin accepts scheme://host[:port] and nothing else, because the
// value goes into the Content-Security-Policy header as written.
func validOrigin(o string) bool {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || o != u.Scheme+"://"+u.Host || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	for _, r := range u.Hostname() {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func validName(s string) bool { return s != "" && !strings.Contains(s, "/") }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Load reads and validates the config at path. If the file does not exist,
// it writes Default() there and reports created = true.
func Load(path string) (c *Config, created bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c = Default()
		if err := Save(path, c); err != nil {
			return nil, false, err
		}
		return c, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	c = &Config{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return c, false, nil
}

// Save writes the config atomically with mode 0600 (directory 0700).
func Save(path string, c *Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'))
}

// WriteFileAtomic writes data to a temp file in the same directory and renames it.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Clone returns a deep copy, so callers can change it without data races.
func (c *Config) Clone() *Config {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

// DefaultPath is $XDG_CONFIG_HOME/stt-router/config.json (or the OS equivalent).
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "stt-router", "config.json")
}

// DefaultStatePath is $XDG_STATE_HOME/stt-router/state.json, falling back to ~/.local/state.
func DefaultStatePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "stt-router", "state.json")
}

// IsLoopback reports whether the listen address binds only to loopback.
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
