package spool

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

func TestSpool(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 1000)
	cases := []struct {
		name     string
		mem      int64
		inMemory bool
	}{{"memory", 1 << 20, true}, {"file", 100, false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := New(bytes.NewReader(data), int64(len(data)), tc.mem, 1<<20, dir)
			if err != nil {
				t.Fatal(err)
			}
			if s.InMemory() != tc.inMemory || s.Size() != int64(len(data)) {
				t.Fatalf("InMemory=%v Size=%d", s.InMemory(), s.Size())
			}
			for i := 0; i < 2; i++ {
				got, err := io.ReadAll(s.Open())
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("replay %d differs (err %v)", i, err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Fatalf("temp file left: %v", ents)
			}
		})
	}
}

func TestSpoolTooLarge(t *testing.T) {
	dir := t.TempDir()
	for _, mem := range []int64{10, 1 << 20} {
		_, err := New(bytes.NewReader(make([]byte, 5000)), -1, mem, 4000, dir)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("mem=%d: err = %v, want ErrTooLarge", mem, err)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("temp file left: %v", ents)
	}
}
