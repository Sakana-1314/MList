package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sakana-1314/MList/server/internal/config"
)

func TestSegmentCount(t *testing.T) {
	cases := []struct {
		d    float64
		seg  int
		want int
	}{
		{60, 6, 10}, {60.02, 6, 10}, {60.9, 6, 10}, {61.0, 6, 11}, {61.5, 6, 11},
		{3, 6, 1}, {0.4, 6, 1}, {6, 6, 1}, {6.5, 6, 1}, {7.2, 6, 2}, {12.5, 6, 2}, {13.5, 6, 3},
	}
	for _, c := range cases {
		if got := segmentCount(c.d, c.seg); got != c.want {
			t.Errorf("segmentCount(%v,%d)=%d want %d", c.d, c.seg, got, c.want)
		}
	}
}

type hlsSettings struct {
	encoder     string
	maxSessions int
	segSeconds  int
	// ttlHours is the transcode cache TTL; 0 means never expires.
	ttlHours int
}

func hlsEnv(t *testing.T, o hlsSettings) *testEnv {
	if o.encoder == "" {
		o.encoder = "libx264"
	}
	if o.maxSessions == 0 {
		o.maxSessions = 4
	}
	if o.segSeconds == 0 {
		o.segSeconds = 6
	}
	return newEnv(t, func(s *config.Config) {
		s.Transcode.Encoder = o.encoder
		s.Transcode.MaxSessions = o.maxSessions
		s.Transcode.SegmentSeconds = o.segSeconds
		s.Transcode.Preset = "veryfast"
		s.Cache.TranscodeTTLHours = o.ttlHours
	})
}

func createSession(t *testing.T, env *testEnv, user uint, p, quality string, audio int) *hlsSession {
	t.Helper()
	se, err := env.svc.CreateSession(context.Background(), user, fmt.Sprint("u", user), p, "/v/"+filepath.Base(p), stat(t, p), TranscodeOptions{Quality: quality, Codec: CodecH264, Audio: audio})
	if err != nil {
		t.Fatal(err)
	}
	return se
}

// fetch requests segment n and returns a private copy (what a player would have
// downloaded) plus the request latency.
func fetch(t *testing.T, env *testEnv, se *hlsSession, user uint, n int, outDir string) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	p, err := env.svc.Segment(context.Background(), se.ID, user, n)
	if err != nil {
		t.Fatalf("segment %d: %v", n, err)
	}
	lat := time.Since(start)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read segment %d: %v", n, err)
	}
	cp := filepath.Join(outDir, fmt.Sprintf("%03d.ts", n))
	os.WriteFile(cp, b, 0o644)
	return cp, lat
}

func runningJobs(env *testEnv) []*hlsJob {
	m := env.svc.hls
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningJobs()
}

type segInfo struct {
	videoStart, audioStart float64
	firstKey               bool
	width, height          int
	vcodec, profile, pix   string
	acodec, aprofile       string
	channels               int
	rate                   string
}

func inspectSegment(t *testing.T, p string) segInfo {
	t.Helper()
	r := ffprobeJSON(t, p, "-show_packets", "-show_entries", "packet=stream_index,pts_time,flags:stream=index,codec_type,codec_name,profile,width,height,pix_fmt,channels,sample_rate,start_time:format=start_time,duration")
	var si segInfo
	v, a := r.stream("video"), r.stream("audio")
	if v == nil {
		t.Fatalf("%s has no video", p)
	}
	si.width, si.height, si.vcodec, si.profile, si.pix = v.Width, v.Height, v.CodecName, v.Profile, v.PixFmt
	si.videoStart = math.Inf(1)
	si.audioStart = math.Inf(1)
	first := true
	for _, pk := range r.Packets {
		ts := f64(pk.PtsTime)
		if pk.StreamIndex == v.Index {
			if first {
				si.firstKey = strings.Contains(pk.Flags, "K")
				first = false
			}
			si.videoStart = math.Min(si.videoStart, ts)
		} else if a != nil && pk.StreamIndex == a.Index {
			si.audioStart = math.Min(si.audioStart, ts)
		}
	}
	if a != nil {
		si.acodec, si.aprofile, si.channels, si.rate = a.CodecName, a.Profile, a.Channels, a.SampleRate
	}
	return si
}

// checkContinuous concatenates segments (as a player's demuxer would see them)
// and verifies decodability and timestamp continuity.
func checkContinuous(t *testing.T, segs []string, wantDur, fps float64, hasAudio bool) {
	t.Helper()
	all := filepath.Join(filepath.Dir(segs[0]), "all.ts")
	f, _ := os.Create(all)
	for _, s := range segs {
		b, _ := os.ReadFile(s)
		f.Write(b)
	}
	f.Close()
	if out, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", all, "-map", "0:v", "-f", "null", "-").CombinedOutput(); err != nil || len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("decoding concatenated video: %v\n%s", err, out)
	}
	if hasAudio {
		// Joins between ffmpeg runs repeat one or two AAC frames on purpose
		// (players drop them); the null muxer complains about those, nothing else is allowed.
		out, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", all, "-map", "0:a", "-f", "null", "-").CombinedOutput()
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" && !strings.Contains(l, "non monotonically increasing dts") {
				t.Fatalf("decoding concatenated audio: %v\n%s", err, out)
			}
		}
	}
	r := ffprobeJSON(t, all, "-show_packets", "-show_entries", "packet=stream_index,pts_time,dts_time,duration_time,flags:stream=index,codec_type")
	v, a := r.stream("video"), r.stream("audio")
	var vpts, apts []float64
	for _, pk := range r.Packets {
		if pk.StreamIndex == v.Index {
			vpts = append(vpts, f64(pk.PtsTime))
		} else if a != nil && pk.StreamIndex == a.Index {
			apts = append(apts, f64(pk.PtsTime))
		}
	}
	sortF(vpts)
	sortF(apts)
	frame := 1 / fps
	for i := 1; i < len(vpts); i++ {
		if d := vpts[i] - vpts[i-1]; d < frame*0.5 || d > frame*1.5 {
			t.Errorf("video discontinuity at %.4f: delta %.4f (frame %.4f)", vpts[i-1], d, frame)
		}
	}
	span := vpts[len(vpts)-1] - vpts[0] + frame
	if math.Abs(span-wantDur) > 0.1 {
		t.Errorf("video span %.3f, want %.3f", span, wantDur)
	}
	if hasAudio {
		// Emulate the player: drop audio frames already covered (overlap at run
		// joins), then require a gapless, grid-aligned sequence.
		dups := 0
		kept := []float64{apts[0]}
		for _, p := range apts[1:] {
			if p < kept[len(kept)-1]+aacFrame/2 {
				if math.Abs(p-kept[len(kept)-1]) > 0.0005 && p > kept[len(kept)-1]-aacFrame*8 {
					// An overlapping frame that is not on the same grid.
					found := false
					for _, k := range kept[max(0, len(kept)-10):] {
						if math.Abs(k-p) < 0.0005 {
							found = true
						}
					}
					if !found {
						t.Errorf("audio frame at %.4f is off the AAC grid", p)
					}
				}
				dups++
				continue
			}
			kept = append(kept, p)
		}
		sortF(kept)
		for i := 1; i < len(kept); i++ {
			if d := kept[i] - kept[i-1]; math.Abs(d-aacFrame) > 0.0005 {
				t.Errorf("audio discontinuity at %.4f: delta %.4f", kept[i-1], d)
			}
		}
		if aspan := kept[len(kept)-1] - kept[0]; math.Abs(aspan-wantDur) > 0.15 {
			t.Errorf("audio span %.3f, want %.3f", aspan, wantDur)
		}
		if dups > 0 {
			t.Logf("%d duplicated AAC frames at run joins", dups)
		}
	}
	t.Logf("concatenated %d segments: %d video / %d audio packets, span %.3fs", len(segs), len(vpts), len(apts), span)
}

func sortF(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func playlistDurations(t *testing.T, pl string) []float64 {
	var out []float64
	for _, l := range strings.Split(pl, "\n") {
		if strings.HasPrefix(l, "#EXTINF:") {
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(l, "#EXTINF:"), ","), 64)
			if err != nil {
				t.Fatal(l)
			}
			out = append(out, v)
		}
	}
	return out
}

// TestHLSSeekAndAlignment is the main end-to-end check: VOD playlist, sequential
// playback, a seek that restarts ffmpeg, and segments from three different
// ffmpeg runs concatenated into one continuous stream.
func TestHLSSeekAndAlignment(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	old := restartCost
	restartCost = 0 // deterministic: only the segment in production counts as covered
	defer func() { restartCost = old }()

	env := hlsEnv(t, hlsSettings{})
	src := movieMKV(t)
	se := createSession(t, env, 1, src, "720p", 0)
	out := t.TempDir()

	pl, err := env.svc.Playlist(se.ID, 1, "access_token=abc")
	if err != nil {
		t.Fatal(err)
	}
	durs := playlistDurations(t, pl)
	wantN := int(math.Ceil(se.Duration/6 - 1e-9))
	if se.Duration-float64((wantN-1)*6) < minTail {
		wantN--
	}
	sum := 0.0
	for _, d := range durs {
		sum += d
	}
	if len(durs) != wantN || len(durs) != 10 || math.Abs(sum-se.Duration) > 1e-3 || durs[0] != 6 ||
		!strings.Contains(pl, "#EXT-X-TARGETDURATION:7") || !strings.Contains(pl, "0.ts?access_token=abc") ||
		!strings.HasSuffix(pl, "#EXT-X-ENDLIST\n") {
		t.Fatalf("playlist (duration %.3f):\n%s", se.Duration, pl)
	}
	if _, err := env.svc.Playlist(se.ID, 2, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("other user must not see the session")
	}
	if _, err := env.svc.Segment(context.Background(), se.ID, 1, 10); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("out of range segment")
	}

	segs := make([]string, 10)
	var lat time.Duration
	segs[0], lat = fetch(t, env, se, 1, 0, out)
	t.Logf("time to first segment (libx264 veryfast 720p): %v", lat)
	segs[1], _ = fetch(t, env, se, 1, 1, out)
	segs[2], _ = fetch(t, env, se, 1, 2, out)

	// Seek: segment 8 is neither on disk nor next in line.
	jobs := runningJobs(env)
	if len(jobs) != 1 || jobs[0].start != 0 {
		t.Fatalf("jobs before seek: %v", jobs)
	}
	firstJob := jobs[0]
	env.svc.hls.jobMu.Lock()
	aheadOK := firstJob.next < 8
	env.svc.hls.jobMu.Unlock()
	segs[8], lat = fetch(t, env, se, 1, 8, out)
	t.Logf("seek restart latency (segment 8): %v", lat)
	if aheadOK {
		jobs = runningJobs(env)
		if len(jobs) != 1 || jobs[0].start != 8 {
			t.Fatalf("expected a single job restarted at 8, got %+v", jobs)
		}
		if !firstJob.isFinished() || !firstJob.killed {
			t.Fatal("previous job of the same user should have been stopped")
		}
	}
	segs[9], _ = fetch(t, env, se, 1, 9, out)
	// Seek back: 3..7 come from a third run.
	for i := 3; i <= 7; i++ {
		segs[i], _ = fetch(t, env, se, 1, i, out)
	}

	base := inspectSegment(t, segs[0])
	t.Logf("segment 0: video starts at %.4f, audio at %.4f", base.videoStart, base.audioStart)
	if base.vcodec != "h264" || base.profile != "High" || base.pix != "yuv420p" || base.width != 1280 || base.height != 720 ||
		base.acodec != "aac" || base.aprofile != "LC" || base.channels != 2 || base.rate != "48000" {
		t.Fatalf("codec params %+v", base)
	}
	fps := 24000.0 / 1001
	for i, s := range segs {
		si := inspectSegment(t, s)
		if !si.firstKey {
			t.Errorf("segment %d does not start with a keyframe", i)
		}
		// Exactly the first frame at or after the boundary, whichever run made it.
		want := base.videoStart + math.Ceil(float64(i*6)*fps-1e-6)/fps
		if d := si.videoStart - want; math.Abs(d) > 0.0015 {
			t.Errorf("segment %d video starts at %.4f, want %.4f", i, si.videoStart, want)
		}
	}
	seg8 := inspectSegment(t, segs[8])
	t.Logf("segment 8 first PTS %.4f = base + %.4f (want 48)", seg8.videoStart, seg8.videoStart-base.videoStart)
	checkContinuous(t, segs, se.Duration, 24000.0/1001, true)

	// No working directories or temp files are left in the session directory.
	env.svc.StopSession(se.ID, 1)
	names, _ := filepath.Glob(filepath.Join(se.Dir, "*"))
	for _, n := range names {
		if !strings.HasSuffix(n, ".ts") {
			t.Errorf("leftover %s", n)
		}
	}
}

func TestHLSSequentialPlaybackSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	for _, enc := range []string{"libx264", "h264_videotoolbox"} {
		t.Run(enc, func(t *testing.T) {
			if enc == "h264_videotoolbox" && !hasEncoder(t, enc) {
				t.Skip("videotoolbox not available")
			}
			env := hlsEnv(t, hlsSettings{encoder: enc})
			src := movieMKV(t)
			se := createSession(t, env, 1, src, "720p", 0)
			out := t.TempDir()
			start := time.Now()
			var segs []string
			var first time.Duration
			usedEnc := ""
			for i := 0; i < se.NSeg; i++ {
				p, lat := fetch(t, env, se, 1, i, out)
				if i == 0 {
					first = lat
					env.svc.hls.jobMu.Lock()
					for _, j := range se.jobs {
						usedEnc = j.encoder
					}
					env.svc.hls.jobMu.Unlock()
				}
				segs = append(segs, p)
			}
			total := time.Since(start)
			if usedEnc != enc {
				t.Fatalf("job used encoder %q", usedEnc)
			}
			t.Logf("%s: first segment %v, all %d segments (%.0fs of video) in %v (%.1fx realtime), encoder=%s",
				enc, first, se.NSeg, se.Duration, total, se.Duration/total.Seconds(), usedEnc)
			si := inspectSegment(t, segs[0])
			if si.vcodec != "h264" || si.profile != "High" || si.pix != "yuv420p" {
				t.Fatalf("%+v", si)
			}
			for i, s := range segs {
				if !inspectSegment(t, s).firstKey {
					t.Errorf("segment %d: no leading keyframe", i)
				}
			}
			checkContinuous(t, segs, se.Duration, 24000.0/1001, true)
		})
	}
}

func hasEncoder(t *testing.T, name string) bool {
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:duration=0.2",
		"-c:v", name, "-f", "null", "-").CombinedOutput()
	return len(strings.TrimSpace(string(out))) == 0
}

func TestHLSAudioTrackAndQuality(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	env := hlsEnv(t, hlsSettings{})
	src := movieMKV(t)
	out := t.TempDir()

	loud := createSession(t, env, 1, src, "480p", 0)
	silent := createSession(t, env, 1, src, "480p", 1)
	if loud.ID == silent.ID {
		t.Fatal("audio track must be part of the session id")
	}
	for _, se := range []*hlsSession{loud, silent} {
		p, _ := fetch(t, env, se, 1, 2, out)
		si := inspectSegment(t, p)
		if si.height != 480 || si.width != 854 {
			t.Fatalf("480p: got %dx%d", si.width, si.height)
		}
		vol, _ := exec.Command("ffmpeg", "-v", "info", "-hide_banner", "-i", p, "-map", "0:a", "-af", "volumedetect", "-f", "null", "-").CombinedOutput()
		i := strings.Index(string(vol), "mean_volume: ")
		if i < 0 {
			t.Fatalf("volumedetect: %s", vol)
		}
		mean, _ := strconv.ParseFloat(strings.Fields(string(vol)[i+13:])[0], 64)
		silentWanted := se == silent
		if (mean < -80) != silentWanted {
			t.Fatalf("audio track %d mean volume %.1f dB", se.Audio, mean)
		}
		// Each ffprobe'd 5.1 source track is downmixed to stereo.
		if si.channels != 2 {
			t.Fatalf("channels %d", si.channels)
		}
	}
	// Invalid quality / track fall back to defaults.
	se := createSession(t, env, 1, src, "8k", 7)
	if se.Quality != "720p" || se.Audio != 0 {
		t.Fatalf("defaults: %s %d", se.Quality, se.Audio)
	}
}

func TestHLSConcurrencyCap(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	env := hlsEnv(t, hlsSettings{maxSessions: 1})
	src := movieMKV(t)
	a := createSession(t, env, 1, src, "720p", 0)
	b := createSession(t, env, 2, src, "480p", 0)
	out := t.TempDir()

	// Sample the number of ffmpeg jobs while both viewers play alternately.
	stop := make(chan struct{})
	maxSeen := 0
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if n := len(runningJobs(env)); n > maxSeen {
				maxSeen = n
			}
		}
	}()

	fetch(t, env, a, 1, 0, out+"/")
	ja := runningJobs(env)
	fetch(t, env, b, 2, 0, out+"/")
	if len(ja) != 1 || !ja[0].isFinished() || !ja[0].killed {
		t.Fatal("older job should have been stopped for the new session")
	}
	// Both viewers keep being served (each preempts the idle other).
	dirA, dirB := t.TempDir(), t.TempDir()
	var errA, errB error
	var wg2 sync.WaitGroup
	wg2.Add(2)
	play := func(se *hlsSession, user uint, dir string, errp *error) {
		defer wg2.Done()
		for i := 1; i < 6; i++ {
			p, err := env.svc.Segment(context.Background(), se.ID, user, i)
			if err != nil {
				*errp = fmt.Errorf("segment %d: %w", i, err)
				return
			}
			b, _ := os.ReadFile(p)
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d.ts", i)), b, 0o644)
			time.Sleep(50 * time.Millisecond) // player consuming
		}
	}
	go play(a, 1, dirA, &errA)
	go play(b, 2, dirB, &errB)
	wg2.Wait()
	close(stop)
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("A: %v, B: %v", errA, errB)
	}
	if maxSeen > 1 {
		t.Fatalf("saw %d concurrent ffmpeg jobs with MaxSessions=1", maxSeen)
	}
}

func TestHLSIdleJanitorAndStop(t *testing.T) {
	oldIdle, oldTick := jobIdleTimeout, janitorInterval
	jobIdleTimeout, janitorInterval = 700*time.Millisecond, 100*time.Millisecond
	env := hlsEnv(t, hlsSettings{})
	jobIdleTimeout, janitorInterval = oldIdle, oldTick

	src := fixture(t, "idle.mkv", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=120",
		"-c:v", "libx264", "-preset", "ultrafast")
	se := createSession(t, env, 1, src, "720p", 0)
	if _, err := env.svc.Segment(context.Background(), se.ID, 1, 0); err != nil {
		t.Fatal(err)
	}
	jobs := runningJobs(env)
	if len(jobs) != 1 {
		t.Fatalf("jobs %d", len(jobs))
	}
	pid := jobs[0].cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for len(runningJobs(env)) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(runningJobs(env)) != 0 {
		t.Fatal("idle job not stopped by the janitor")
	}
	if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err == nil {
		t.Fatalf("ffmpeg pid %d still alive", pid)
	}
	infos := env.svc.Sessions()
	if len(infos) != 1 || infos[0].Running || infos[0].User != "u1" {
		t.Fatalf("sessions %+v", infos)
	}

	// StopSession kills a running job; the session and its segments remain usable.
	if _, err := env.svc.Segment(context.Background(), se.ID, 1, 5); err != nil {
		t.Fatal(err)
	}
	if !env.svc.Sessions()[0].Running {
		t.Fatal("expected a running job")
	}
	if err := env.svc.StopSession(se.ID, 1); err != nil {
		t.Fatal(err)
	}
	if env.svc.Sessions()[0].Running {
		t.Fatal("job still running after StopSession")
	}
	if err := env.svc.StopSession(se.ID, 99); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("foreign user stop")
	}
	if _, err := env.svc.Segment(context.Background(), se.ID, 1, 6); err != nil {
		t.Fatal(err)
	}
}

func TestHLSEncoderFallback(t *testing.T) {
	if hasEncoder(t, "h264_nvenc") {
		t.Skip("nvenc is actually available here")
	}
	env := hlsEnv(t, hlsSettings{encoder: "h264_nvenc"})
	src := fixture(t, "fallback.mp4", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25:duration=20",
		"-f", "lavfi", "-i", "sine=duration=20", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac")
	se := createSession(t, env, 1, src, "720p", 0)
	start := time.Now()
	p, err := env.svc.Segment(context.Background(), se.ID, 1, 0)
	if err != nil {
		t.Fatalf("fallback did not happen: %v", err)
	}
	t.Logf("first segment after nvenc failure + libx264 fallback: %v", time.Since(start))
	if si := inspectSegment(t, p); si.vcodec != "h264" {
		t.Fatal(si)
	}
	env.svc.hls.encMu.Lock()
	_, broken := env.svc.hls.brokenEncDue["h264_nvenc"]
	env.svc.hls.encMu.Unlock()
	if !broken {
		t.Fatal("nvenc should be remembered as broken")
	}
	// A new session uses libx264 right away.
	se2 := createSession(t, env, 1, src, "480p", 0)
	env.svc.Segment(context.Background(), se2.ID, 1, 0)
	env.svc.hls.jobMu.Lock()
	for _, j := range se2.jobs {
		if j.encoder != "libx264" {
			t.Errorf("new job uses %s", j.encoder)
		}
	}
	env.svc.hls.jobMu.Unlock()
}

// TestHLSEdgeCases covers sources that commonly break naive transcoding.
func TestHLSEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	env := hlsEnv(t, hlsSettings{})
	cases := []struct {
		name     string
		args     []string
		fps      float64
		hasAudio bool
		w, h     int
		nseg     int
	}{
		{"hevc10-offset.ts", []string{"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=25:duration=20", "-f", "lavfi", "-i", "sine=duration=20",
			"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc",
			"-c:a", "ac3", "-output_ts_offset", "100"}, 25, true, 1280, 720, 4},
		{"noaudio.mp4", []string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=14", "-c:v", "libx264", "-preset", "ultrafast"}, 30, false, 640, 360, 3},
		{"short.mkv", []string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=2.5", "-f", "lavfi", "-i", "sine=duration=2.5",
			"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac"}, 25, true, 320, 240, 1},
		{"odd.mkv", []string{"-f", "lavfi", "-i", "testsrc2=size=641x359:rate=25:duration=12.5", "-f", "lavfi", "-i", "sine=duration=12.5",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv444p", "-c:a", "mp3"}, 25, true, 640, 358, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := fixture(t, "edge2-"+c.name, c.args...)
			se := createSession(t, env, 1, src, "720p", 0)
			if se.NSeg != c.nseg {
				t.Fatalf("NSeg %d want %d (duration %.3f)", se.NSeg, c.nseg, se.Duration)
			}
			out := t.TempDir()
			segs := make([]string, se.NSeg)
			// Last segment first (seek), then the rest.
			segs[se.NSeg-1], _ = fetch(t, env, se, 1, se.NSeg-1, out)
			for i := 0; i < se.NSeg-1; i++ {
				segs[i], _ = fetch(t, env, se, 1, i, out)
			}
			// Every segment starts with a keyframe at its boundary, whichever run made it.
			base := inspectSegment(t, segs[0]).videoStart
			for i, sp := range segs {
				x := inspectSegment(t, sp)
				want := base + math.Ceil(float64(i*se.SegDur)*c.fps-1e-6)/c.fps
				if !x.firstKey || math.Abs(x.videoStart-want) > 0.0015 {
					t.Errorf("segment %d: key=%v start %.4f want %.4f", i, x.firstKey, x.videoStart, want)
				}
			}
			si := inspectSegment(t, segs[0])
			if si.width != c.w || si.height != c.h || si.pix != "yuv420p" || si.profile != "High" {
				t.Fatalf("%+v", si)
			}
			if (si.acodec != "") != c.hasAudio {
				t.Fatalf("audio %q", si.acodec)
			}
			checkContinuous(t, segs, se.Duration, c.fps, c.hasAudio)
			// Nothing beyond the announced segments was produced.
			env.svc.StopSession(se.ID, 1)
			if exists(se.segPath(se.NSeg)) {
				t.Fatal("ffmpeg produced a segment beyond the playlist")
			}
		})
	}
}

// TestHLSPlayableOverHTTP serves the session like the API does and lets
// ffprobe/ffmpeg play the playlist URL (the same HLS demuxing path hls.js uses:
// MPEG-TS segments with H.264 High + AAC-LC stereo).
func TestHLSPlayableOverHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	env := hlsEnv(t, hlsSettings{})
	src := movieMKV(t)
	se := createSession(t, env, 1, src, "720p", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if name == "index.m3u8" {
			pl, err := env.svc.Playlist(se.ID, 1, r.URL.RawQuery)
			if err != nil {
				http.Error(w, err.Error(), 404)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(pl))
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".ts"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		p, err := env.svc.Segment(r.Context(), se.ID, 1, n)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		http.ServeFile(w, r, p)
	}))
	defer srv.Close()
	u := srv.URL + "/hls/" + se.ID + "/index.m3u8?access_token=x"

	r := ffprobeJSON(t, u)
	v, a := r.stream("video"), r.stream("audio")
	if r.Format.FormatName != "hls" || v == nil || a == nil || v.CodecName != "h264" || v.Profile != "High" ||
		a.CodecName != "aac" || a.Profile != "LC" || a.Channels != 2 || math.Abs(f64(r.Format.Duration)-se.Duration) > 0.5 {
		t.Fatalf("ffprobe over HTTP: %+v %+v %+v", r.Format, v, a)
	}
	// Play the whole thing through the HLS demuxer, with a seek into the middle.
	start := time.Now()
	if out, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", u, "-f", "null", "-").CombinedOutput(); err != nil || len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("full playback: %v\n%s", err, out)
	}
	t.Logf("full HLS playback via HTTP took %v", time.Since(start))
	if out, err := exec.Command("ffmpeg", "-v", "error", "-ss", "40", "-i", u, "-t", "8", "-f", "null", "-").CombinedOutput(); err != nil || len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("seek playback: %v\n%s", err, out)
	}
}

func TestHLSCacheEviction(t *testing.T) {
	env := hlsEnv(t, hlsSettings{ttlHours: 1})
	src := fixture(t, "evict.mp4", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=13",
		"-c:v", "libx264", "-preset", "ultrafast")
	active := createSession(t, env, 1, src, "720p", 0)
	idle := createSession(t, env, 1, src, "480p", 0)
	for _, se := range []*hlsSession{active, idle} {
		if _, err := env.svc.Segment(context.Background(), se.ID, 1, 0); err != nil {
			t.Fatal(err)
		}
		env.svc.StopSession(se.ID, 1)
	}
	// Age both the in-memory access time and the session directory: the cache
	// tracks "last used" through the directory mtime.
	old := time.Now().Add(-2 * time.Hour)
	idle.mu.Lock()
	idle.lastAccess = old
	idle.mu.Unlock()
	if err := os.Chtimes(idle.Dir, old, old); err != nil {
		t.Fatal(err)
	}

	env.cache.Cleanup(nil) // expired sessions that are not in use
	if exists(idle.Dir) || !exists(active.segPath(0)) {
		t.Fatalf("idle dir exists=%v, active segment exists=%v", exists(idle.Dir), exists(active.segPath(0)))
	}
	if _, err := env.svc.Playlist(idle.ID, 1, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("evicted session must be forgotten")
	}
	if _, err := env.svc.Playlist(active.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	// A running job keeps its session even when it was not accessed recently.
	if _, err := env.svc.Segment(context.Background(), active.ID, 1, 1); err != nil {
		t.Fatal(err)
	}
	active.mu.Lock()
	active.lastAccess = old
	active.mu.Unlock()
	if err := os.Chtimes(active.Dir, old, old); err != nil {
		t.Fatal(err)
	}
	running := env.svc.Sessions()[0].Running
	env.cache.Cleanup(nil)
	if running && !exists(active.Dir) {
		t.Fatal("session with a running ffmpeg was evicted")
	}
}

func TestEncoderForCodec(t *testing.T) {
	cases := []struct{ configured, codec, want string }{
		{"libx264", CodecH264, "libx264"},
		{"", CodecH264, "libx264"},
		{"h264_nvenc", CodecH264, "h264_nvenc"},
		{"libx264", CodecH265, "libx265"},
		{"h264_videotoolbox", CodecH265, "hevc_videotoolbox"},
		{"h264_nvenc", CodecH265, "hevc_nvenc"},
		{"h264_qsv", CodecH265, "hevc_qsv"},
		{"h264_vaapi", CodecH265, "hevc_vaapi"},
	}
	for _, c := range cases {
		if got := encoderForCodec(c.configured, c.codec); got != c.want {
			t.Errorf("encoderForCodec(%q, %q) = %q, want %q", c.configured, c.codec, got, c.want)
		}
	}
	if softwareEncoder(CodecH265) != "libx265" || softwareEncoder(CodecH264) != "libx264" {
		t.Fatal("software encoders")
	}
}

func TestHLSCodecAndBitrate(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	env := hlsEnv(t, hlsSettings{})
	src := movieMKV(t)
	out := t.TempDir()
	for _, d := range []string{"low", "high", "hevc"} {
		os.MkdirAll(filepath.Join(out, d), 0o755)
	}
	ctx := context.Background()
	open := func(o TranscodeOptions) (*hlsSession, error) {
		return env.svc.CreateSession(ctx, 1, "u1", src, "/v/"+filepath.Base(src), stat(t, src), o)
	}

	if _, err := open(TranscodeOptions{Quality: "480p", Codec: "vp9"}); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("unknown codec: %v", err)
	}
	for _, br := range []int{-1, MinBitrate - 1, MaxBitrate + 1} {
		if _, err := open(TranscodeOptions{Quality: "480p", Bitrate: br}); !errors.Is(err, ErrInvalidBitrate) {
			t.Fatalf("bitrate %d: %v", br, err)
		}
	}

	low, err := open(TranscodeOptions{Quality: "480p", Bitrate: 300})
	if err != nil {
		t.Fatal(err)
	}
	high, err := open(TranscodeOptions{Quality: "480p", Bitrate: 6000})
	if err != nil {
		t.Fatal(err)
	}
	auto := createSession(t, env, 1, src, "480p", 0)
	if low.ID == high.ID || low.ID == auto.ID {
		t.Fatal("bitrate must be part of the session id")
	}
	// Without a codec the session uses the server's default: HEVC when available.
	if def, err := open(TranscodeOptions{Quality: "480p"}); err != nil || def.Codec != env.svc.DefaultCodec() {
		t.Fatalf("default codec: %v %+v", err, def)
	}
	wantDefault := CodecH264
	if slices.Contains(env.svc.Codecs(), CodecH265) {
		wantDefault = CodecH265
	}
	if env.svc.DefaultCodec() != wantDefault {
		t.Fatalf("DefaultCodec = %s, want %s", env.svc.DefaultCodec(), wantDefault)
	}
	pl, _ := fetch(t, env, low, 1, 1, filepath.Join(out, "low"))
	ph, _ := fetch(t, env, high, 1, 1, filepath.Join(out, "high"))
	sl, sh := fileSize(t, pl), fileSize(t, ph)
	if sl*2 > sh {
		t.Fatalf("300k segment (%d B) should be much smaller than 6000k (%d B)", sl, sh)
	}

	if !slices.Contains(env.svc.Codecs(), CodecH265) {
		t.Log("ffmpeg has no HEVC encoder; skipping the h265 part")
		return
	}
	hevc, err := open(TranscodeOptions{Quality: "480p", Codec: CodecH265})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh run and a run restarted by seeking both start with a keyframe.
	for _, n := range []int{0, 3} {
		p, _ := fetch(t, env, hevc, 1, n, filepath.Join(out, "hevc"))
		si := inspectSegment(t, p)
		if si.vcodec != "hevc" || !si.firstKey || si.height != 480 {
			t.Fatalf("hevc segment %d: %+v", n, si)
		}
	}
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
