package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadCreatesDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	path := filepath.Join(dir, "config.json")
	c, created, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if c.Listen != DefaultListen || len(c.Models) != 3 {
		t.Fatalf("unexpected default config: %+v", c)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %o, want 600", fi.Mode().Perm())
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %o, want 700", di.Mode().Perm())
		}
	}
	// A second load reads the file and does not create it again.
	if _, created, err := Load(path); err != nil || created {
		t.Fatalf("second load: created=%v err=%v", created, err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct {
		json string
		want string
	}{
		"bad type": {
			`{"providers":{"x":{"type":"foo","base_url":"https://a"}}}`,
			`providers.x.type "foo"`,
		},
		"bad url": {
			`{"providers":{"x":{"type":"openai","base_url":"ftp://a"}}}`,
			`providers.x.base_url`,
		},
		"missing key": {
			`{"providers":{"x":{"type":"openai","base_url":"https://a","accounts":[{"name":"a"}]}}}`,
			`api_key is required`,
		},
		"duplicate account": {
			`{"providers":{"x":{"type":"openai","base_url":"https://a","accounts":[{"name":"a","api_key":"k"},{"name":"a","api_key":"k"}]}}}`,
			`duplicate name "a"`,
		},
		"unknown provider in model": {
			`{"models":{"m":[{"provider":"nope","model":"x"}]}}`,
			`unknown provider "nope"`,
		},
		"empty chain": {
			`{"models":{"m":[]}}`,
			`needs at least one target`,
		},
		"unknown field": {
			`{"listen":"127.0.0.1:1","bogus":1}`,
			`unknown field "bogus"`,
		},
		"unknown account field": {
			`{"providers":{"x":{"type":"openai","base_url":"https://a","accounts":[{"name":"a","api_key":"k","wieght":2}]}}}`,
			`unknown field "wieght"`,
		},
		"bad duration": {
			`{"request_timeout":"ten"}`,
			`invalid duration`,
		},
		"bad listen": {
			`{"listen":"8330"}`,
			`listen "8330"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.json")
			if err := os.WriteFile(path, []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestEnvReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	body := `{"providers":{"g":{"type":"openai","base_url":"https://a/","accounts":[{"name":"a","api_key":"env:STT_ROUTER_TEST_KEY"}]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "STT_ROUTER_TEST_KEY is not set") {
		t.Fatalf("err = %v, want missing env error", err)
	}
	t.Setenv("STT_ROUTER_TEST_KEY", "secret-from-env")
	c, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := c.Providers["g"].Accounts[0]
	if a.APIKey != "env:STT_ROUTER_TEST_KEY" {
		t.Errorf("raw key = %q, the reference must stay in the config", a.APIKey)
	}
	if Resolve(a.APIKey) != "secret-from-env" {
		t.Errorf("resolved key = %q", Resolve(a.APIKey))
	}
	if !a.Enabled {
		t.Error("enabled must default to true")
	}
	if c.Providers["g"].BaseURL != "https://a" {
		t.Errorf("base_url = %q, want trailing slash removed", c.Providers["g"].BaseURL)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	t.Setenv("GROQ_API_KEY_2", "x")
	c, created, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if len(c.Models["auto"]) != 2 || len(c.Providers["groq"].Accounts) != 2 || len(c.Providers["elevenlabs"].Accounts) != 2 {
		t.Fatalf("unexpected example config: %+v", c)
	}
}

func TestIsLoopback(t *testing.T) {
	for in, want := range map[string]bool{
		"127.0.0.1:8330": true, "[::1]:8330": true, "localhost:1": true,
		"0.0.0.0:8330": false, ":8330": false, "192.168.1.2:80": false,
	} {
		if got := IsLoopback(in); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", in, got, want)
		}
	}
}
