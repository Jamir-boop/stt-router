package main

import "testing"

func TestDashboardURL(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:8330": "http://127.0.0.1:8330/",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/",
		":8330":          "http://127.0.0.1:8330/",
		"[::]:8330":      "http://127.0.0.1:8330/",
		"[::1]:8330":     "http://[::1]:8330/",
	} {
		if got := dashboardURL(listen); got != want {
			t.Errorf("dashboardURL(%q) = %q, want %q", listen, got, want)
		}
	}
}
