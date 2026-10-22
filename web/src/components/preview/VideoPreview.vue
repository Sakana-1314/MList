<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { NAlert, NIcon, NSelect, NSpin, useMessage, type SelectOption } from 'naive-ui'
import { InformationCircleOutline } from '@vicons/ionicons5'
import type HlsType from 'hls.js'
import type { Entry, MediaInfo, Quality } from '@/api/types'
import { fsApi } from '@/api/fs'
import { mediaApi } from '@/api/media'
import { buildUrl, withToken } from '@/api/http'
import { getToken } from '@/api/token'
import { usePrefsStore, type VideoCodec } from '@/stores/prefs'
import { baseName } from '@/utils/path'
import { formatBitrate, formatDuration } from '@/utils/format'

const props = defineProps<{ entry: Entry }>()
const message = useMessage()
const prefs = usePrefsStore()

type Mode = 'direct' | Quality

const videoRef = ref<HTMLVideoElement | null>(null)
const info = ref<MediaInfo | null>(null)
const infoLoading = ref(true)
const mode = ref<Mode>('direct')
const audioTrack = ref(0)
const subtitleIdx = ref(-1)
const loading = ref(false)
const error = ref('')
const showInfo = ref(false)

const session = ref<string | null>(null)
const hls = shallowRef<HlsType | null>(null)
let fallbackTried = false
let disposed = false
let applySeq = 0

const IMAGE_SUBS = new Set(['hdmv_pgs_subtitle', 'pgssub', 'dvd_subtitle', 'dvdsub', 'dvb_subtitle', 'xsub'])

function defaultQuality(m: MediaInfo | null): Quality {
  const h = m?.video?.height ?? 0
  return h > 1000 ? '1080p' : '720p'
}

const qualityOptions = computed<SelectOption[]>(() => {
  const h = info.value?.video?.height ?? 0
  const opts: SelectOption[] = [
    { label: '原画直连', value: 'direct', disabled: audioTrack.value !== 0 },
    { label: '原画（转码）', value: 'original' },
    { label: '1080p', value: '1080p', disabled: h > 0 && h < 1000 },
    { label: '720p', value: '720p', disabled: h > 0 && h < 700 },
    { label: '480p', value: '480p' },
  ]
  return opts
})

// 转码码率（kbps）；0 表示按画质自动选择
const BITRATES = [0, 20000, 12000, 8000, 6000, 4000, 2000, 1000, 500]
const bitrateOptions: SelectOption[] = BITRATES.map((k) => ({
  value: k,
  label: k === 0 ? '码率自动' : k >= 1000 ? `${k / 1000} Mbps` : `${k} kbps`,
}))

// 浏览器能否解码 HEVC：转码分片经 hls.js（MSE）播放，因此以 MSE 的支持为准。
// 只走原生 HLS 的浏览器（如 iOS 早期版本）不支持 MPEG-TS 中的 HEVC。
function browserPlaysHevc(): boolean {
  const w = window as Window & { ManagedMediaSource?: typeof MediaSource }
  const MS = w.ManagedMediaSource ?? window.MediaSource
  try {
    return !!MS?.isTypeSupported?.('video/mp4; codecs="hvc1.1.6.L93.B0"')
  } catch {
    return false
  }
}
const hevcPlayable = browserPlaysHevc()

/** 服务器能编码、浏览器也能解码 HEVC 时才提供 H.265 */
const hevcAvailable = computed(() => hevcPlayable && (info.value?.transcode_codecs ?? []).includes('h265'))

const codecOptions = computed<SelectOption[]>(() => [
  ...(hevcAvailable.value ? [{ value: 'h265', label: 'H.265 / HEVC' }] : []),
  { value: 'h264', label: 'H.264' },
])

/** 实际使用的编码：默认 HEVC；所选编码不可用时回退到可用的默认编码 */
const codec = computed<VideoCodec>(() => {
  const pref = prefs.transcodeCodec
  if (pref === 'h264' || (pref === 'h265' && hevcAvailable.value)) return pref
  return hevcAvailable.value ? 'h265' : 'h264'
})

const transcoding = computed(() => mode.value !== 'direct' || audioTrack.value !== 0)

const audioOptions = computed<SelectOption[]>(() =>
  (info.value?.audio ?? []).map((a, i) => ({
    value: i,
    label: [a.title || a.language || `音轨 ${i + 1}`, a.codec, a.channels ? `${a.channels}ch` : '']
      .filter(Boolean)
      .join(' · '),
  })),
)

const subtitleOptions = computed<SelectOption[]>(() => [
  { value: -1, label: '无字幕' },
  ...(info.value?.subtitles ?? []).map((s, i) => ({
    value: i,
    disabled: !s.external && IMAGE_SUBS.has((s.codec || '').toLowerCase()),
    label: s.external
      ? `外挂 · ${baseName(s.external)}`
      : [s.title || s.language || `字幕 ${i + 1}`, s.codec].filter(Boolean).join(' · ') +
        (IMAGE_SUBS.has((s.codec || '').toLowerCase()) ? '（图形字幕，不支持）' : ''),
  })),
])

const subtitleSrc = computed(() => {
  const i = subtitleIdx.value
  const s = info.value?.subtitles[i]
  if (i < 0 || !s) return ''
  return s.external ? mediaApi.externalSubtitleUrl(s.external) : mediaApi.embeddedSubtitleUrl(props.entry.path, i)
})
const subtitleLabel = computed(() => {
  const o = subtitleOptions.value.find((x) => x.value === subtitleIdx.value)
  return typeof o?.label === 'string' ? o.label : '字幕'
})

function stopSession(id: string | null): void {
  if (id) void mediaApi.stopHls(id)
}

function destroyHls(): void {
  if (hls.value) {
    hls.value.destroy()
    hls.value = null
  }
}

function resetVideo(): void {
  const v = videoRef.value
  if (!v) return
  v.pause()
  v.removeAttribute('src')
  v.load()
}

function seekWhenReady(v: HTMLVideoElement, t: number): void {
  if (t <= 0) return
  const h = () => {
    try {
      v.currentTime = t
    } catch {
      /* ignore */
    }
  }
  v.addEventListener('loadedmetadata', h, { once: true })
}

// 转码会话可能因缓存淘汰或服务重启而失效（404），此时自动重建一次
let sessionRecreates = 0

async function apply(startAt = 0, recreating = false): Promise<void> {
  if (!recreating) sessionRecreates = 0
  const seq = ++applySeq
  const v = videoRef.value
  if (!v) return
  error.value = ''
  destroyHls()
  const oldSession = session.value
  session.value = null
  stopSession(oldSession)
  resetVideo()

  if (mode.value === 'direct' && audioTrack.value === 0) {
    v.src = fsApi.rawUrl(props.entry.path)
    seekWhenReady(v, startAt)
    void v.play().catch(() => {})
    return
  }

  loading.value = true
  try {
    const quality: Quality = mode.value === 'direct' ? 'original' : mode.value
    const r = await mediaApi.transcode({
      path: props.entry.path,
      quality,
      bitrate: prefs.videoBitrate,
      codec: codec.value,
      audio_track: audioTrack.value,
    })
    if (seq !== applySeq || disposed) {
      stopSession(r.id)
      return
    }
    session.value = r.id
    const playlist = buildUrl(r.playlist)
    const { default: Hls } = await import('hls.js')
    if (seq !== applySeq || disposed) return
    if (Hls.isSupported()) {
      const inst = new Hls({
        startPosition: startAt > 0 ? startAt : -1,
        maxBufferLength: 30,
        xhrSetup: (xhr) => {
          const t = getToken()
          if (t) xhr.setRequestHeader('Authorization', `Bearer ${t}`)
        },
      })
      let netRetries = 0
      inst.on(Hls.Events.ERROR, (_evt, data) => {
        if (!data.fatal) return
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR && data.response?.code === 404 && sessionRecreates < 1) {
          sessionRecreates++
          void apply(v.currentTime || startAt, true)
          return
        }
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR && netRetries < 3) {
          netRetries++
          inst.startLoad()
        } else if (data.type === Hls.ErrorTypes.MEDIA_ERROR) {
          inst.recoverMediaError()
        } else {
          error.value = `播放失败：${data.details}`
          destroyHls()
        }
      })
      inst.on(Hls.Events.MANIFEST_PARSED, () => {
        void v.play().catch(() => {})
      })
      // hls.js does not reliably honour startPosition for these on-demand
      // playlists; seek explicitly so switching quality / bitrate / codec
      // continues where playback was.
      seekWhenReady(v, startAt)
      inst.loadSource(playlist)
      inst.attachMedia(v)
      hls.value = inst
    } else if (v.canPlayType('application/vnd.apple.mpegurl')) {
      // Safari 原生 HLS：无法设置请求头，使用 access_token（分片依赖会话 Cookie / 服务端改写）
      v.src = withToken(playlist)
      seekWhenReady(v, startAt)
      void v.play().catch(() => {})
    } else {
      error.value = '当前浏览器不支持 HLS 播放'
    }
  } catch {
    if (seq === applySeq) error.value = '创建转码会话失败'
  } finally {
    if (seq === applySeq) loading.value = false
  }
}

function currentTime(): number {
  return videoRef.value?.currentTime ?? 0
}

function onModeChange(v: Mode): void {
  mode.value = v
  void apply(currentTime())
}

function onBitrateChange(k: number): void {
  prefs.videoBitrate = k
  if (transcoding.value) void apply(currentTime())
}

function onCodecChange(c: VideoCodec): void {
  prefs.transcodeCodec = c
  if (transcoding.value) void apply(currentTime())
}

function onAudioChange(i: number): void {
  audioTrack.value = i
  if (i !== 0 && mode.value === 'direct') mode.value = defaultQuality(info.value)
  void apply(currentTime())
}

function onVideoError(): void {
  const v = videoRef.value
  if (!v || !v.error) return
  if (mode.value === 'direct' && !hls.value) {
    if (!fallbackTried) {
      fallbackTried = true
      message.info('浏览器无法直接播放该视频，已切换为转码播放')
      const t = v.currentTime
      mode.value = defaultQuality(info.value)
      void apply(t)
    } else {
      error.value = '视频无法播放'
    }
  }
}

function onTrackLoad(): void {
  const v = videoRef.value
  if (!v) return
  for (const t of Array.from(v.textTracks)) t.mode = 'showing'
}

// ---------- 手机全屏横屏 ----------
// 手机上进入全屏时把屏幕锁定为横屏（竖屏视频除外），退出全屏时解锁。
// Android Chrome 等支持 Screen Orientation API；iOS 的原生全屏播放器会随设备方向自动旋转，无需处理。
type LockableOrientation = ScreenOrientation & { lock?: (o: 'landscape') => Promise<void> }
const coarsePointer = typeof window !== 'undefined' && window.matchMedia?.('(pointer: coarse)').matches
let orientationLocked = false

function fullscreenElement(): Element | null {
  const d = document as Document & { webkitFullscreenElement?: Element | null }
  return document.fullscreenElement ?? d.webkitFullscreenElement ?? null
}

function isLandscapeVideo(v: HTMLVideoElement): boolean {
  const w = v.videoWidth || info.value?.video?.width || 0
  const h = v.videoHeight || info.value?.video?.height || 0
  return !w || !h || w >= h
}

function unlockOrientation(): void {
  if (!orientationLocked) return
  orientationLocked = false
  try {
    screen.orientation?.unlock()
  } catch {
    /* ignore */
  }
}

async function onFullscreenChange(): Promise<void> {
  const v = videoRef.value
  const el = fullscreenElement()
  if (!v || !el || !(el === v || el.contains(v))) {
    unlockOrientation()
    return
  }
  const o = screen.orientation as LockableOrientation | undefined
  if (!coarsePointer || !o?.lock || !isLandscapeVideo(v)) return
  try {
    await o.lock('landscape')
    orientationLocked = true
  } catch {
    /* 浏览器不支持或不允许锁定方向 */
  }
}

// 页面关闭时尽量停止转码会话
function onPageHide(): void {
  const id = session.value
  if (!id) return
  const t = getToken()
  void fetch(buildUrl(`/media/hls/${encodeURIComponent(id)}`), {
    method: 'DELETE',
    keepalive: true,
    headers: t ? { Authorization: `Bearer ${t}` } : {},
  }).catch(() => {})
}

onMounted(async () => {
  window.addEventListener('pagehide', onPageHide)
  document.addEventListener('fullscreenchange', onFullscreenChange)
  document.addEventListener('webkitfullscreenchange', onFullscreenChange)
  try {
    info.value = await mediaApi.info(props.entry.path, { silent: true })
  } catch {
    info.value = null
  } finally {
    infoLoading.value = false
  }
  if (disposed) return
  mode.value = !info.value || info.value.direct_play ? 'direct' : defaultQuality(info.value)
  // 默认选中第一个文本字幕？保持关闭，避免意外遮挡
  await nextTick()
  void apply(0)
})

onBeforeUnmount(() => {
  disposed = true
  applySeq++
  window.removeEventListener('pagehide', onPageHide)
  document.removeEventListener('fullscreenchange', onFullscreenChange)
  document.removeEventListener('webkitfullscreenchange', onFullscreenChange)
  unlockOrientation()
  destroyHls()
  stopSession(session.value)
  session.value = null
  resetVideo()
})
</script>

<template>
  <div class="video-preview">
    <div class="stage">
      <video
        ref="videoRef"
        class="video"
        controls
        playsinline
        preload="metadata"
        :poster="mediaApi.thumbUrl(entry, 1024) || undefined"
        @error="onVideoError"
      >
        <track
          v-if="subtitleSrc"
          :key="subtitleSrc"
          kind="subtitles"
          :src="subtitleSrc"
          :label="subtitleLabel"
          srclang="und"
          default
          @load="onTrackLoad"
        />
      </video>
      <div v-if="loading || infoLoading" class="overlay"><n-spin /><span>{{ infoLoading ? '读取媒体信息…' : '正在准备转码…' }}</span></div>

      <!-- 媒体信息：半透明浮窗，悬在画面上，不再占下方一行 -->
      <transition name="fade">
        <div v-if="showInfo" class="info-float">
          <template v-if="info">
            <div><span>容器</span>{{ info.format || '-' }}</div>
            <div><span>时长</span>{{ formatDuration(info.duration) }}</div>
            <div><span>码率</span>{{ formatBitrate(info.bit_rate) }}</div>
            <div v-if="info.video">
              <span>视频</span>{{ info.video.codec }} · {{ info.video.width }}×{{ info.video.height }}
              <template v-if="info.video.fps"> · {{ Math.round(info.video.fps * 100) / 100 }} fps</template>
            </div>
            <div v-for="(a, i) in info.audio" :key="'a' + i">
              <span>音轨 {{ i + 1 }}</span>{{ a.codec }}<template v-if="a.channels"> · {{ a.channels }} 声道</template>
              <template v-if="a.language"> · {{ a.language }}</template><template v-if="a.title"> · {{ a.title }}</template>
            </div>
            <div v-if="info.subtitles.length"><span>字幕</span>{{ info.subtitles.length }} 条</div>
            <div><span>直连</span>{{ info.direct_play ? '浏览器可直接播放' : '需要转码' }}</div>
          </template>
          <div v-else>无法获取媒体信息（服务器可能未安装 ffmpeg）</div>
        </div>
      </transition>
    </div>

    <div class="controls">
      <n-select
        class="sel"
        size="small"
        :value="mode"
        :options="qualityOptions"
        :consistent-menu-width="false"
        @update:value="onModeChange"
      />
      <n-select
        v-if="transcoding"
        class="sel"
        size="small"
        :value="prefs.videoBitrate"
        :options="bitrateOptions"
        :consistent-menu-width="false"
        @update:value="onBitrateChange"
      />
      <n-select
        v-if="transcoding && codecOptions.length > 1"
        class="sel"
        size="small"
        :value="codec"
        :options="codecOptions"
        :consistent-menu-width="false"
        @update:value="onCodecChange"
      />
      <n-select
        v-if="audioOptions.length > 1"
        class="sel"
        size="small"
        :value="audioTrack"
        :options="audioOptions"
        :consistent-menu-width="false"
        @update:value="onAudioChange"
      />
      <n-select
        v-if="subtitleOptions.length > 1"
        v-model:value="subtitleIdx"
        class="sel"
        size="small"
        :options="subtitleOptions"
        :consistent-menu-width="false"
      />
      <span class="spacer" />
      <a class="info-toggle" :class="{ on: showInfo }" @click="showInfo = !showInfo">
        <n-icon :component="InformationCircleOutline" /> 媒体信息
      </a>
    </div>

    <n-alert v-if="error" type="error" class="err" :bordered="false">{{ error }}</n-alert>
  </div>
</template>

<style scoped>
.video-preview {
  width: 100%;
  height: 100%;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 0 16px 12px;
  min-height: 0;
}
.stage {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  align-items: center;
  justify-content: center;
}
.video {
  max-width: 100%;
  max-height: 100%;
  width: 100%;
  height: 100%;
  background: #000;
  border-radius: 6px;
  outline: none;
}
.overlay {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: rgba(255, 255, 255, 0.85);
  background: rgba(0, 0, 0, 0.35);
  pointer-events: none;
}
.controls {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.sel {
  width: auto;
  min-width: 120px;
  max-width: 260px;
}
.spacer {
  flex: 1;
}
.info-toggle {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 13px;
  cursor: pointer;
  opacity: 0.8;
}
.info-toggle:hover {
  opacity: 1;
}
.info-toggle.on {
  opacity: 1;
  color: #63e2b7;
}
/* 半透明浮窗：叠在画面左上角，不挤压播放器 */
.info-float {
  position: absolute;
  top: 12px;
  left: 12px;
  z-index: 1;
  max-width: min(380px, calc(100% - 24px));
  max-height: calc(100% - 24px);
  overflow-y: auto;
  padding: 10px 12px;
  border-radius: 8px;
  background: rgba(12, 12, 14, 0.55);
  -webkit-backdrop-filter: blur(10px);
  backdrop-filter: blur(10px);
  border: 1px solid rgba(255, 255, 255, 0.12);
  color: rgba(255, 255, 255, 0.9);
  font-size: 12.5px;
  line-height: 1.6;
  pointer-events: auto;
}
.info-float > div + div {
  margin-top: 2px;
}
.info-float span {
  display: inline-block;
  min-width: 52px;
  margin-right: 8px;
  opacity: 0.6;
}
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.15s;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
.err {
  flex-shrink: 0;
}
</style>
