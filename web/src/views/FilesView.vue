<script setup lang="ts">
import { computed, h, nextTick, onBeforeUnmount, onMounted, ref, watch, type Component } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NButtonGroup,
  NDropdown,
  NEmpty,
  NIcon,
  NResult,
  NSpin,
  NTooltip,
  useDialog,
  useMessage,
  useThemeVars,
  type DropdownOption,
} from 'naive-ui'
import {
  AddOutline,
  ArrowDownOutline,
  ArrowUpOutline,
  CloseOutline,
  CloudUploadOutline,
  CopyOutline,
  CreateOutline,
  DownloadOutline,
  FolderOpenOutline,
  FolderOutline,
  GridOutline,
  ListOutline,
  LinkOutline,
  MoveOutline,
  OpenOutline,
  RefreshOutline,
  SwapVerticalOutline,
  TrashOutline,
  CheckmarkDoneOutline,
  SearchOutline,
  ShareSocialOutline,
} from '@vicons/ionicons5'
import type { BatchResult, Entry } from '@/api/types'
import { ApiError, isAbortError } from '@/api/http'
import { fsApi } from '@/api/fs'
import PathBreadcrumb from '@/components/files/PathBreadcrumb.vue'
import FileTable from '@/components/files/FileTable.vue'
import FileGrid from '@/components/files/FileGrid.vue'
import FolderPickerModal from '@/components/files/FolderPickerModal.vue'
import InputModal from '@/components/common/InputModal.vue'
import PreviewOverlay from '@/components/preview/PreviewOverlay.vue'
import ShareDialog from '@/components/share/ShareDialog.vue'
import { sortEntries, type SortKey, type SortState } from '@/components/files/sort'
import { usePrefsStore } from '@/stores/prefs'
import { useUploadsStore } from '@/stores/uploads'
import { useIsMobile } from '@/composables/useIsMobile'
import { useCopy } from '@/composables/useCopy'
import { baseName, filesRoute, isAncestor, joinPath, normalizePath, parentPath, validateName } from '@/utils/path'
import { isPreviewable } from '@/utils/fileKind'
import { formatSize } from '@/utils/format'
import { downloadMany } from '@/utils/download'
import { fromDataTransfer, fromFileList, hasFiles } from '@/utils/dropFiles'

const route = useRoute()
const router = useRouter()
const prefs = usePrefsStore()
const uploads = useUploadsStore()
const message = useMessage()
const dialog = useDialog()
const isMobile = useIsMobile()
const copy = useCopy()
const vars = useThemeVars()

// ---------- 路径与加载 ----------
const currentPath = computed(() => {
  const p = route.params.path
  const segs = Array.isArray(p) ? p : p ? [p] : []
  return normalizePath('/' + segs.join('/'))
})
const isRoot = computed(() => currentPath.value === '/')

const entries = ref<Entry[]>([])
const dirWritable = ref(false)
const loading = ref(false)
const loadError = ref<{ status: number; message: string } | null>(null)
const loadedPath = ref<string | null>(null)
let controller: AbortController | null = null

async function load(opts: { keepSelection?: boolean; silent?: boolean } = {}): Promise<void> {
  controller?.abort()
  const ctl = new AbortController()
  controller = ctl
  const path = currentPath.value
  if (!opts.silent) loading.value = true
  try {
    const r = await fsApi.list(path, { signal: ctl.signal, silent: true })
    if (ctl.signal.aborted) return
    entries.value = r.entries
    dirWritable.value = r.writable
    loadError.value = null
    loadedPath.value = path
    if (opts.keepSelection) {
      const exists = new Set(r.entries.map((e) => e.path))
      selected.value = selected.value.filter((p) => exists.has(p))
    } else {
      selected.value = []
    }
    openPreviewFromQuery()
  } catch (e) {
    if (isAbortError(e)) return
    const err = e instanceof ApiError ? e : new ApiError(0, '加载失败')
    if (err.status === 401) return
    loadError.value = { status: err.status, message: err.message }
    entries.value = []
    dirWritable.value = false
  } finally {
    if (controller === ctl) {
      loading.value = false
      controller = null
    }
  }
}

watch(
  currentPath,
  (p) => {
    selected.value = []
    anchor = null
    document.title = `${baseName(p) || '全部目录'} · MList`
    void load()
  },
  { immediate: false },
)

function navigate(path: string, query?: Record<string, string>): void {
  void router.push(filesRoute(path, query))
}

// 上传完成后刷新当前目录（防抖）
let reloadTimer: ReturnType<typeof setTimeout> | undefined
watch(
  () => uploads.lastCompleted,
  (c) => {
    if (!c) return
    if (c.dir === currentPath.value || isAncestor(currentPath.value, c.dir)) {
      if (reloadTimer) clearTimeout(reloadTimer)
      reloadTimer = setTimeout(() => void load({ keepSelection: true, silent: true }), 600)
    }
  },
)

// ---------- 排序、视图 ----------
const sort = ref<SortState>({ key: 'name', order: 'ascend' })
const sorted = computed(() => sortEntries(entries.value, sort.value))

const sortLabels: Record<SortKey, string> = { name: '名称', size: '大小', mod_time: '修改时间' }
const sortOptions = computed<DropdownOption[]>(() =>
  (Object.keys(sortLabels) as SortKey[]).map((k) => ({
    key: k,
    label: sortLabels[k] + (sort.value.key === k ? (sort.value.order === 'ascend' ? ' ↑' : ' ↓') : ''),
  })),
)
function onSortSelect(k: SortKey): void {
  if (sort.value.key === k) sort.value = { key: k, order: sort.value.order === 'ascend' ? 'descend' : 'ascend' }
  else sort.value = { key: k, order: k === 'name' ? 'ascend' : 'descend' }
}

const stats = computed(() => {
  let dirs = 0
  let files = 0
  let bytes = 0
  for (const e of entries.value) {
    if (e.is_dir) {
      dirs++
      if (e.size > 0) bytes += e.size
    } else {
      files++
      bytes += e.size
    }
  }
  return { dirs, files, bytes }
})

// ---------- 选择 ----------
const selected = ref<string[]>([])
let anchor: string | null = null
const selectedEntries = computed(() => {
  const set = new Set(selected.value)
  return sorted.value.filter((e) => set.has(e.path))
})
const single = computed(() => (selectedEntries.value.length === 1 ? selectedEntries.value[0] : undefined))
const selectionWritable = computed(() => selectedEntries.value.length > 0 && selectedEntries.value.every((e) => e.writable))

function onSelect(entry: Entry, ev: MouseEvent): void {
  const list = sorted.value
  if (ev.shiftKey && anchor) {
    const a = list.findIndex((e) => e.path === anchor)
    const b = list.findIndex((e) => e.path === entry.path)
    if (a >= 0 && b >= 0) {
      selected.value = list.slice(Math.min(a, b), Math.max(a, b) + 1).map((e) => e.path)
      return
    }
  }
  selected.value = [entry.path]
  anchor = entry.path
}

function selectAll(): void {
  selected.value = sorted.value.map((e) => e.path)
}
function clearSelection(): void {
  selected.value = []
}

// ---------- 打开 / 预览 ----------
const previewShow = ref(false)
const previewIndex = ref(0)
const previewItems = ref<Entry[]>([])

function open(entry: Entry): void {
  if (entry.is_dir) {
    navigate(entry.path)
    return
  }
  if (isPreviewable(entry)) {
    const list = sorted.value.filter(isPreviewable)
    previewItems.value = list
    previewIndex.value = Math.max(0, list.findIndex((e) => e.path === entry.path))
  } else {
    previewItems.value = [entry]
    previewIndex.value = 0
  }
  previewShow.value = true
}

/** 在新标签页打开：文件夹进入目录，文件打开预览（复用 ?preview= 路由参数）。 */
function openInNewTab(entry: Entry): void {
  const target = entry.is_dir
    ? filesRoute(entry.path)
    : filesRoute(parentPath(entry.path), { preview: entry.name })
  window.open(router.resolve(target).href, '_blank', 'noopener')
}

function openPreviewFromQuery(): void {
  const name = route.query.preview
  if (typeof name !== 'string' || !name) return
  const entry = entries.value.find((e) => e.name === name)
  if (entry) {
    selected.value = [entry.path]
    anchor = entry.path
    if (!entry.is_dir) open(entry)
  } else {
    message.warning(`未找到文件：${name}`)
  }
  if (!entry || entry.is_dir) clearPreviewQuery()
}

function clearPreviewQuery(): void {
  if (route.query.preview !== undefined) {
    const q = { ...route.query }
    delete q.preview
    void router.replace({ query: q })
  }
}

function onPreviewClosed(): void {
  const cur = previewItems.value[previewIndex.value]
  if (cur) {
    selected.value = [cur.path]
    anchor = cur.path
  }
  clearPreviewQuery()
}

// ---------- 上传 ----------
const fileInput = ref<HTMLInputElement | null>(null)
const dirInput = ref<HTMLInputElement | null>(null)

function pickFiles(): void {
  fileInput.value?.click()
}
function pickFolder(): void {
  dirInput.value?.click()
}
function onInputChange(e: Event): void {
  const input = e.target as HTMLInputElement
  if (input.files?.length) uploads.enqueue(currentPath.value, fromFileList(input.files))
  input.value = ''
}

const dragDepth = ref(0)
const dragging = computed(() => dragDepth.value > 0)
function onDragEnter(e: DragEvent): void {
  if (!hasFiles(e.dataTransfer)) return
  e.preventDefault()
  dragDepth.value++
}
function onDragOver(e: DragEvent): void {
  if (!hasFiles(e.dataTransfer)) return
  e.preventDefault()
  if (e.dataTransfer) e.dataTransfer.dropEffect = dirWritable.value ? 'copy' : 'none'
}
function onDragLeave(e: DragEvent): void {
  if (!hasFiles(e.dataTransfer)) return
  dragDepth.value = Math.max(0, dragDepth.value - 1)
}
async function onDrop(e: DragEvent): Promise<void> {
  if (!hasFiles(e.dataTransfer)) return
  e.preventDefault()
  dragDepth.value = 0
  if (!dirWritable.value) {
    message.warning(isRoot.value ? '请先进入一个桶再上传' : '当前目录为只读，无法上传')
    return
  }
  if (!e.dataTransfer) return
  try {
    const files = await fromDataTransfer(e.dataTransfer)
    if (!files.length) {
      message.info('没有可上传的文件（空文件夹不会被上传）')
      return
    }
    uploads.enqueue(currentPath.value, files)
  } catch {
    message.error('读取拖放的文件失败')
  }
}

// ---------- 新建 / 重命名 ----------
const mkdirOpen = ref(false)
const renameOpen = ref(false)
const renameTarget = ref<Entry | null>(null)

function startMkdir(): void {
  if (!dirWritable.value) return
  mkdirOpen.value = true
}

async function doMkdir(name: string): Promise<void> {
  const entry = await fsApi.mkdir(joinPath(currentPath.value, name))
  message.success('文件夹已创建')
  await load({ silent: true })
  if (entry?.path) selected.value = [entry.path]
}

function startRename(entry?: Entry): void {
  const target = entry ?? single.value
  if (!target || !target.writable) return
  renameTarget.value = target
  renameOpen.value = true
}

async function doRename(name: string): Promise<boolean | void> {
  const t = renameTarget.value
  if (!t) return
  if (name === t.name) return
  const entry = await fsApi.rename(t.path, name)
  message.success('已重命名')
  await load({ silent: true })
  selected.value = [entry?.path ?? joinPath(currentPath.value, name)]
}

// ---------- 复制 / 移动 ----------
const pickerOpen = ref(false)
const pickerMode = ref<'copy' | 'move'>('copy')
const pickerSources = ref<string[]>([])
const pickerExclude = computed(() =>
  selectedEntriesFor(pickerSources.value)
    .filter((e) => e.is_dir)
    .map((e) => e.path),
)

function selectedEntriesFor(paths: string[]): Entry[] {
  const set = new Set(paths)
  return entries.value.filter((e) => set.has(e.path))
}

function startTransfer(mode: 'copy' | 'move', targets?: Entry[]): void {
  const list = targets ?? selectedEntries.value
  if (!list.length) return
  pickerMode.value = mode
  pickerSources.value = list.map((e) => e.path)
  pickerOpen.value = true
}

async function doTransfer(dest: string): Promise<void> {
  const mode = pickerMode.value
  const sources = pickerSources.value.slice()
  if (mode === 'move' && normalizePath(dest) === currentPath.value) {
    message.warning('目标文件夹与当前文件夹相同')
    return
  }
  const run = async (srcs: string[], overwrite: boolean) => {
    const api = mode === 'copy' ? fsApi.copy : fsApi.move
    const loadingMsg = message.loading(mode === 'copy' ? '正在复制…' : '正在移动…', { duration: 0 })
    try {
      const r = await api({ sources: srcs, dest_dir: dest, overwrite })
      handleBatch(r, mode === 'copy' ? '复制' : '移动', overwrite ? undefined : (failed) => run(failed, true))
    } catch {
      /* 错误已提示 */
    } finally {
      loadingMsg.destroy()
      await load({ silent: true })
    }
  }
  await run(sources, false)
}

function handleBatch(r: BatchResult, label: string, retryOverwrite?: (failed: string[]) => Promise<void>): void {
  if (!r.failed.length) {
    message.success(`${label}成功：${r.succeeded.length} 项`)
    return
  }
  const lines = r.failed.slice(0, 8).map((f) => h('li', [h('b', baseName(f.path)), '：', f.error]))
  if (r.failed.length > 8) lines.push(h('li', `…… 共 ${r.failed.length} 项失败`))
  const content = () =>
    h('div', [
      h('p', { style: 'margin:0 0 6px' }, `${label}成功 ${r.succeeded.length} 项，失败 ${r.failed.length} 项：`),
      h('ul', { style: 'margin:0;padding-left:18px;max-height:240px;overflow:auto;word-break:break-all' }, lines),
    ])
  // 仅当失败原因看起来是“目标已存在”时才提供覆盖重试
  const conflict = r.failed.filter((f) => /exist|已存在|冲突|conflict/i.test(f.error))
  if (retryOverwrite && conflict.length) {
    dialog.warning({
      title: `部分文件${label}失败`,
      content,
      positiveText: '覆盖目标并重试',
      negativeText: '关闭',
      onPositiveClick: () => {
        void retryOverwrite(conflict.map((f) => f.path))
      },
    })
  } else {
    dialog.error({ title: `部分文件${label}失败`, content, positiveText: '知道了' })
  }
}

// ---------- 删除 ----------
function startDelete(targets?: Entry[]): void {
  const list = targets ?? selectedEntries.value
  if (!list.length || !list.every((e) => e.writable)) return
  const names = list.slice(0, 6).map((e) => h('li', { style: 'word-break:break-all' }, e.name + (e.is_dir ? '/' : '')))
  if (list.length > 6) names.push(h('li', `…… 等 ${list.length} 项`))
  const hasDir = list.some((e) => e.is_dir)
  dialog.warning({
    title: `删除 ${list.length} 项？`,
    content: () =>
      h('div', [
        h('ul', { style: 'margin:0 0 8px;padding-left:18px' }, names),
        h('div', { style: 'opacity:.75' }, hasDir ? '文件夹将被递归删除，此操作不可恢复。' : '此操作不可恢复。'),
      ]),
    positiveText: '删除',
    negativeText: '取消',
    positiveButtonProps: { type: 'error' },
    onPositiveClick: async () => {
      try {
        const r = await fsApi.remove(list.map((e) => e.path))
        handleBatch(r, '删除')
      } catch {
        /* 错误已提示 */
      }
      await load({ silent: true })
    },
  })
}

// ---------- 下载 ----------
function download(targets?: Entry[]): void {
  const list = targets ?? selectedEntries.value
  const files = list.filter((e) => !e.is_dir)
  if (list.some((e) => e.is_dir)) {
    message.warning('暂不支持直接下载文件夹，可通过 WebDAV / SMB 挂载后复制', { duration: 4000 })
  }
  if (files.length > 10) message.info(`即将依次下载 ${files.length} 个文件，浏览器可能会询问是否允许多个下载`)
  void downloadMany(files.map((e) => ({ path: e.path, name: e.name })))
}

// ---------- 分享 ----------
const shareOpen = ref(false)
const sharePath = ref('')
function startShare(path: string): void {
  sharePath.value = path
  shareOpen.value = true
}

// ---------- 右键菜单 ----------
const ctxShow = ref(false)
const ctxX = ref(0)
const ctxY = ref(0)
const ctxTarget = ref<Entry | null>(null)

const icon = (c: Component) => () => h(NIcon, null, { default: () => h(c) })

const ctxOptions = computed<DropdownOption[]>(() => {
  const t = ctxTarget.value
  if (!t) {
    return [
      { key: 'mkdir', label: '新建文件夹', icon: icon(AddOutline), disabled: !dirWritable.value },
      { key: 'upload', label: '上传文件', icon: icon(CloudUploadOutline), disabled: !dirWritable.value },
      { key: 'upload-dir', label: '上传文件夹', icon: icon(FolderOutline), disabled: !dirWritable.value },
      { type: 'divider', key: 'd0' },
      { key: 'select-all', label: '全选', icon: icon(CheckmarkDoneOutline), disabled: !sorted.value.length },
      { key: 'refresh', label: '刷新', icon: icon(RefreshOutline) },
    ]
  }
  const multi = selectedEntries.value.length > 1
  const targets = multi ? selectedEntries.value : [t]
  const writable = targets.every((e) => e.writable)
  const opts: DropdownOption[] = []
  if (!multi) opts.push({ key: 'open', label: t.is_dir ? '打开' : '预览', icon: icon(t.is_dir ? FolderOpenOutline : OpenOutline) })
  if (targets.some((e) => !e.is_dir)) opts.push({ key: 'download', label: multi ? `下载 ${targets.length} 项` : '下载', icon: icon(DownloadOutline) })
  if (!multi && t.is_dir) {
    opts.push(
      { key: 'search-here', label: '在此文件夹中搜索', icon: icon(SearchOutline) },
      { key: 'share', label: '分享…', icon: icon(ShareSocialOutline) },
    )
  }
  opts.push({ type: 'divider', key: 'd1' })
  if (!isRoot.value) {
    opts.push(
      { key: 'copy', label: '复制到…', icon: icon(CopyOutline) },
      { key: 'move', label: '移动到…', icon: icon(MoveOutline), disabled: !writable },
    )
    if (!multi) opts.push({ key: 'rename', label: '重命名', icon: icon(CreateOutline), disabled: !writable })
  }
  opts.push({ key: 'copy-path', label: multi ? '复制路径' : '复制路径', icon: icon(LinkOutline) })
  if (!isRoot.value) {
    opts.push(
      { type: 'divider', key: 'd2' },
      {
        key: 'delete',
        label: multi ? `删除 ${targets.length} 项` : '删除',
        icon: icon(TrashOutline),
        disabled: !writable,
        props: { style: writable ? 'color:#d03050' : '' },
      },
    )
  }
  return opts
})

function showCtx(entry: Entry | null, ev: MouseEvent): void {
  ctxShow.value = false
  if (entry && !selected.value.includes(entry.path)) {
    selected.value = [entry.path]
    anchor = entry.path
  }
  ctxTarget.value = entry
  void nextTick(() => {
    ctxX.value = ev.clientX
    ctxY.value = ev.clientY
    ctxShow.value = true
  })
}

function onBlankContext(ev: MouseEvent): void {
  const t = ev.target as HTMLElement
  if (t.closest('.card, tr, .n-data-table-tr')) return
  ev.preventDefault()
  showCtx(null, ev)
}

function onCtxSelect(key: string): void {
  ctxShow.value = false
  const t = ctxTarget.value
  const targets = t ? (selectedEntries.value.length > 1 ? selectedEntries.value : [t]) : []
  switch (key) {
    case 'open':
      if (t) open(t)
      break
    case 'download':
      download(targets)
      break
    case 'share':
      if (t) startShare(t.path)
      break
    case 'search-here':
      if (t) void router.push({ name: 'search', query: { path: t.path } })
      break
    case 'copy':
      startTransfer('copy', targets)
      break
    case 'move':
      startTransfer('move', targets)
      break
    case 'rename':
      if (t) startRename(t)
      break
    case 'copy-path':
      void copy(targets.map((e) => e.path).join('\n'), '路径已复制')
      break
    case 'delete':
      startDelete(targets)
      break
    case 'mkdir':
      startMkdir()
      break
    case 'upload':
      pickFiles()
      break
    case 'upload-dir':
      pickFolder()
      break
    case 'select-all':
      selectAll()
      break
    case 'refresh':
      void load({ keepSelection: true })
      break
  }
}

// ---------- 键盘快捷键 ----------
function isTyping(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null
  if (!el) return false
  return el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable
}

function onKeydown(e: KeyboardEvent): void {
  if (previewShow.value || mkdirOpen.value || renameOpen.value || pickerOpen.value) return
  if (document.querySelector('.n-modal-mask, .n-drawer-mask, .n-dialog')) return
  if (isTyping(e.target)) return
  const mod = e.ctrlKey || e.metaKey
  if (mod && e.key.toLowerCase() === 'a') {
    e.preventDefault()
    selectAll()
  } else if (e.key === 'Delete' || (e.key === 'Backspace' && e.metaKey)) {
    if (selectedEntries.value.length && !isRoot.value) {
      e.preventDefault()
      startDelete()
    }
  } else if (e.key === 'F2') {
    if (single.value && !isRoot.value) {
      e.preventDefault()
      startRename()
    }
  } else if (e.key === 'Escape') {
    if (selected.value.length) clearSelection()
  } else if (e.key === 'Enter') {
    if (single.value) {
      e.preventDefault()
      open(single.value)
    }
  } else if (e.key === 'Backspace' && !mod && !isRoot.value) {
    e.preventDefault()
    navigate(currentPath.value.split('/').slice(0, -1).join('/') || '/')
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  document.title = `${baseName(currentPath.value) || '全部目录'} · MList`
  void load()
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  controller?.abort()
  if (reloadTimer) clearTimeout(reloadTimer)
})

function onPreviewSaved(saved: Entry): void {
  if (saved?.path) previewItems.value = previewItems.value.map((e) => (e.path === saved.path ? { ...e, ...saved } : e))
  void load({ keepSelection: true, silent: true })
}

const emptyText = computed(() => {
  if (isRoot.value) return '管理员尚未为你分配任何目录'
  return dirWritable.value ? '文件夹为空，拖放文件到此处即可上传' : '文件夹为空'
})
</script>

<template>
  <div
    class="files-view"
    :style="{ '--brand-primary': vars.primaryColor }"
    @dragenter="onDragEnter"
    @dragover="onDragOver"
    @dragleave="onDragLeave"
    @drop="onDrop"
    @contextmenu="onBlankContext"
  >
    <!-- 导航栏 -->
    <div class="nav-row">
      <path-breadcrumb class="crumbs" :path="currentPath" @navigate="navigate" />
      <div class="nav-tools">
        <n-dropdown v-if="prefs.viewMode === 'grid'" :options="sortOptions" trigger="click" @select="onSortSelect">
          <n-button size="small" quaternary>
            <template #icon><n-icon :component="SwapVerticalOutline" /></template>
            <span class="btn-text">{{ sortLabels[sort.key] }}</span>
            <n-icon :component="sort.order === 'ascend' ? ArrowUpOutline : ArrowDownOutline" style="margin-left: 2px" />
          </n-button>
        </n-dropdown>
        <n-button-group size="small">
          <n-tooltip>
            <template #trigger>
              <n-button :type="prefs.viewMode === 'list' ? 'primary' : 'default'" secondary @click="prefs.viewMode = 'list'">
                <template #icon><n-icon :component="ListOutline" /></template>
              </n-button>
            </template>
            列表视图
          </n-tooltip>
          <n-tooltip>
            <template #trigger>
              <n-button :type="prefs.viewMode === 'grid' ? 'primary' : 'default'" secondary @click="prefs.viewMode = 'grid'">
                <template #icon><n-icon :component="GridOutline" /></template>
              </n-button>
            </template>
            网格视图
          </n-tooltip>
        </n-button-group>
      </div>
    </div>

    <!-- 操作栏 -->
    <div class="action-row">
      <template v-if="!selected.length">
        <n-button size="small" type="primary" :disabled="!dirWritable" @click="pickFiles">
          <template #icon><n-icon :component="CloudUploadOutline" /></template>
          <span class="btn-text">上传文件</span>
        </n-button>
        <n-button size="small" :disabled="!dirWritable" @click="pickFolder">
          <template #icon><n-icon :component="FolderOutline" /></template>
          <span class="btn-text">上传文件夹</span>
        </n-button>
        <n-button size="small" :disabled="!dirWritable" @click="startMkdir">
          <template #icon><n-icon :component="AddOutline" /></template>
          <span class="btn-text">新建文件夹</span>
        </n-button>
        <n-button size="small" quaternary :loading="loading" @click="load({ keepSelection: true })">
          <template #icon><n-icon :component="RefreshOutline" /></template>
          <span class="btn-text">刷新</span>
        </n-button>
        <n-button v-if="!isRoot && loadedPath" size="small" quaternary @click="startShare(currentPath)">
          <template #icon><n-icon :component="ShareSocialOutline" /></template>
          <span class="btn-text">分享此文件夹</span>
        </n-button>
        <span v-if="!dirWritable && loadedPath && !isRoot" class="ro-hint">只读目录</span>
      </template>
      <template v-else>
        <span class="sel-count">已选 {{ selected.length }} 项</span>
        <n-button size="small" :disabled="!selectedEntries.some((e) => !e.is_dir)" @click="download()">
          <template #icon><n-icon :component="DownloadOutline" /></template>
          <span class="btn-text">下载</span>
        </n-button>
        <n-button v-if="single && selectedEntries[0]?.is_dir" size="small" @click="startShare(selectedEntries[0].path)">
          <template #icon><n-icon :component="ShareSocialOutline" /></template>
          <span class="btn-text">分享</span>
        </n-button>
        <template v-if="!isRoot">
          <n-button size="small" @click="startTransfer('copy')">
            <template #icon><n-icon :component="CopyOutline" /></template>
            <span class="btn-text">复制到…</span>
          </n-button>
          <n-button size="small" :disabled="!selectionWritable" @click="startTransfer('move')">
            <template #icon><n-icon :component="MoveOutline" /></template>
            <span class="btn-text">移动到…</span>
          </n-button>
          <n-button size="small" :disabled="!single || !selectionWritable" @click="startRename()">
            <template #icon><n-icon :component="CreateOutline" /></template>
            <span class="btn-text">重命名</span>
          </n-button>
          <n-button size="small" type="error" secondary :disabled="!selectionWritable" @click="startDelete()">
            <template #icon><n-icon :component="TrashOutline" /></template>
            <span class="btn-text">删除</span>
          </n-button>
        </template>
        <n-button size="small" quaternary @click="clearSelection">
          <template #icon><n-icon :component="CloseOutline" /></template>
          <span class="btn-text">取消选择</span>
        </n-button>
      </template>
    </div>

    <!-- 内容 -->
    <div class="content">
      <n-result
        v-if="loadError"
        class="error"
        :status="loadError.status === 403 ? '403' : loadError.status === 404 ? '404' : 'error'"
        :title="loadError.status === 404 ? '目录不存在' : loadError.status === 403 ? '无权访问' : '加载失败'"
        :description="loadError.message"
      >
        <template #footer>
          <n-button @click="navigate('/')">返回全部目录</n-button>
          <n-button type="primary" style="margin-left: 8px" @click="load()">重试</n-button>
        </template>
      </n-result>
      <template v-else>
        <file-table
          v-if="prefs.viewMode === 'list'"
          v-model:selected="selected"
          v-model:sort="sort"
          :entries="sorted"
          :loading="loading"
          :is-root="isRoot"
          :compact="isMobile"
          @open="open"
          @open-new="openInNewTab"
          @select="onSelect"
          @contextmenu="showCtx"
        >
          <template #empty>
            <n-empty :description="emptyText" />
          </template>
        </file-table>
        <n-spin v-else :show="loading" class="grid-spin" content-class="grid-spin-content">
          <file-grid
            v-model:selected="selected"
            :entries="sorted"
            :is-root="isRoot"
            @open="open"
            @open-new="openInNewTab"
            @select="onSelect"
            @contextmenu="showCtx"
          >
            <template #empty>
              <n-empty v-if="!loading" :description="emptyText" class="grid-empty" />
            </template>
          </file-grid>
        </n-spin>
      </template>
    </div>

    <div class="status-bar">
      <span>{{ stats.dirs }} 个文件夹，{{ stats.files }} 个文件<template v-if="stats.bytes">，共 {{ formatSize(stats.bytes) }}</template></span>
      <span class="spacer" />
    </div>

    <!-- 拖放遮罩 -->
    <div v-if="dragging" class="drop-mask" :class="{ disabled: !dirWritable }">
      <div class="drop-box">
        <n-icon :component="CloudUploadOutline" size="48" />
        <div v-if="dirWritable">释放以上传到 <b>{{ currentPath }}</b></div>
        <div v-else>{{ isRoot ? '请先进入一个桶' : '当前目录为只读' }}</div>
      </div>
    </div>

    <input ref="fileInput" type="file" multiple hidden @change="onInputChange" />
    <input ref="dirInput" type="file" webkitdirectory multiple hidden @change="onInputChange" />

    <n-dropdown
      trigger="manual"
      placement="bottom-start"
      :show="ctxShow"
      :x="ctxX"
      :y="ctxY"
      :options="ctxOptions"
      @select="onCtxSelect"
      @clickoutside="ctxShow = false"
    />

    <input-modal
      v-model:show="mkdirOpen"
      title="新建文件夹"
      placeholder="文件夹名称"
      default-value="新建文件夹"
      confirm-text="创建"
      :validate="validateName"
      :on-confirm="doMkdir"
    />
    <input-modal
      v-model:show="renameOpen"
      title="重命名"
      placeholder="新名称"
      :default-value="renameTarget?.name ?? ''"
      :validate="validateName"
      :select-stem="!renameTarget?.is_dir"
      :on-confirm="doRename"
    />
    <folder-picker-modal
      v-model:show="pickerOpen"
      :title="pickerMode === 'copy' ? `复制 ${pickerSources.length} 项到…` : `移动 ${pickerSources.length} 项到…`"
      :confirm-text="pickerMode === 'copy' ? '复制到此处' : '移动到此处'"
      :initial-path="currentPath"
      :exclude="pickerExclude"
      @confirm="doTransfer"
    />
    <share-dialog v-model:show="shareOpen" :path="sharePath" />
    <preview-overlay
      v-model:show="previewShow"
      v-model:index="previewIndex"
      :items="previewItems"
      :writable="dirWritable"
      @saved="onPreviewSaved"
      @closed="onPreviewClosed"
    />
  </div>
</template>

<style scoped>
.files-view {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  padding: 12px 16px 0;
}
.nav-row {
  display: flex;
  align-items: center;
  gap: 12px;
  min-height: 34px;
}
.crumbs {
  flex: 1;
  min-width: 0;
}
.nav-tools {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}
.action-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 0;
  flex-wrap: wrap;
}
.sel-count {
  font-size: 13px;
  font-weight: 600;
  margin-right: 4px;
}
.ro-hint {
  font-size: 12px;
  opacity: 0.55;
}
.content {
  flex: 1;
  min-height: 0;
  position: relative;
}
.error {
  padding-top: 40px;
}
.grid-spin {
  height: 100%;
}
.grid-spin :deep(.grid-spin-content) {
  height: 100%;
}
.grid-empty {
  padding-top: 80px;
}
.status-bar {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  opacity: 0.7;
  padding: 8px 2px 10px;
  white-space: nowrap;
  overflow: hidden;
}
.spacer {
  flex: 1;
}
.muted {
  opacity: 0.75;
}
.keys {
  overflow: hidden;
  text-overflow: ellipsis;
}
.drop-mask {
  position: absolute;
  inset: 8px;
  z-index: 20;
  border: 2px dashed var(--brand-primary);
  border-radius: 12px;
  background: color-mix(in srgb, var(--brand-primary) 8%, transparent);
  backdrop-filter: blur(2px);
  display: flex;
  align-items: center;
  justify-content: center;
  pointer-events: none;
}
.drop-mask.disabled {
  border-color: #d03050;
  background: rgba(208, 48, 80, 0.06);
}
.drop-box {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  font-size: 15px;
  color: var(--brand-primary);
}
.drop-mask.disabled .drop-box {
  color: #d03050;
}
@media (max-width: 768px) {
  .files-view {
    padding: 8px 10px 0;
  }
  .nav-row {
    flex-wrap: wrap;
    gap: 8px;
  }
  .crumbs {
    flex-basis: 100%;
  }
  .nav-tools {
    width: 100%;
  }
  .btn-text {
    display: none;
  }
  .action-row {
    gap: 6px;
    padding: 8px 0;
  }
}
</style>
