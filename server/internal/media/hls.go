package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sakana-1314/MList/server/internal/cache"
)

// Real-time HLS transcoding.
//
// A session publishes a complete VOD playlist up front (segment count derived
// from the duration) so the player can seek anywhere. Segments are produced on
// demand by ffmpeg "jobs": a request for a segment that is neither on disk nor
// about to be produced by a running job starts a job at that segment's
// timestamp (input seek). Keyframes are forced on exact segment boundaries and
// every run uses the same timestamp base, so segments produced by different
// runs line up and can be mixed freely.
//
// Every job writes into its own working directory (ffmpeg's segment muxer);
// segments that ffmpeg has finished (listed in the job's segment list) are
// promoted into the session directory with an atomic rename. A segment file in
// the session directory is therefore always complete, and two jobs of one
// session never write the same file.
//
// Jobs are owned by the user whose request started them. A user seeking
// replaces their own job; another user watching the same file (same session)
// gets a separate job. The global job count is capped by
// Settings.Transcode.MaxSessions: when the cap is reached the least recently
// used job without waiting requests is stopped.
//
// Finished segments stay in the transcode cache and are subject to its TTL/LRU
// lifecycle.

var (
	ErrSessionNotFound = errors.New("转码会话不存在或已过期")
	ErrNoVideo         = errors.New("文件不包含视频流")
	// ErrUnsupportedCodec: the codec is unknown or the server has no encoder for it.
	ErrUnsupportedCodec = errors.New("服务器不支持该视频编码")
	ErrInvalidBitrate   = errors.New("码率超出范围")
)

var Qualities = map[string]struct {
	Height  int
	Bitrate int // kbit/s
}{
	"original": {0, 8000},
	"1080p":    {1080, 6000},
	"720p":     {720, 3000},
	"480p":     {480, 1200},
}

// Output codecs. Only codecs that browsers can play from MPEG-TS segments
// (directly or through hls.js) are offered: H.264 everywhere, HEVC where the
// browser decodes it.
const (
	CodecH264 = "h264"
	CodecH265 = "h265"
)

// Bitrate bounds (kbit/s) accepted for an explicitly requested bitrate.
const (
	MinBitrate = 200
	MaxBitrate = 80000
)

// TranscodeOptions selects what a transcode session produces.
type TranscodeOptions struct {
	Quality string // key of Qualities; unknown values fall back to 720p
	Bitrate int    // target video bitrate in kbit/s; 0 picks one from the quality
	Codec   string // CodecH264 or CodecH265; empty picks DefaultCodec
	Audio   int    // audio track index
}

// softwareEncoder is the always-available fallback encoder for a codec.
func softwareEncoder(codec string) string {
	if codec == CodecH265 {
		return "libx265"
	}
	return "libx264"
}

// encoderForCodec maps the configured (H.264) encoder to the one producing codec.
func encoderForCodec(configured, codec string) string {
	if codec != CodecH265 {
		if configured == "" {
			return "libx264"
		}
		return configured
	}
	switch configured {
	case "h264_videotoolbox", "h264_nvenc", "h264_qsv", "h264_vaapi":
		return "hevc_" + strings.TrimPrefix(configured, "h264_")
	}
	return "libx265"
}

const (
	sessionActive  = 3 * time.Minute  // sessions accessed this recently are never evicted
	segmentWait    = 90 * time.Second // max time a segment request waits for ffmpeg
	maxLookahead   = 8                // a running job never "covers" segments further ahead than this
	pollInterval   = 100 * time.Millisecond
	encoderBackoff = 10 * time.Minute // a failed hardware encoder is not retried for this long
	// tsBase is added to every output timestamp. It keeps the first DTS of a
	// run positive (B-frame reordering delay, AAC priming, negative source
	// timestamps) so the muxer never shifts a run's timestamps, which would
	// misalign runs started at 0 vs. n.
	tsBase = 10.0
	// minTail: a trailing remainder shorter than this is merged into the last segment.
	minTail = 1.0
)

// Tunables (variables so tests can shorten them; read once in newHLSManager).
var (
	jobIdleTimeout  = 45 * time.Second // kill ffmpeg when no request needed it for this long
	janitorInterval = 10 * time.Second
	// restartCost is the estimated extra latency of restarting ffmpeg at a
	// segment (process start, input seek, encoder warm-up) compared to a job
	// that is already running. See hlsJob.covers.
	restartCost = time.Second
)

// Lock order: jobMu -> mu -> hlsSession.mu. jobMu guards every hlsSession.jobs
// and all mutable hlsJob fields.
type hlsManager struct {
	svc      *Service
	jobMu    sync.Mutex
	mu       sync.Mutex
	sessions map[string]*hlsSession
	stop     chan struct{}
	stopped  sync.WaitGroup
	closed   bool // guarded by jobMu
	seq      int  // guarded by jobMu; job directory counter

	idleTimeout time.Duration

	encMu        sync.Mutex
	brokenEncDue map[string]time.Time // encoder -> retry time
}

type hlsSession struct {
	ID       string
	Real     string
	VPath    string
	Quality  string
	Bitrate  int    // requested video bitrate (kbit/s); 0 = derived from Quality
	Codec    string // output codec
	Audio    int
	Duration float64
	SegDur   int
	NSeg     int
	Dir      string
	info     *MediaInfo
	encoder  string // configured encoder when the session was created

	jobs []*hlsJob // guarded by hlsManager.jobMu

	mu         sync.Mutex
	users      map[uint]string
	lastAccess time.Time
}

type hlsJob struct {
	se      *hlsSession
	owner   uint
	start   int
	next    int // first segment index of this run not yet promoted
	dir     string
	cmd     *exec.Cmd
	done    chan struct{} // closed when ffmpeg exited; err is valid afterwards
	stderr  *bytes.Buffer
	err     error
	encoder string
	started time.Time

	// guarded by hlsManager.jobMu
	lastUse   time.Time
	waiters   int
	killed    bool
	finalized bool
}

func newHLSManager(s *Service) *hlsManager {
	m := &hlsManager{svc: s, sessions: map[string]*hlsSession{}, stop: make(chan struct{}),
		idleTimeout: jobIdleTimeout, brokenEncDue: map[string]time.Time{}}
	m.stopped.Add(1)
	go m.janitor(janitorInterval)
	return m
}

// SessionInfo 是转码会话信息。
// @Description 转码会话信息
type SessionInfo struct {
	ID         string    `json:"id"`          // 会话 ID
	Path       string    `json:"path"`        // 源视频的虚拟路径
	Quality    string    `json:"quality"`     // 输出画质
	User       string    `json:"user"`        // 正在观看的用户名（多用户时用逗号分隔）
	Running    bool      `json:"running"`     // 当前是否有 ffmpeg 进程在转码
	LastAccess time.Time `json:"last_access"` // 最近一次访问时间
}

// Sessions lists known transcode sessions.
func (s *Service) Sessions() []SessionInfo {
	m := s.hls
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []SessionInfo{}
	for _, se := range m.sessions {
		se.mu.Lock()
		names := []string{}
		for _, n := range se.users {
			names = append(names, n)
		}
		out = append(out, SessionInfo{ID: se.ID, Path: se.VPath, Quality: se.Quality, User: strings.Join(names, ","),
			Running: se.running(), LastAccess: se.lastAccess})
		se.mu.Unlock()
	}
	return out
}

// segmentCount returns the number of segments for a duration. A short trailing
// remainder is merged into the previous segment (keyframes are not forced at
// that boundary) so the playlist never announces a near-empty segment that
// ffmpeg might not produce.
func segmentCount(duration float64, segDur int) int {
	n := int(math.Ceil(duration/float64(segDur) - 1e-9))
	if n < 1 {
		return 1
	}
	if n > 1 && duration-float64((n-1)*segDur) < minTail {
		n--
	}
	return n
}

// CreateSession registers (or reuses) a transcode session for a user.
func (s *Service) CreateSession(ctx context.Context, userID uint, username, real, vpath string, fi os.FileInfo, opts TranscodeOptions) (*hlsSession, error) {
	if !s.hasFFmpeg {
		return nil, ErrNoFFmpeg
	}
	quality, audio := opts.Quality, opts.Audio
	if _, ok := Qualities[quality]; !ok {
		quality = "720p"
	}
	codec := opts.Codec
	if codec == "" {
		codec = s.DefaultCodec()
	}
	if !slices.Contains(s.Codecs(), codec) {
		return nil, ErrUnsupportedCodec
	}
	bitrate := opts.Bitrate
	if bitrate != 0 && (bitrate < MinBitrate || bitrate > MaxBitrate) {
		return nil, ErrInvalidBitrate
	}
	mi, err := s.Probe(ctx, real, fi.ModTime(), fi.Size())
	if err != nil {
		return nil, err
	}
	if mi.Video == nil || mi.Duration <= 0 {
		return nil, ErrNoVideo
	}
	if audio < 0 || audio >= len(mi.Audio) {
		audio = 0
	}
	fp, err := s.Fingerprint(real, fi.Size(), fi.ModTime())
	if err != nil {
		return nil, err
	}
	ts := s.transcode
	// The encoder is part of the key: segments of different encoders are not mixed.
	id := cacheKey("hls", fp, quality, audio, ts.SegmentSeconds, ts.Encoder, bitrate, codec)[:24]

	m := s.hls
	m.mu.Lock()
	defer m.mu.Unlock()
	se := m.sessions[id]
	if se == nil {
		se = &hlsSession{
			ID: id, Real: real, VPath: vpath, Quality: quality, Bitrate: bitrate, Codec: codec, Audio: audio, Duration: mi.Duration,
			SegDur: ts.SegmentSeconds, Dir: s.cache.TranscodeDir(id), info: mi, encoder: ts.Encoder,
			users: map[uint]string{},
		}
		se.NSeg = segmentCount(mi.Duration, se.SegDur)
		if err := os.MkdirAll(se.Dir, 0o755); err != nil {
			return nil, err
		}
		// Working directories left behind by a previous process are useless.
		if olds, _ := filepath.Glob(filepath.Join(se.Dir, "job-*")); len(olds) > 0 {
			for _, o := range olds {
				os.RemoveAll(o)
			}
		}
		m.sessions[id] = se
	}
	se.mu.Lock()
	se.users[userID] = username
	se.lastAccess = time.Now()
	se.mu.Unlock()
	return se, nil
}

func (m *hlsManager) get(id string, userID uint) (*hlsSession, error) {
	m.mu.Lock()
	se := m.sessions[id]
	m.mu.Unlock()
	if se == nil {
		return nil, ErrSessionNotFound
	}
	se.mu.Lock()
	_, ok := se.users[userID]
	se.mu.Unlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	return se, nil
}

// segDuration is the nominal duration of segment i.
func (se *hlsSession) segDuration(i int) float64 {
	if i == se.NSeg-1 {
		return se.Duration - float64(i*se.SegDur)
	}
	return float64(se.SegDur)
}

// Playlist renders the VOD playlist. query is appended to segment URIs (e.g. access_token).
func (s *Service) Playlist(id string, userID uint, query string) (string, error) {
	se, err := s.hls.get(id, userID)
	if err != nil {
		return "", err
	}
	target := se.SegDur
	if last := int(math.Ceil(se.segDuration(se.NSeg - 1))); last > target {
		target = last
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n", target)
	if query != "" {
		query = "?" + query
	}
	for i := 0; i < se.NSeg; i++ {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%d.ts%s\n", se.segDuration(i), i, query)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String(), nil
}

// StopSession stops ffmpeg for the session (cached segments are kept).
func (s *Service) StopSession(id string, userID uint) error {
	se, err := s.hls.get(id, userID)
	if err != nil {
		return err
	}
	s.hls.jobMu.Lock()
	defer s.hls.jobMu.Unlock()
	for _, j := range se.jobs {
		s.hls.killJob(j)
	}
	return nil
}

func (se *hlsSession) segPath(n int) string { return filepath.Join(se.Dir, strconv.Itoa(n)+".ts") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Segment returns the local path of segment n, transcoding it if necessary.
func (s *Service) Segment(ctx context.Context, id string, userID uint, n int) (string, error) {
	m := s.hls
	se, err := m.get(id, userID)
	if err != nil {
		return "", err
	}
	if n < 0 || n >= se.NSeg {
		return "", ErrSessionNotFound
	}
	se.mu.Lock()
	se.lastAccess = time.Now()
	se.mu.Unlock()
	cache.Touch(se.Dir)

	p := se.segPath(n)
	if exists(p) {
		m.jobMu.Lock()
		m.markUsed(se, n)
		m.jobMu.Unlock()
		return p, nil
	}

	deadline := time.Now().Add(segmentWait)
	var waitingOn *hlsJob // job this request is waiting for (counted in its waiters)
	failures := 0
	defer func() {
		if waitingOn == nil {
			return
		}
		m.jobMu.Lock()
		defer m.jobMu.Unlock()
		waitingOn.waiters--
		// An abandoned request (player seeked away) whose job has been
		// superseded by a newer job of the same user: stop the old job now
		// instead of letting it run until the idle timeout.
		if ctx.Err() != nil && waitingOn.waiters == 0 && !waitingOn.isFinished() {
			for _, o := range se.jobs {
				if o != waitingOn && o.owner == waitingOn.owner && o.started.After(waitingOn.started) && !o.isFinished() {
					m.killJob(waitingOn)
					break
				}
			}
		}
	}()

	for {
		m.jobMu.Lock()
		if m.closed {
			m.jobMu.Unlock()
			return "", ErrSessionNotFound
		}
		m.promoteSession(se)
		if exists(p) {
			m.markUsed(se, n)
			m.jobMu.Unlock()
			return p, nil
		}
		job := se.covering(n)
		if job == nil {
			if waitingOn != nil && waitingOn.isFinished() && !waitingOn.killed {
				// The job we were waiting for ended without producing n.
				failures++
				if failures >= 2 {
					txt := waitingOn.errText()
					m.jobMu.Unlock()
					return "", fmt.Errorf("转码失败: %s", txt)
				}
			}
			job, err = m.startJob(se, userID, n)
			if err != nil {
				m.jobMu.Unlock()
				return "", err
			}
		}
		if job != waitingOn {
			if waitingOn != nil {
				waitingOn.waiters--
			}
			if job != nil {
				job.waiters++
			}
			waitingOn = job
		}
		if job != nil {
			job.lastUse = time.Now()
		}
		m.jobMu.Unlock()

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
		if time.Now().After(deadline) {
			return "", errors.New("转码超时")
		}
	}
}

// markUsed keeps running jobs that are ahead of a viewer at segment n alive. Caller holds jobMu.
func (m *hlsManager) markUsed(se *hlsSession, n int) {
	now := time.Now()
	for _, j := range se.jobs {
		if !j.isFinished() && j.start <= n && n < j.next+maxLookahead+16 {
			j.lastUse = now
		}
	}
}

// covering returns a running job that will produce segment n soon. Caller holds jobMu.
func (se *hlsSession) covering(n int) *hlsJob {
	for _, j := range se.jobs {
		if j.covers(n) {
			return j
		}
	}
	return nil
}

// covers reports whether waiting for j to reach segment n is expected to be
// faster than restarting ffmpeg at n. j.next is the segment in production; a
// restart produces n in about (one segment + restartCost), waiting takes
// about (n-j.next) segments plus what remains of the current one.
func (j *hlsJob) covers(n int) bool {
	if j.isFinished() || n < j.start {
		return false
	}
	ahead := n - j.next
	if ahead <= 0 {
		return true
	}
	if ahead >= maxLookahead {
		return false
	}
	produced := j.next - j.start
	if produced == 0 {
		return ahead <= 1 && restartCost > 0
	}
	perSeg := time.Since(j.started) / time.Duration(produced)
	return time.Duration(ahead)*perSeg < perSeg+restartCost
}

func (se *hlsSession) running() bool {
	for _, j := range se.jobs {
		if !j.isFinished() {
			return true
		}
	}
	return false
}

func (j *hlsJob) isFinished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

func (j *hlsJob) errText() string {
	if j.stderr != nil && j.stderr.Len() > 0 {
		return lastLine(j.stderr.Bytes())
	}
	if j.err != nil {
		return j.err.Error()
	}
	return "ffmpeg 未生成该分片"
}

// listed returns the highest segment index ffmpeg has completed in this run, or -1.
// The segment muxer appends a segment to its list only after the segment file
// was closed, so listed segments are complete. A partially written last line
// lacks the ".ts" suffix and is ignored.
func (j *hlsJob) listed() int {
	f, err := os.Open(filepath.Join(j.dir, "list.txt"))
	if err != nil {
		return -1
	}
	defer f.Close()
	last := -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasSuffix(line, ".ts") {
			continue
		}
		if i, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(line), ".ts")); err == nil && i > last {
			last = i
		}
	}
	return last
}

// promote moves completed segments of j into the session directory. Caller holds jobMu.
func (m *hlsManager) promote(j *hlsJob) {
	last := j.listed()
	var added int64
	for ; j.next <= last; j.next++ {
		src := filepath.Join(j.dir, strconv.Itoa(j.next)+".ts")
		if fi, err := os.Stat(src); err == nil {
			if os.Rename(src, j.se.segPath(j.next)) == nil {
				added += fi.Size()
			}
		}
	}
	if added > 0 {
		m.svc.cache.Added(cache.Transcode, added)
	}
}

func (m *hlsManager) promoteSession(se *hlsSession) {
	for _, j := range se.jobs {
		if !j.finalized {
			m.promote(j)
		}
	}
}

// finalize promotes what the finished job produced and removes its working
// directory. Caller holds jobMu; j must have exited.
func (m *hlsManager) finalize(j *hlsJob) {
	if j.finalized {
		return
	}
	j.finalized = true
	m.promote(j)
	os.RemoveAll(j.dir)
	se := j.se
	for i, o := range se.jobs {
		if o == j {
			se.jobs = append(se.jobs[:i], se.jobs[i+1:]...)
			break
		}
	}
}

// killJob stops j and waits for ffmpeg to exit. Caller holds jobMu.
func (m *hlsManager) killJob(j *hlsJob) {
	if !j.isFinished() {
		j.killed = true
		j.cmd.Process.Kill()
		<-j.done
	}
	m.finalize(j)
}

func (m *hlsManager) runningJobs() []*hlsJob {
	var out []*hlsJob
	for _, se := range m.sessions {
		for _, j := range se.jobs {
			if !j.isFinished() {
				out = append(out, j)
			}
		}
	}
	return out
}

func (m *hlsManager) encoderFor(se *hlsSession) string {
	enc := encoderForCodec(se.encoder, se.Codec)
	sw := softwareEncoder(se.Codec)
	if enc == sw {
		return sw
	}
	m.encMu.Lock()
	defer m.encMu.Unlock()
	if due, ok := m.brokenEncDue[enc]; ok && time.Now().Before(due) {
		return sw
	}
	return enc
}

// startJob starts ffmpeg for se at segment n on behalf of owner. It returns
// (nil, nil) when the concurrency cap is reached and every running job is
// serving a waiting request; the caller retries later. Caller holds jobMu.
func (m *hlsManager) startJob(se *hlsSession, owner uint, n int) (*hlsJob, error) {
	// A user seeking replaces their own job in this session. Jobs still serving
	// a waiting request (another device of the same account, or a request the
	// player is about to abandon) are left alone; see Segment's cleanup.
	for _, j := range append([]*hlsJob(nil), se.jobs...) {
		if j.owner == owner && j.waiters == 0 {
			m.killJob(j)
		}
	}
	ts := m.svc.transcode

	// Enforce the global concurrency limit by stopping the least recently used idle job.
	m.mu.Lock()
	for {
		running := m.runningJobs()
		if len(running) < max(ts.MaxSessions, 1) {
			break
		}
		var victim *hlsJob
		for _, j := range running {
			if j.waiters > 0 {
				continue
			}
			if victim == nil || j.lastUse.Before(victim.lastUse) {
				victim = j
			}
		}
		if victim == nil {
			m.mu.Unlock()
			return nil, nil
		}
		slog.Debug("hls job preempted", "session", victim.se.ID, "segment", victim.start)
		m.killJob(victim)
	}
	m.mu.Unlock()

	m.seq++
	dir := filepath.Join(se.Dir, fmt.Sprintf("job-%d-%d", os.Getpid(), m.seq))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	encoder := m.encoderFor(se)
	args := m.svc.hlsArgs(se, n, encoder, ts.Preset, dir)
	cmd := exec.Command(m.svc.ffmpeg, args...)
	setProcAttr(cmd)
	stderr := &bytes.Buffer{}
	cmd.Stderr = &limitedWriter{buf: stderr, max: 8192}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	now := time.Now()
	job := &hlsJob{se: se, owner: owner, start: n, next: n, dir: dir, cmd: cmd, done: make(chan struct{}),
		stderr: stderr, encoder: encoder, started: now, lastUse: now}
	se.jobs = append(se.jobs, job)
	slog.Debug("hls job start", "session", se.ID, "segment", n, "encoder", encoder)
	go m.wait(job)
	return job, nil
}

func (m *hlsManager) wait(job *hlsJob) {
	err := job.cmd.Wait()
	job.err = err
	close(job.done)
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	killed := job.killed
	m.finalize(job)
	if sw := softwareEncoder(job.se.Codec); err != nil && !killed && job.encoder != sw && job.next == job.start {
		// The hardware encoder failed before producing anything: use the software encoder for a while.
		slog.Warn("hardware encoder failed, falling back to software", "encoder", job.encoder, "fallback", sw, "err", job.errText())
		m.encMu.Lock()
		m.brokenEncDue[job.encoder] = time.Now().Add(encoderBackoff)
		m.encMu.Unlock()
	} else if err != nil && !killed {
		slog.Warn("hls transcode failed", "session", job.se.ID, "segment", job.start, "err", job.errText())
	}
}

// aacFrame is the duration of one AAC frame at the output sample rate.
const aacFrame = 1024.0 / 48000

// audioLead is how much audio before the segment boundary a restarted run
// includes. A run from 0 puts a few AAC frames from just before each video
// keyframe into the following segment (mux interleaving); starting the audio
// slightly early makes joins between runs overlap by a frame or two (players
// drop the duplicates) instead of leaving a gap.
const audioLead = 0.1

// seekPreroll is how far before the target index-less formats are entered.
const seekPreroll = 12.0

func needsPreroll(format string) bool {
	for _, f := range strings.Split(format, ",") {
		switch f {
		case "mpegts", "mpeg", "mpegvideo", "h264", "hevc", "flv":
			return true
		}
	}
	return false
}

// hlsArgs builds the ffmpeg command for a run producing segments n, n+1, ...
// into dir. Runs started at different segments produce interchangeable
// segments:
//   - -copyts keeps source timestamps, so frames carry the same timestamps
//     in every run; video is trimmed exactly at the boundary, audio starts
//     on the same AAC frame grid a run from 0 uses (-ss alone would re-base
//     timestamps on the seek point, -start_at_zero breaks input seeking);
//   - keyframes are forced at absolute boundary times and the segment muxer
//     cuts exactly there (the hls muxer cuts relative to the run's first
//     packet, which drifts by up to a frame).
func (s *Service) hlsArgs(se *hlsSession, n int, encoder, preset, dir string) []string {
	q := Qualities[se.Quality]
	start := float64(n * se.SegDur)
	hasAudio := len(se.info.Audio) > 0
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	if encoder == "h264_vaapi" {
		args = append(args, "-vaapi_device", "/dev/dri/renderD128")
	}
	// Timestamps stay on the source timeline (-copyts); fs maps it to the
	// playlist timeline (0 = container start).
	fs := se.info.startTime
	var filters, afilters []string
	if start > 0 {
		vstart := fs + start
		seek := vstart
		if hasAudio {
			// First audio sample on the AAC frame grid of a run from 0, which
			// starts at the audio stream's first sample.
			as := se.info.Audio[se.Audio].startTime
			if math.IsNaN(as) {
				as = fs
			}
			seek = as + math.Floor((vstart-audioLead-as)/aacFrame)*aacFrame
			if seek < as {
				seek = as
			}
			afilters = append(afilters, "atrim=start="+strconv.FormatFloat(seek, 'f', 6, 64))
		}
		filters = append(filters, "trim=start="+strconv.FormatFloat(vstart, 'f', 6, 64))
		// Input seeking lands before the target; formats without a seek
		// index (MPEG-TS/PS, raw streams) may land after the preceding
		// keyframe, so decode a little more and let trim cut exactly.
		in := seek - fs
		if needsPreroll(se.info.Format) {
			in -= seekPreroll
		}
		if in > 0 {
			args = append(args, "-ss", strconv.FormatFloat(in, 'f', 6, 64))
		}
	}
	args = append(args, "-copyts", "-i", se.Real, "-map", "0:"+strconv.Itoa(se.info.Video.index))
	if hasAudio {
		args = append(args, "-map", "0:a:"+strconv.Itoa(se.Audio))
	}

	if q.Height > 0 && se.info.Video.Height > q.Height {
		filters = append(filters, "scale=-2:"+strconv.Itoa(q.Height))
	} else {
		filters = append(filters, "scale=trunc(iw/2)*2:trunc(ih/2)*2")
	}
	// An explicit bitrate is honoured as is; the quality's default is capped
	// at the source bitrate, re-encoding never needs more than that.
	bitrate := se.Bitrate
	if bitrate == 0 {
		bitrate = q.Bitrate
		if se.info.BitRate > 0 && int(se.info.BitRate/1000) < bitrate {
			bitrate = max(int(se.info.BitRate/1000), 500)
		}
	}
	br := strconv.Itoa(bitrate) + "k"
	buf := strconv.Itoa(bitrate*2) + "k"
	// GOP longer than a segment plus the merged tail: the only keyframes are
	// the forced ones, so segments are cut exactly where the playlist says.
	fps := se.info.Video.FPS
	if fps <= 0 || fps > 240 {
		fps = 30
	}
	gop := strconv.Itoa(int(math.Ceil(fps * float64(se.SegDur) * 2)))
	profile := "high"
	if se.Codec == CodecH265 {
		profile = "main"
	}
	// Software encoders use constant quality capped at the bitrate, unless a
	// bitrate was chosen explicitly: then they target it like hardware ones.
	rate := []string{"-crf", "23", "-maxrate", br, "-bufsize", buf}
	if se.Bitrate > 0 {
		rate = []string{"-b:v", br, "-maxrate", br, "-bufsize", buf}
	}
	switch encoder {
	case "h264_videotoolbox", "hevc_videotoolbox":
		filters = append(filters, "format=yuv420p")
		args = append(args, "-c:v", encoder, "-b:v", br, "-maxrate", br, "-bufsize", buf, "-allow_sw", "1",
			"-profile:v", profile, "-g", gop)
	case "h264_nvenc", "hevc_nvenc":
		filters = append(filters, "format=yuv420p")
		args = append(args, "-c:v", encoder, "-preset", "p4", "-b:v", br, "-maxrate", br, "-bufsize", buf,
			"-profile:v", profile, "-g", gop, "-no-scenecut", "1", "-forced-idr", "1")
	case "h264_qsv", "hevc_qsv":
		filters = append(filters, "format=nv12")
		args = append(args, "-c:v", encoder, "-preset", "veryfast", "-b:v", br, "-maxrate", br, "-bufsize", buf, "-g", gop)
	case "h264_vaapi", "hevc_vaapi":
		filters = append(filters, "format=nv12", "hwupload")
		args = append(args, "-c:v", encoder, "-b:v", br, "-maxrate", br, "-bufsize", buf, "-g", gop)
	case "libx265":
		filters = append(filters, "format=yuv420p")
		if se.Bitrate == 0 {
			rate[1] = "28" // x265's CRF scale: 28 is about x264's 23
		}
		args = append(args, "-c:v", "libx265", "-preset", preset)
		args = append(args, rate...)
		// Closed GOPs and no scene cuts: forced keyframes must be IDR frames
		// so every segment starts decodable.
		args = append(args, "-profile:v", "main", "-g", gop,
			"-x265-params", "log-level=error:open-gop=0:scenecut=0:keyint="+gop+":min-keyint="+gop)
	default:
		filters = append(filters, "format=yuv420p")
		args = append(args, "-c:v", "libx264", "-preset", preset)
		args = append(args, rate...)
		args = append(args, "-profile:v", "high", "-sc_threshold", "0", "-g", gop)
	}
	// Keyframes at every boundary from n on (absolute source times thanks to
	// -copyts), except boundaries beyond the last announced segment (merged
	// tail). Cut times for the segment muxer are relative to the run start.
	var kf, cuts strings.Builder
	for i := n; i < se.NSeg; i++ {
		if i > n {
			kf.WriteByte(',')
		}
		kf.WriteString(strconv.FormatFloat(fs+float64(i*se.SegDur), 'f', 6, 64))
		if i > n {
			if cuts.Len() > 0 {
				cuts.WriteByte(',')
			}
			cuts.WriteString(strconv.Itoa((i - n) * se.SegDur))
		}
	}
	args = append(args, "-vf", strings.Join(filters, ","), "-force_key_frames", kf.String())
	if hasAudio {
		if len(afilters) > 0 {
			args = append(args, "-af", strings.Join(afilters, ","))
		}
		args = append(args, "-c:a", "aac", "-ac", "2", "-ar", "48000", "-b:a", "160k")
	}
	// The muxer measures cut times from the run's first video frame, which is
	// up to one frame after the boundary: accept keyframes slightly early.
	delta := math.Min(math.Max(1.5/fps+0.01, 0.1), float64(se.SegDur)/3)
	args = append(args, "-sn", "-dn", "-map_chapters", "-1", "-max_muxing_queue_size", "2048",
		"-output_ts_offset", strconv.FormatFloat(tsBase-fs, 'f', 6, 64),
		"-f", "segment", "-segment_format", "mpegts", "-segment_start_number", strconv.Itoa(n))
	if cuts.Len() > 0 {
		args = append(args, "-segment_times", cuts.String(), "-segment_time_delta", strconv.FormatFloat(delta, 'f', 3, 64))
	} else {
		args = append(args, "-segment_time", "86400")
	}
	args = append(args,
		// Each segment restarts the MPEG-TS continuity counters; flag that so
		// demuxers don't treat the first packets as corrupt.
		"-segment_format_options", "mpegts_flags=+initial_discontinuity",
		// The list gets an entry only once a segment file is complete.
		"-segment_list", filepath.Join(dir, "list.txt"), "-segment_list_type", "flat",
		filepath.Join(dir, "%d.ts"))
	return args
}

func (m *hlsManager) janitor(every time.Duration) {
	defer m.stopped.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		m.jobMu.Lock()
		m.mu.Lock()
		sessions := make([]*hlsSession, 0, len(m.sessions))
		for _, se := range m.sessions {
			sessions = append(sessions, se)
		}
		m.mu.Unlock()
		for _, se := range sessions {
			m.promoteSession(se)
			for _, j := range append([]*hlsJob(nil), se.jobs...) {
				if !j.isFinished() && j.waiters == 0 && time.Since(j.lastUse) > m.idleTimeout {
					slog.Debug("hls job idle, stopping", "session", se.ID, "segment", j.start)
					m.killJob(j)
				}
			}
		}
		m.jobMu.Unlock()
	}
}

func (m *hlsManager) isActive(id string) bool {
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	m.mu.Lock()
	se := m.sessions[id]
	m.mu.Unlock()
	if se == nil {
		return false
	}
	if se.running() {
		return true
	}
	se.mu.Lock()
	defer se.mu.Unlock()
	return time.Since(se.lastAccess) < sessionActive
}

func (m *hlsManager) forget(id string) {
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	m.mu.Lock()
	se := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if se != nil {
		for _, j := range append([]*hlsJob(nil), se.jobs...) {
			m.killJob(j)
		}
	}
}

func (m *hlsManager) closeAll() {
	m.jobMu.Lock()
	if m.closed {
		m.jobMu.Unlock()
		return
	}
	m.closed = true
	close(m.stop)
	m.mu.Lock()
	for _, se := range m.sessions {
		for _, j := range append([]*hlsJob(nil), se.jobs...) {
			m.killJob(j)
		}
	}
	m.mu.Unlock()
	m.jobMu.Unlock()
	m.stopped.Wait()
}

type limitedWriter struct {
	mu  sync.Mutex
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() < w.max {
		w.buf.Write(p[:min(len(p), w.max-w.buf.Len())])
	}
	return len(p), nil
}
