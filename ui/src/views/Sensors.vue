<script setup lang="ts">
// Sensors: the readings this host takes, and which identities publish each one as their own.
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Link2, Pencil, Plus, RefreshCw, Thermometer, Timer, Trash } from '@lucide/vue'
import { api, enc } from '@/api/client'
import type { Sensor, SensorsResponse } from '@/api/types'
import SensorReading from '@/components/sensors/SensorReading.vue'
import SensorDialog from '@/components/sensors/SensorDialog.vue'
import AttachSensorModal from '@/components/sensors/AttachSensorModal.vue'
import SensorIntervalModal from '@/components/sensors/SensorIntervalModal.vue'
import { live, nodeLabel, on } from '@/store/live'
import { confirmDialog } from '@/composables/confirm'
import { toast, toastError } from '@/composables/toast'
import { num } from '@/lib/format'

const data = ref<SensorsResponse | null>(null)
const loading = ref(false)
const addOpen = ref(false)
const editing = ref<Sensor | null>(null)
const attaching = ref<Sensor | null>(null)
const intervalOpen = ref(false)
const reading = ref('')

const kindLabel = { exec: 'command', file: 'file', push: 'pushed' } as const

async function load() {
  loading.value = true
  try {
    data.value = await api.get<SensorsResponse>('/sensors')
  } catch (e) {
    toastError(e)
  } finally {
    loading.value = false
  }
}

function upsert(s: Sensor) {
  if (!data.value) return
  const i = data.value.sensors.findIndex((x) => x.id === s.id)
  if (s.deleted) {
    if (i >= 0) data.value.sensors.splice(i, 1)
  } else if (i >= 0) data.value.sensors[i] = s
  else data.value.sensors.push(s)
}

// Read now shows exactly what came back: the values on success, what the command or file said on
// failure. Either way the row is up to date.
async function readNow(s: Sensor) {
  reading.value = s.id
  try {
    const out = await api.post<Sensor>(`/sensors/${enc(s.id)}/read`)
    upsert(out)
    const fields = Object.entries(out.last?.fields ?? {})
      .map(([f, v]) => `${f} ${num(v, 2)}`)
      .join(', ')
    toast(`${out.name}: ${fields || 'no readings'}`)
  } catch (e) {
    toastError(e)
    void load() // the failure is remembered against the sensor
  } finally {
    reading.value = ''
  }
}

async function remove(s: Sensor) {
  const who = s.identities.map((id) => nodeLabel(id).short).join(', ')
  const ok = await confirmDialog({
    title: `Delete ${s.name}?`,
    body: s.identities.length
      ? `It is removed from the host and from the ${s.identities.length} identity(ies) publishing it (${who}). Those nodes stop reporting it when they next start.`
      : 'It is removed from the host and from the config file. Nothing else changes.',
    confirm: 'Delete sensor',
    danger: true,
  })
  if (!ok) return
  try {
    await api.del(`/sensors/${enc(s.id)}`)
    upsert({ ...s, deleted: true })
    toast(`${s.name} deleted`)
  } catch (e) {
    toastError(e)
  }
}

const sensors = computed(() => data.value?.sensors ?? [])
const identityCount = computed(() => live.identities.length)

const off = on('sensor', upsert)
const offResync = on('resync', () => void load())
onMounted(load)
onBeforeUnmount(() => {
  off()
  offResync()
})
</script>

<template>
  <div>
    <div class="page-head">
      <div>
        <h2 class="page-title">Sensors</h2>
        <p class="page-sub">
          Readings this host takes — a command, a file, or something that pushes to the API — offered to identities as sensors of
          their own. One real sensor can appear on as many nodes as you like.
        </p>
      </div>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-sm" :disabled="loading" @click="load"><RefreshCw :class="['size-3.5', loading && 'animate-spin']" />Refresh</button>
        <button v-if="data?.enabled" type="button" class="btn btn-sm" @click="intervalOpen = true">
          <Timer class="size-3.5" />Broadcast every {{ (data.interval ?? '1h').replace(/(\d+h)0m0s$/, '$1').replace(/(\d+m)0s$/, '$1') }}
        </button>
        <button v-if="data?.enabled" type="button" class="btn btn-sm btn-primary" @click="addOpen = true"><Plus class="size-3.5" />Add sensor</button>
      </div>
    </div>

    <div v-if="!data" class="grid gap-4 md:grid-cols-2">
      <div v-for="n in 2" :key="n" class="card h-32 animate-pulse" />
    </div>

    <div v-else-if="!data.enabled" class="card empty">
      Sensors are turned off on this host. Add a <span class="mono">sensors:</span> section to the config file and restart.
    </div>

    <div v-else-if="!sensors.length" class="card flex flex-col items-center px-6 py-12 text-center">
      <span class="flex size-12 items-center justify-center rounded-2xl bg-sunken text-ink-3"><Thermometer class="size-6" /></span>
      <h3 class="mt-3 text-[15px] font-semibold">No sensors yet</h3>
      <p class="mt-1 max-w-md text-[13px] text-ink-3">
        Add a command to run, a file to read, or a push sensor for something that will send readings to the API. Then choose which
        identities publish it.
      </p>
      <button type="button" class="btn btn-primary mt-4" @click="addOpen = true"><Plus class="size-4" />Add sensor</button>
    </div>

    <div v-else class="grid gap-4 xl:grid-cols-2">
      <section v-for="s in sensors" :key="s.id" class="card flex flex-col p-4 sm:p-5">
        <div class="flex items-start gap-3">
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <h3 class="truncate text-[14px] font-semibold">{{ s.name }}</h3>
              <span class="chip bg-info/12 text-info">{{ kindLabel[s.kind] }}</span>
              <span v-if="s.interval" class="chip bg-ink-3/12 text-ink-3">every {{ s.interval.replace(/(\d+h)0m0s$/, '$1').replace(/(\d+m)0s$/, '$1') }}</span>
            </div>
            <div class="mono truncate text-xs text-ink-3" :title="s.command || s.path || s.id">
              {{ s.command || s.path || s.id }}
            </div>
          </div>
          <div class="flex shrink-0 items-center gap-0.5">
            <button type="button" class="icon-btn" title="Edit" @click="editing = s"><Pencil class="size-4" /></button>
            <button type="button" class="icon-btn hover:!text-bad" title="Delete" @click="remove(s)"><Trash class="size-4" /></button>
          </div>
        </div>

        <div class="mt-3 rounded-xl bg-raised/70 px-3.5 py-2.5">
          <SensorReading :sensor="s" :fields="data.fields" />
        </div>

        <p v-if="s.error" class="mt-2 text-xs text-bad">
          Last read failed: {{ s.error }}
        </p>
        <p v-else-if="s.errors" class="mt-2 text-xs text-ink-3">{{ s.reads }} reads, {{ s.errors }} failed.</p>

        <div class="mt-3">
          <div class="eyebrow mb-1">Published by</div>
          <div v-if="s.identities.length" class="flex flex-wrap gap-1">
            <span v-for="id in s.identities" :key="id" class="chip bg-brand/12 text-brand" :title="id">{{ nodeLabel(id).long }}</span>
          </div>
          <p v-else class="text-xs text-ink-3">
            Nobody yet<template v-if="identityCount"> — attach it to an identity to put it on air</template>.
          </p>
        </div>

        <div class="mt-auto flex flex-wrap justify-end gap-2 pt-4">
          <button type="button" class="btn btn-sm" :disabled="reading === s.id" @click="readNow(s)">
            <RefreshCw :class="['size-3.5', reading === s.id && 'animate-spin']" />Read now
          </button>
          <button type="button" class="btn btn-sm" @click="attaching = s"><Link2 class="size-3.5" />Attach</button>
        </div>
      </section>
    </div>

    <p v-if="data?.enabled && sensors.length" class="mt-4 text-xs text-ink-3">
      RepeaterTastic never sends a telemetry packet itself: it writes the reading where each node can see it, and the node detects
      the sensor at start-up and broadcasts on its own schedule. A node that has just been attached restarts to find it.
    </p>

    <SensorDialog :open="addOpen" :fields="data?.fields" @close="addOpen = false" @saved="(s) => { upsert(s); addOpen = false }" />
    <SensorDialog :open="!!editing" :sensor="editing" :fields="data?.fields" @close="editing = null" @saved="() => { void load(); editing = null }" />
    <AttachSensorModal :sensor="attaching" :fields="data?.fields" @close="attaching = null" @saved="() => { void load(); attaching = null }" />
    <SensorIntervalModal
      :open="intervalOpen"
      :interval="data?.interval"
      :nodes="identityCount"
      @close="intervalOpen = false"
      @saved="(iv) => { if (data) data.interval = iv; intervalOpen = false }"
    />
  </div>
</template>
