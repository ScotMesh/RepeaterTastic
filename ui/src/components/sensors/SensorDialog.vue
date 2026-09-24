<script setup lang="ts">
// Add or edit one sensor: where the reading comes from, how often, and any scaling. The one dialog
// for both, so there is a single place to change what a sensor can be.
import { computed, ref, watch } from 'vue'
import { api, enc } from '@/api/client'
import type { Sensor, SensorFieldInfo, SensorKind } from '@/api/types'
import Modal from '@/components/ui/Modal.vue'
import Spinner from '@/components/ui/Spinner.vue'
import { toast } from '@/composables/toast'

const props = defineProps<{ open: boolean; sensor?: Sensor | null; fields?: SensorFieldInfo[] }>()
const emit = defineEmits<{ close: []; saved: [sensor: Sensor] }>()

const editing = computed(() => !!props.sensor)
const id = ref('')
const name = ref('')
const kind = ref<SensorKind>('exec')
const command = ref('')
const path = ref('')
const interval = ref('1m')
const scaled = ref<{ field: string; factor: number }[]>([])
const busy = ref(false)
const error = ref('')

// Only fields a node can actually carry are worth scaling: the rest can't be published.
const carried = computed(() => (props.fields ?? []).filter((f) => f.chip))

watch(
  () => props.open,
  (o) => {
    if (!o) return
    const s = props.sensor
    id.value = s?.id ?? ''
    name.value = s?.name ?? ''
    kind.value = s?.kind ?? 'exec'
    command.value = s?.command ?? ''
    path.value = s?.path ?? ''
    // "5m0s" is the same length of time as "5m", and reads better in a box someone has to type in.
    interval.value = (s?.interval ?? '1m').replace(/(\d+h)0m0s$/, '$1').replace(/(\d+m)0s$/, '$1')
    scaled.value = Object.entries(s?.scale ?? {}).map(([field, factor]) => ({ field, factor }))
    error.value = ''
  },
)

// An id names a file in each node's directory, so it has to be safe to put in a path.
const idOk = computed(() => /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(id.value) && !id.value.includes('..'))
const valid = computed(() => {
  if (!idOk.value) return false
  if (kind.value === 'exec') return !!command.value.trim()
  if (kind.value === 'file') return !!path.value.trim()
  return true
})

function addScale() {
  const free = carried.value.find((f) => !scaled.value.some((s) => s.field === f.field))
  if (free) scaled.value.push({ field: free.field, factor: 1 })
}

async function save() {
  busy.value = true
  error.value = ''
  const body: Record<string, unknown> = {
    id: id.value.trim(),
    name: name.value.trim(),
    kind: kind.value,
    command: kind.value === 'exec' ? command.value.trim() : '',
    path: kind.value === 'file' ? path.value.trim() : '',
    interval: kind.value === 'push' ? '' : interval.value.trim(),
    scale: Object.fromEntries(scaled.value.filter((s) => s.factor).map((s) => [s.field, s.factor])),
  }
  try {
    const out = props.sensor
      ? await api.put<Sensor>(`/sensors/${enc(props.sensor.id)}`, body)
      : await api.post<Sensor>('/sensors', body)
    toast(`${out.name} saved`)
    emit('saved', out)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Modal
    :open="open"
    :title="editing ? `Edit ${sensor?.name}` : 'Add sensor'"
    subtitle="A reading this host can take. Identities publish it as their own sensor."
    size="lg"
    @close="emit('close')"
  >
    <div class="grid gap-4 sm:grid-cols-2">
      <div>
        <label class="label" for="sn-name">Name</label>
        <input id="sn-name" v-model="name" class="input" placeholder="Shed" maxlength="60" />
        <p class="hint">What you'll call it here. The node publishes the readings, not the name.</p>
      </div>
      <div>
        <label class="label" for="sn-id">ID</label>
        <input id="sn-id" v-model="id" class="input mono" placeholder="shed" maxlength="40" spellcheck="false" />
        <p :class="['hint', id && !idOk && '!text-bad']">
          Letters, digits, dashes, underscores and dots. It names the sensor in the config file and in each node's folder.
        </p>
      </div>
    </div>

    <div class="mt-4">
      <span class="label">Where the reading comes from</span>
      <div class="tabs" role="tablist">
        <button type="button" role="tab" :aria-selected="kind === 'exec'" @click="kind = 'exec'">Run a command</button>
        <button type="button" role="tab" :aria-selected="kind === 'file'" @click="kind = 'file'">Read a file</button>
        <button type="button" role="tab" :aria-selected="kind === 'push'" @click="kind = 'push'">Pushed to us</button>
      </div>
    </div>

    <div v-if="kind === 'exec'" class="mt-3">
      <label class="label" for="sn-cmd">Command</label>
      <input id="sn-cmd" v-model="command" class="input mono" placeholder="/usr/local/bin/read-shed" spellcheck="false" />
      <p class="hint">Run with <span class="mono">/bin/sh -c</span>. Print one <span class="mono">temperature=21.5</span> per line and exit; it's killed after 10 seconds.</p>
    </div>
    <div v-else-if="kind === 'file'" class="mt-3">
      <label class="label" for="sn-path">File</label>
      <input id="sn-path" v-model="path" class="input mono" placeholder="/run/shed/reading" spellcheck="false" />
      <p class="hint">Something else keeps it up to date. Same format: one <span class="mono">temperature=21.5</span> per line.</p>
    </div>
    <p v-else class="mt-3 rounded-xl bg-raised px-3 py-2.5 text-[13px] text-ink-3">
      A plugin or a script sends readings to <span class="mono">POST /api/v1/sensors/{{ id || 'id' }}/push</span>. Nothing is read
      on a schedule, and a pushed reading counts as current for 15 minutes.
    </p>

    <div v-if="kind !== 'push'" class="mt-4 sm:w-1/2">
      <label class="label" for="sn-iv">Read every</label>
      <input id="sn-iv" v-model="interval" class="input mono" placeholder="1m" spellcheck="false" />
      <p class="hint">A length of time: <span class="mono">30s</span>, <span class="mono">5m</span>, <span class="mono">1h</span>. No more often than every 5 seconds.</p>
    </div>

    <div class="mt-5">
      <div class="flex items-center justify-between gap-2">
        <span class="label !mb-0">Scaling</span>
        <button type="button" class="btn btn-sm" :disabled="scaled.length >= carried.length" @click="addScale">Add a field</button>
      </div>
      <p class="hint !mt-1">For a source that reports in another unit: each reading is multiplied as it arrives (milliamps to amps is 0.001).</p>
      <div v-for="(s, i) in scaled" :key="i" class="mt-2 flex items-center gap-2">
        <select v-model="s.field" class="input" :aria-label="`Field ${i + 1}`">
          <option v-for="f in carried" :key="f.field" :value="f.field">{{ f.field }} ({{ f.unit }})</option>
        </select>
        <input v-model.number="s.factor" type="number" step="any" class="input mono w-32 tabular-nums" :aria-label="`Multiply ${s.field} by`" />
        <button type="button" class="btn btn-sm" @click="scaled.splice(i, 1)">Remove</button>
      </div>
    </div>

    <p v-if="error" class="mt-4 text-[13px] text-bad">{{ error }}</p>
    <template #footer>
      <button type="button" class="btn" @click="emit('close')">Cancel</button>
      <button type="button" class="btn btn-primary" :disabled="busy || !valid" @click="save">
        <Spinner v-if="busy" />{{ editing ? 'Save sensor' : 'Add sensor' }}
      </button>
    </template>
  </Modal>
</template>
