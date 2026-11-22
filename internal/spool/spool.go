// Package spool stores an upload once, in memory up to a limit and then in a
// temp file, so it can be replayed for each upstream attempt.
package spool

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
)

// ErrTooLarge is returned when the upload is bigger than the max size.
var ErrTooLarge = errors.New("upload is too large")

// Spool is a write-once, read-many buffer.
type Spool struct {
	mem  []byte
	file *os.File
	size int64
}

// New reads r fully. Data up to memLimit stays in memory; the rest moves to a
// temp file in dir. It fails with ErrTooLarge when r has more than maxSize
// bytes. sizeHint is the expected size (or -1); it presizes the memory buffer.
func New(r io.Reader, sizeHint, memLimit, maxSize int64, dir string) (*Spool, error) {
	s := &Spool{}
	var buf bytes.Buffer
	if sizeHint > 0 {
		buf.Grow(int(min(sizeHint, memLimit) + bytes.MinRead))
	}
	n, err := io.Copy(&buf, io.LimitReader(r, memLimit+1))
	if err != nil {
		return nil, err
	}
	if n <= memLimit {
		if n > maxSize {
			return nil, ErrTooLarge
		}
		s.mem, s.size = buf.Bytes(), n
		return s, nil
	}
	f, err := os.CreateTemp(dir, "stt-router-*.upload")
	if err != nil {
		return nil, err
	}
	s.file = f
	// The multipart reader gives small chunks; a big buffer saves write syscalls.
	w := bufio.NewWriterSize(f, 256<<10)
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.Close()
		return nil, err
	}
	m, err := io.Copy(w, io.LimitReader(r, maxSize-n+1))
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	s.size = n + m
	if s.size > maxSize {
		s.Close()
		return nil, ErrTooLarge
	}
	return s, nil
}

// Size is the number of bytes stored.
func (s *Spool) Size() int64 { return s.size }

// InMemory reports whether the data stayed in memory.
func (s *Spool) InMemory() bool { return s.file == nil }

// Open returns a new reader from the start. Readers are independent.
func (s *Spool) Open() io.Reader {
	if s.file == nil {
		return bytes.NewReader(s.mem)
	}
	return io.NewSectionReader(s.file, 0, s.size)
}

// Close removes the temp file, if any.
func (s *Spool) Close() error {
	if s.file == nil {
		return nil
	}
	name := s.file.Name()
	s.file.Close()
	s.file = nil
	return os.Remove(name)
}
