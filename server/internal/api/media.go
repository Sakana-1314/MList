package api

import (
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Sakana-1314/MList/server/internal/media"
	"github.com/Sakana-1314/MList/server/internal/vfs"
)

// mediaThumb serves a preview by content digest. The URL carries no path: a
// listing hands the client Entry.Thumb plus its signature, and this resolves the
// digest to the current file. Previews are generated on first use.
//
// The signature is the only credential (it never expires), so the route is
// reachable without a session — that is what lets <img> tags work behind a
// reverse proxy without leaking tokens into URLs.
// @Summary     获取预览图
// @Description 按内容摘要返回 JPEG 预览图。
// @Description 只接收摘要 d 与其签名 s，不接收文件路径，也不要求登录，因此可以直接放进 img 标签；签名不过期。
// @Description 摘要取自列表接口条目的 thumb 与 thumb_sig。签名无效返回 403，摘要未知或无法生成返回 404。
// @Tags     媒体
// @Param    d query string true "内容摘要（Entry.thumb）"
// @Param    s query string true "服务端对摘要的签名（Entry.thumb_sig）"
// @Param    size query integer false "预览图边长（像素），取最接近的可用档位" default(256) Enums(128, 256, 512, 1024)
// @Success  200 {string} binary "JPEG 预览图（Content-Type: image/jpeg）"
// @Failure  403 {object} ErrorResponse "签名无效或缺失"
// @Failure  404 {object} ErrorResponse "摘要未知（服务端无记录或文件已变化），或该文件无法生成预览图"
// @Router   /media/thumb [get]
func (a *API) mediaThumb(c *gin.Context) {
	digest, sig := c.Query("d"), c.Query("s")
	if !a.Auth.VerifyThumbSignature(digest, sig) {
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorResponse{Error: "签名无效"})
		return
	}
	real, ok := a.thumbs.Lookup(digest)
	if !ok {
		fail(c, media.ErrNoThumb)
		return
	}
	fi, err := os.Stat(real)
	if err != nil {
		fail(c, media.ErrNoThumb)
		return
	}
	size, _ := strconv.Atoi(c.DefaultQuery("size", "256"))
	p, err := a.Media.Thumbnail(c.Request.Context(), real, fi, size)
	if err != nil {
		fail(c, err)
		return
	}
	c.Header("Cache-Control", "private, max-age=86400")
	c.Header("Content-Type", "image/jpeg")
	http.ServeFile(c.Writer, c.Request, p)
}

// probeError maps an unexpected ffprobe failure (the file is not a parsable
// media file) to 415 instead of a 500 server error.
func probeError(c *gin.Context, err error) error {
	if statusOf(err) == http.StatusInternalServerError && c.Request.Context().Err() == nil {
		return &httpError{http.StatusUnsupportedMediaType, "无法识别的媒体文件"}
	}
	return err
}

// @Summary     获取媒体信息
// @Description 用 ffprobe 探测媒体文件，返回时长、码率、视频/音频/字幕轨道，以及浏览器能否直接播放（direct_play）。
// @Description transcode_codecs 列出服务器可以转码输出的视频编码。
// @Tags     媒体
// @Produce  json
// @Param    path query string true "媒体文件的虚拟路径"
// @Success  200 {object} MediaInfoResponse "探测到的媒体信息（含视频/音频/字幕轨道）与可用转码编码"
// @Failure  401 {object} ErrorResponse "未登录或会话已失效"
// @Failure  404 {object} ErrorResponse "文件不存在"
// @Security bearerAuth
// @Router   /media/info [get]
func (a *API) mediaInfo(c *gin.Context) {
	vp := vfs.CleanPath(c.Query("path"))
	loc, fi, err := a.fs(c).LocalFile(vp)
	if err != nil {
		fail(c, err)
		return
	}
	mi, err := a.Media.Probe(c.Request.Context(), loc.Real, fi.ModTime(), fi.Size())
	if err != nil {
		fail(c, probeError(c, err))
		return
	}
	out := *mi
	out.Subtitles = append([]media.SubtitleStream{}, mi.Subtitles...)
	if mi.Video != nil {
		for _, name := range media.SidecarSubtitles(loc.Real) {
			out.Subtitles = append(out.Subtitles, media.SubtitleStream{
				Index: len(out.Subtitles), Codec: vfs.Ext(name), Title: name, External: path.Join(path.Dir(vp), name),
			})
		}
	}
	c.JSON(http.StatusOK, MediaInfoResponse{MediaInfo: out, TranscodeCodecs: a.Media.Codecs()})
}

// @Summary     创建转码会话
// @Description 为视频创建 HLS 会话，分片按需转码。
// @Description quality 可选 original、1080p、720p、480p；bitrate 为视频码率（kbps，0 为自动）；codec 为输出编码 h264 或 h265（省略时默认 h265，服务器不支持 HEVC 时为 h264）。
// @Description audio_track 为音轨序号。返回会话 ID 与播放列表地址。
// @Tags     媒体
// @Accept   json
// @Produce  json
// @Param    请求体 body TranscodeRequest true "源文件、画质、码率、编码与音轨"
// @Success  200 {object} TranscodeResponse "会话创建成功，返回播放列表地址"
// @Failure  401 {object} ErrorResponse "未登录或会话已失效"
// @Failure  400 {object} ErrorResponse "码率超出范围、服务器不支持该编码或文件不含视频"
// @Failure  404 {object} ErrorResponse "文件不存在"
// @Security bearerAuth
// @Router   /media/transcode [post]
func (a *API) mediaTranscode(c *gin.Context) {
	var req TranscodeRequest
	if !bind(c, &req) {
		return
	}
	vp := vfs.CleanPath(req.Path)
	loc, fi, err := a.fs(c).LocalFile(vp)
	if err != nil {
		fail(c, err)
		return
	}
	u := principal(c).User
	se, err := a.Media.CreateSession(c.Request.Context(), u.ID, u.Username, loc.Real, vp, fi, media.TranscodeOptions{
		Quality: req.Quality, Bitrate: req.Bitrate, Codec: req.Codec, Audio: req.AudioTrack,
	})
	if err != nil {
		fail(c, probeError(c, err))
		return
	}
	c.JSON(http.StatusOK, TranscodeResponse{
		ID: se.ID, Playlist: "/api/v1/media/hls/" + se.ID + "/index.m3u8",
		Duration: se.Duration, Quality: se.Quality, Bitrate: se.Bitrate, Codec: se.Codec,
	})
}

// @Summary     获取转码播放列表
// @Description 返回该会话的 m3u8 播放列表；分片在请求时按需转码。
// @Tags     媒体
// @Param    id path string true "转码会话 ID"
// @Success  200 {string} binary "HLS 播放列表（m3u8，Content-Type: application/vnd.apple.mpegurl）"
// @Failure  401 {object} ErrorResponse "未登录或会话已失效"
// @Failure  404 {object} ErrorResponse "会话不存在或已停止"
// @Security bearerAuth
// @Router   /media/hls/{id}/index.m3u8 [get]
func (a *API) mediaHLSPlaylist(c *gin.Context) {
	id := c.Param("id")
	query := ""
	if t := c.Query("access_token"); t != "" {
		query = "access_token=" + url.QueryEscape(t)
	}
	pl, err := a.Media.Playlist(id, principal(c).User.ID, query)
	if err != nil {
		fail(c, err)
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "application/vnd.apple.mpegurl", []byte(pl))
}

// @Summary     获取转码分片
// @Description 返回 MPEG-TS 分片数据，首次请求时按需转码并缓存。
// @Tags     媒体
// @Param    id path string true "转码会话 ID"
// @Param    file path string true "分片文件名，形如 0.ts"
// @Success  200 {string} binary "MPEG-TS 分片数据（按需转码后返回，Content-Type: video/mp2t）"
// @Failure  401 {object} ErrorResponse "未登录或会话已失效"
// @Failure  404 {object} ErrorResponse "会话不存在、已停止或分片不存在"
// @Security bearerAuth
// @Router   /media/hls/{id}/{file} [get]
func (a *API) mediaHLSSegment(c *gin.Context) {
	id, file := c.Param("id"), c.Param("file")
	n, err := strconv.Atoi(strings.TrimSuffix(file, ".ts"))
	if err != nil || !strings.HasSuffix(file, ".ts") {
		fail(c, media.ErrSessionNotFound)
		return
	}
	p, err := a.Media.Segment(c.Request.Context(), id, principal(c).User.ID, n)
	if err != nil {
		if c.Request.Context().Err() != nil {
			return
		}
		fail(c, err)
		return
	}
	c.Header("Content-Type", "video/mp2t")
	c.Header("Cache-Control", "private, max-age=3600")
	http.ServeFile(c.Writer, c.Request, p)
}

// @Summary     停止转码会话
// @Description 停止该会话并结束其转码进程。
// @Tags     媒体
// @Param    id path string true "转码会话 ID"
// @Success  204 "会话已停止，无响应体"
// @Failure  401 {object} ErrorResponse "未登录或会话已失效"
// @Security bearerAuth
// @Router   /media/hls/{id} [delete]
func (a *API) mediaHLSStop(c *gin.Context) {
	if err := a.Media.StopSession(c.Param("id"), principal(c).User.ID); err != nil {
		fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// @Summary     转码音频为 MP3
// @Description 把浏览器不支持的音频实时转成 MP3 流，用于在线播放，例如 APE、WMA、DSD。
// @Description start 指定起始秒数，便于拖动进度。
// @Tags     媒体
// @Param    path query string true "音频文件的虚拟路径"
// @Param    start query number false "起始秒数，从该位置开始转码（用于拖动进度）"
// @Success  200 {string} binary "MP3 音频流（chunked 传输，Content-Type: audio/mpeg）"
// @Failure  401 {object} ErrorResponse "未登录或会话已失效"
// @Failure  404 {object} ErrorResponse "文件不存在"
// @Security bearerAuth
// @Router   /media/audio [get]
func (a *API) mediaAudio(c *gin.Context) {
	loc, _, err := a.fs(c).LocalFile(c.Query("path"))
	if err != nil {
		fail(c, err)
		return
	}
	if !a.Media.HasFFmpeg() {
		fail(c, media.ErrNoFFmpeg)
		return
	}
	start, err := strconv.ParseFloat(c.DefaultQuery("start", "0"), 64)
	if err != nil || math.IsNaN(start) || math.IsInf(start, 0) || start < 0 {
		fail(c, badRequest("start 无效"))
		return
	}
	c.Header("Content-Type", "audio/mpeg")
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	a.Media.StreamAudio(c.Request.Context(), loc.Real, start, c.Writer)
}

// @Summary     获取字幕
// @Description 把内嵌字幕或同名外挂字幕（.srt、.ass、.vtt）转成 WebVTT。
// @Description 传 index 时提取视频的内嵌字幕，省略时按外挂字幕文件处理。
// @Tags     媒体
// @Param    path query string true "视频路径（用于提取内嵌字幕）或字幕文件路径"
// @Param    index query integer false "内嵌字幕序号（MediaInfo.subtitles 下标）；省略时按外挂字幕处理"
// @Success  200 {string} binary "WebVTT 字幕内容（Content-Type: text/vtt）"
// @Failure  401 {object} ErrorResponse "未登录或会话已失效"
// @Failure  404 {object} ErrorResponse "文件不存在"
// @Security bearerAuth
// @Router   /media/subtitle [get]
func (a *API) mediaSubtitle(c *gin.Context) {
	loc, fi, err := a.fs(c).LocalFile(c.Query("path"))
	if err != nil {
		fail(c, err)
		return
	}
	var p string
	if idx := c.Query("index"); idx != "" && !media.IsSubtitleFile(fi.Name()) {
		n, err := strconv.Atoi(idx)
		if err != nil {
			fail(c, badRequest("index 无效"))
			return
		}
		p, err = a.Media.EmbeddedSubtitle(c.Request.Context(), loc.Real, fi, n)
		if err != nil {
			fail(c, err)
			return
		}
	} else {
		if !media.IsSubtitleFile(fi.Name()) {
			fail(c, badRequest("不是字幕文件"))
			return
		}
		if p, err = a.Media.ExternalSubtitle(c.Request.Context(), loc.Real, fi); err != nil {
			fail(c, err)
			return
		}
	}
	c.Header("Content-Type", "text/vtt; charset=utf-8")
	c.Header("Cache-Control", "private, max-age=3600")
	http.ServeFile(c.Writer, c.Request, p)
}
