<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { NCheckbox, NTag, useThemeVars } from 'naive-ui'
import type { Entry } from '@/api/types'
import FileThumb from './FileThumb.vue'
import { formatEntrySize, formatRelative } from '@/utils/format'

const props = defineProps<{
  entries: Entry[]
  selected: string[]
  isRoot?: boolean
}>()

const emit = defineEmits<{
  'update:selected': [keys: string[]]
  open: [entry: Entry]
  'open-new': [entry: Entry]
  select: [entry: Entry, ev: MouseEvent]
  contextmenu: [entry: Entry, ev: MouseEvent]
}>()

const vars = useThemeVars()
const selectedSet = computed(() => new Set(props.selected))

// 大目录分批渲染，滚动到底部时追加
const PAGE = 240
const visibleCount = ref(PAGE)
watch(
  () => props.entries,
  () => (visibleCount.value = PAGE),
)
const visible = computed(() => props.entries.slice(0, visibleCount.value))

const sentinel = ref<HTMLElement | null>(null)
let observer: IntersectionObserver | null = null
watch(sentinel, (el) => {
  observer?.disconnect()
  if (!el) return
  observer = new IntersectionObserver(
    (items) => {
      if (items.some((i) => i.isIntersecting) && visibleCount.value < props.entries.length) {
        visibleCount.value += PAGE
      }
    },
    { rootMargin: '400px' },
  )
  observer.observe(el)
})
onBeforeUnmount(() => observer?.disconnect())

function toggle(e: Entry, checked: boolean): void {
  const set = new Set(props.selected)
  if (checked) set.add(e.path)
  else set.delete(e.path)
  emit('update:selected', Array.from(set))
}

function onClick(e: Entry, ev: MouseEvent): void {
  // Ctrl/Cmd+点击：浏览器新标签页打开；Shift+点击：范围选择
  if (ev.ctrlKey || ev.metaKey) {
    emit('open-new', e)
    return
  }
  if (ev.shiftKey) {
    emit('select', e, ev)
    return
  }
  emit('open', e)
}
</script>

<template>
  <div
    class="grid-wrap"
    :style="{
      '--card-bg': vars.cardColor,
      '--card-border': vars.dividerColor,
      '--card-hover': vars.hoverColor,
      '--primary': vars.primaryColor,
      '--thumb-bg': vars.actionColor,
    }"
  >
    <slot v-if="!entries.length" name="empty" />
    <div v-else class="grid">
      <div
        v-for="e in visible"
        :key="e.path"
        class="card"
        :class="{ selected: selectedSet.has(e.path) }"
        :title="e.name"
        @click="onClick(e, $event)"
        @contextmenu.prevent.stop="emit('contextmenu', e, $event)"
      >
        <div class="cover">
          <file-thumb :entry="e" :mount="isRoot && e.is_dir" />
          <n-checkbox
            class="check"
            :checked="selectedSet.has(e.path)"
            @click.stop
            @update:checked="(v: boolean) => toggle(e, v)"
          />
          <n-tag v-if="isRoot && !e.writable" class="ro" size="tiny" :bordered="false">只读</n-tag>
        </div>
        <div class="meta">
          <div class="name">{{ e.name }}</div>
          <div class="sub">{{ e.is_dir && e.size < 0 ? formatRelative(e.mod_time) : formatEntrySize(e) }}</div>
        </div>
      </div>
    </div>
    <div ref="sentinel" class="sentinel" />
  </div>
</template>

<style scoped>
.grid-wrap {
  height: 100%;
  overflow-y: auto;
  padding: 4px 2px 16px;
}
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  gap: 12px;
}
@media (max-width: 768px) {
  .grid {
    grid-template-columns: repeat(auto-fill, minmax(104px, 1fr));
    gap: 8px;
  }
}
.card {
  position: relative;
  border: 1px solid var(--card-border);
  background: var(--card-bg);
  border-radius: 10px;
  overflow: hidden;
  cursor: pointer;
  user-select: none;
  transition:
    border-color 0.15s,
    box-shadow 0.15s,
    transform 0.15s;
}
.card:hover {
  border-color: var(--primary);
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.08);
}
.card.selected {
  border-color: var(--primary);
  box-shadow: 0 0 0 2px color-mix(in srgb, var(--primary) 35%, transparent);
}
.cover {
  position: relative;
  aspect-ratio: 4 / 3;
  background: var(--thumb-bg);
}
.check {
  position: absolute;
  top: 6px;
  left: 6px;
  opacity: 0;
  transition: opacity 0.15s;
  background: var(--card-bg);
  border-radius: 4px;
  padding: 1px;
}
.card:hover .check,
.card.selected .check {
  opacity: 1;
}
@media (hover: none) {
  .check {
    opacity: 1;
  }
}
.ro {
  position: absolute;
  top: 6px;
  right: 6px;
}
.meta {
  padding: 8px 10px 9px;
}
.name {
  font-size: 13px;
  line-height: 1.35;
  overflow: hidden;
  text-overflow: ellipsis;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  word-break: break-all;
  min-height: 2.7em;
}
.sub {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 2px;
}
.sentinel {
  height: 1px;
}
</style>
