package media

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// hasEncoder reports whether the ffmpeg binary was built with encoder name.
// The list is read once from `ffmpeg -encoders`.
func (s *Service) hasEncoder(name string) bool {
	s.encodersOnce.Do(func() {
		s.encoders = map[string]bool{}
		if !s.hasFFmpeg {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, s.ffmpeg, "-hide_banner", "-encoders").Output()
		if err != nil {
			return
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			// Lines look like " V....D libx264  libx264 H.264 / AVC ...".
			f := strings.Fields(sc.Text())
			if len(f) >= 2 && len(f[0]) == 6 && f[0][0] == 'V' {
				s.encoders[f[1]] = true
			}
		}
	})
	return s.encoders[name]
}

// DefaultCodec is the codec used when a request does not name one: HEVC when
// the server can encode it (smaller segments at the same quality), else H.264.
func (s *Service) DefaultCodec() string {
	if slices.Contains(s.Codecs(), CodecH265) {
		return CodecH265
	}
	return CodecH264
}

// Codecs lists the output codecs transcode sessions can produce on this
// server. H.264 is always offered when ffmpeg is present (it is the
// historical default); HEVC needs libx265 or the HEVC variant of the
// configured hardware encoder.
func (s *Service) Codecs() []string {
	if !s.hasFFmpeg {
		return []string{}
	}
	out := []string{CodecH264}
	if s.hasEncoder("libx265") || s.hasEncoder(encoderForCodec(s.transcode.Encoder, CodecH265)) {
		out = append(out, CodecH265)
	}
	return out
}
