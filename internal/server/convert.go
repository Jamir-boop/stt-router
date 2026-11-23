package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
	"github.com/Jamir-boop/stt-router/internal/spool"
	"github.com/Jamir-boop/stt-router/internal/upstream"
)

// convertTimeout limits one ffmpeg run.
const convertTimeout = 2 * time.Minute

// errUnsupported marks an upload format that a target cannot take.
var errUnsupported = errors.New("unsupported audio format")

// audioPrep holds, for one request, the detected upload format and the
// converted FLAC, so the conversion runs at most once.
type audioPrep struct {
	format    string
	detected  bool
	converted *spool.Spool
	convErr   error
	converts  bool // a conversion was tried
}

func (a *audioPrep) close() {
	if a.converted != nil {
		a.converted.Close()
	}
}

// azureFile returns the upload as an azure-speech group must get it. The
// error wraps errUnsupported when the group cannot take the format, or is
// the conversion failure.
func (s *Server) azureFile(ctx context.Context, cfg *config.Config, p *config.Provider, f *clientForm, a *audioPrep) (upstream.File, error) {
	if !a.detected {
		head := make([]byte, 12)
		n, _ := io.ReadFull(f.file.Open(), head)
		a.format, a.detected = upstream.DetectAudio(head[:n]), true
	}
	ct, ok := upstream.AzureFormats[a.format]
	convert := p.Convert == config.ConvertFFmpeg
	// Diarization needs mono, and the channel count is unknown without decoding.
	if !convert || (ok && !upstream.Diarize(f.req.Extra)) {
		if !ok {
			name := a.format
			if name == "" {
				name = "unknown"
			}
			return upstream.File{}, fmt.Errorf("%w: %s (azure-speech takes WAV, MP3 or FLAC; set \"convert\": \"ffmpeg\" to convert)", errUnsupported, name)
		}
		return upstream.File{Name: "audio." + a.format, ContentType: ct, Size: f.file.Size(), Open: f.file.Open}, nil
	}
	if !a.converts {
		a.converts = true
		a.converted, a.convErr = s.toFLAC(ctx, cfg, f.file)
	}
	if a.convErr != nil {
		return upstream.File{}, a.convErr
	}
	return upstream.File{Name: "audio.flac", ContentType: "audio/flac", Size: a.converted.Size(), Open: a.converted.Open}, nil
}

// toFLAC converts the upload to 16 kHz mono FLAC with ffmpeg. Its temp files
// are in the spool directory and are removed before it returns.
func (s *Server) toFLAC(ctx context.Context, cfg *config.Config, src *spool.Spool) (*spool.Spool, error) {
	dir, err := os.MkdirTemp(s.opts.TempDir, "stt-router-convert-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "input"), filepath.Join(dir, "output.flac")
	if err := writeFile(in, src.Open()); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe",
		"-i", in, "-map", "0:a:0", "-ac", "1", "-ar", "16000", "-c:a", "flac", out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if cctx.Err() != nil {
			err = fmt.Errorf("timed out after %s", convertTimeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg != "" {
			msg = ": " + msg
		}
		return nil, fmt.Errorf("ffmpeg could not convert the audio (%v)%s", err, msg)
	}
	of, err := os.Open(out)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg wrote no output: %w", err)
	}
	defer of.Close()
	sp, err := spool.New(of, -1, int64(cfg.SpoolMemoryMB)<<20, int64(cfg.MaxUploadMB)<<20, s.opts.TempDir)
	if err != nil {
		return nil, fmt.Errorf("read converted audio: %w", err)
	}
	return sp, nil
}

func writeFile(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
