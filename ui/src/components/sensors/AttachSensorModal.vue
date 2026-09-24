<script setup lang="ts">
// Which identities publish one sensor, and what each of them sends. The one dialog for attaching,
// from the Sensors page and from an identity's drawer.
//
// Saving restarts the nodes that changed — meshtasticd only looks for sensors when it starts — so
// the dialog says which ones will bounce before anyone presses the button.
import { computed, ref, watch } from 'vue'
import { api, enc } from '@/api/client'
import type { Identity, Sensor, SensorAttachment, SensorFieldInfo } from '@/api/types'
import Modal from '@/components/ui/Modal.vue'
import Spinner from '@/components/ui/Spinner.vue'
import CheckDropdown from '@/components/ui/CheckDropdown.vue'
import { live } from '@/store/live'
import { toast } from '@/composables/toast'

const props = defineProps<{ sensor: Sensor | null; fields?: SensorFieldInfo[] }>()
const emit = defineEmits<{ close: []; saved: [] }>()

/** One identity's row: everything it publishes, so this sensor can be added to it or taken out. */
interface Row {
  identity: Identity
  had: SensorAttachment[]
  on: boolean
  fields: string[]
}

const rows = ref<Row[]>([])
const loading = ref(false)
const busy = ref(false)
const error = ref('')

// What this sensor can offer: the fields it has reported, or every publishable field until it has
// been read. A field no chip can carry can't be offered at all.
const choices = computed(() => {
  const carried = new Set((props.fields ?? []).filter((f) => f.chip).map((f) => f.field))
  const reported = Object.keys(props.sensor?.last?.fields ?? {})
  const list = reported.length ? reported.filter((f) => carried.has(f)) : [...carried]
  return list.sort().map((f) => ({ value: f, label: `${f} (${props.fields?.find((x) => x.field === f)?.unit ?? ''})`.replace(' ()', '') }))
})
// Read but unpublishable (pressure, today): say so rather than silently dropping it.
const dropped = computed(() => {
  const carried = new Set((props.fields ?? []).filter((f) => f.chip).map((f) => f.field))
  return Object.keys(props.sensor?.last?.fields ?? {}).filter((f) => !carried.has(f))
})

async function load() {
  const sn = props.sensor
  if (!sn) return
  loading.value = true
  error.value = ''
  try {
    const lists = await Promise.all(
      live.identities.map((i) => api.get<SensorAttachment[]>(`/identities/${enc(i.node_id)}/sensors`).catch(() => [] as SensorAttachment[])),
    )
    rows.value = live.identities.map((identity, at) => {
      const had = lists[at] ?? []
      const mine = had.find((a) => a.sensor === sn.id)
      return { identity, had, on: !!mine, fields: mine?.fields ?? [] }
    })
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
watch(() => props.sensor, load, { immediate: true })

const same = (a: string[], b: string[]) => a.length === b.length && [...a].sort().join() === [...b].sort().join()

/** The rows whose node has to restart: what it publishes is changing. */
const changed = computed(() =>
  rows.value.filter((r) => {
    const mine = r.had.find((a) => a.sensor === props.sensor?.id)
    if (!!mine !== r.on) return true
    return !!mine && r.on && !same(mine.fields ?? [], r.fields)
  }),
)

async function save() {
  const sn = props.sensor
  if (!sn) return
  busy.value = true
  error.value = ''
  try {
    // One at a time: each PUT bounces that identity's node, and nothing else on the host.
    for (const r of changed.value) {
      const rest = r.had.filter((a) => a.sensor !== sn.id)
      const body = r.on ? [...rest, { sensor: sn.id, fields: r.fields }] : rest
      r.had = await api.put<SensorAttachment[]>(`/identities/${enc(r.identity.node_id)}/sensors`, body)
    }
    toast(changed.value.length ? `${sn.name} saved · ${changed.value.length} node(s) restarting` : `${sn.name} unchanged`)
    emit('saved')
  } catch (e) {
    error.value = (e as Error).message
    await load()
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Modal
    :open="!!sensor"
    :title="`Publish ${sensor?.name ?? ''}`"
    subtitle="Tick the identities that should publish this sensor as their own, and what each one sends."
    size="lg"
    @close="emit('close')"
  >
    <div v-if="loading" class="empty">Loading what each identity publishes…</div>
    <template v-else>
      <div v-for="r in rows" :key="r.identity.node_id" class="flex flex-wrap items-center gap-3 border-b border-line-soft py-2.5 last:border-b-0">
        <label class="flex min-w-48 flex-1 cursor-pointer items-center gap-3">
          <input v-model="r.on" type="checkbox" class="size-4 accent-[var(--brand)]" />
          <span class="min-w-0">
            <span class="block truncate text-[13px] font-medium">{{ r.identity.long_name }}</span>
            <span class="mono block truncate text-2xs text-ink-3">
              {{ r.identity.node_id }}<template v-if="r.identity.is_relay"> · relay persona</template>
            </span>
          </span>
        </label>
        <div class="w-56 max-sm:w-full">
          <CheckDropdown v-model="r.fields" :options="choices" :disabled="!r.on" empty-label="Everything it reports" />
        </div>
      </div>

      <p v-if="!rows.length" class="empty">No identities yet. Add one on the Identities page first.</p>
      <p v-if="dropped.length" class="mt-3 rounded-xl bg-warn/10 px-3 py-2.5 text-xs text-warn">
        {{ dropped.join(', ') }} can't be published: none of the chips RepeaterTastic imitates carries it, so no node could report it.
      </p>

      <div :class="['mt-4 rounded-xl px-3.5 py-3 text-[13px]', changed.length ? 'bg-warn/10 text-warn' : 'bg-raised text-ink-3']">
        <template v-if="changed.length">
          Saving restarts {{ changed.length === 1 ? 'this node' : 'these nodes' }}:
          <strong>{{ changed.map((r) => r.identity.long_name).join(', ') }}</strong>. They only look for sensors when they start.
          Each keeps its key, its settings and its app port, and nothing else on the host is interrupted.
        </template>
        <template v-else>Nothing has changed yet, so no node needs to restart.</template>
      </div>
      <p v-if="error" class="mt-3 text-[13px] text-bad">{{ error }}</p>
    </template>

    <template #footer>
      <button type="button" class="btn" @click="emit('close')">Cancel</button>
      <button type="button" class="btn btn-primary" :disabled="busy || loading || !changed.length" @click="save">
        <Spinner v-if="busy" />{{ changed.length ? `Save and restart ${changed.length} node${changed.length === 1 ? '' : 's'}` : 'Save' }}
      </button>
    </template>
  </Modal>
</template>
