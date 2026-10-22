import { defineStore } from 'pinia'
import { ref, watch } from 'vue'

export type ThemePref = 'auto' | 'light' | 'dark'
export type ViewMode = 'list' | 'grid'
export type VideoCodec = 'h264' | 'h265'

const KEY = 'ml_prefs'

interface PrefsData {
  theme: ThemePref
  viewMode: ViewMode
  siderCollapsed: boolean
  /**
   * 用户选择的转码输出编码；null 为默认（浏览器与服务器都支持时用 HEVC，否则 H.264）。
   * 早期版本存的 videoCodec 总是 h264（未必是用户选的），因此换了字段名，不再读取它。
   */
  transcodeCodec: VideoCodec | null
  /** 转码视频码率（kbps），0 为按画质自动 */
  videoBitrate: number
}

function load(): Partial<PrefsData> {
  try {
    const raw = localStorage.getItem(KEY)
    return raw ? (JSON.parse(raw) as Partial<PrefsData>) : {}
  } catch {
    return {}
  }
}

export const usePrefsStore = defineStore('prefs', () => {
  const saved = load()
  const theme = ref<ThemePref>(saved.theme === 'light' || saved.theme === 'dark' ? saved.theme : 'auto')
  const viewMode = ref<ViewMode>(saved.viewMode === 'grid' ? 'grid' : 'list')
  const siderCollapsed = ref<boolean>(!!saved.siderCollapsed)
  const transcodeCodec = ref<VideoCodec | null>(
    saved.transcodeCodec === 'h265' || saved.transcodeCodec === 'h264' ? saved.transcodeCodec : null,
  )
  const videoBitrate = ref<number>(typeof saved.videoBitrate === 'number' && saved.videoBitrate > 0 ? saved.videoBitrate : 0)

  watch([theme, viewMode, siderCollapsed, transcodeCodec, videoBitrate], () => {
    try {
      const data: PrefsData = {
        theme: theme.value,
        viewMode: viewMode.value,
        siderCollapsed: siderCollapsed.value,
        transcodeCodec: transcodeCodec.value,
        videoBitrate: videoBitrate.value,
      }
      localStorage.setItem(KEY, JSON.stringify(data))
    } catch {
      /* ignore */
    }
  })

  function cycleTheme(): void {
    theme.value = theme.value === 'auto' ? 'light' : theme.value === 'light' ? 'dark' : 'auto'
  }

  return { theme, viewMode, siderCollapsed, transcodeCodec, videoBitrate, cycleTheme }
})
