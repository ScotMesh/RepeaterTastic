<script setup lang="ts">
// What one persona publishes, from its own side. The same Attach dialog does the editing as on the
// Sensors page, so there is one place where publishing is decided.
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Link2, Pencil } from '@lucide/vue'
import { api, enc } from '@/api/client'
import type { Identity, Sensor, SensorAttachment, SensorsResponse } from '@/api/types'
import SensorReading from '@/components/sensors/SensorReading.vue'
import AttachSensorModal from '@/components/sensors/AttachSensorModal.vue'
import { on } from '@/store/live'
import { toastError } from '@/composables/toast'

const props = defineProps<{ identity: Identity }>()

const data = ref<SensorsResponse | null>(null)
const mine = ref<SensorAttachment[]>([])
const loading = ref(false)
const attaching = ref<Sensor | null>(null)

async function load() {
  loading.value = true
  try {
    const [all, ours] = await Promise.all([
      api.get<SensorsResponse>('/sensors'),
      api.get<SensorAttachment[]>(`/identities/${enc(props.identity.node_id)}/sensors`),
    ])
    data.value = all
    mine.value = ours
  } catch (e) {
    toastError(e)
  } finally {
    loading.value = false
  }
}
watch(() => props.identity.node_id, load, { immediate: true })

const sensors = computed(() => data.value?.sensors ?? [])
const published = computed(() =>
  mine.value
    .map((a) => ({ attachment: a, sensor: sensors.value.find((s) => s.id === a.sensor) }))
    .filter((r): r is { attachment: SensorAttachment; sensor: Sensor } => !!r.sensor),
)
const rest = computed(() => sensors.value.filter((s) => !mine.value.some((a) => a.sensor === s.id)))

const off = on('sensor', () => void load())
onBeforeUnmount(off)
</script>

<template>
  <div>
    <div v-if="loading && !data" class="empty">Loading sensors…</div>

    <div v-else-if="!data?.enabled" class="rounded-xl bg-raised px-3.5 py-3 text-[13px] text-ink-3">
      Sensors are turned off on this host.
    </div>

    <template v-else>
      <section>
        <h3 class="eyebrow mb-2">Published by this identity</h3>
        <p v-if="!published.length" class="rounded-xl bg-raised px-3.5 py-3 text-[13px] text-ink-3">
          Nothing yet. This node reports no sensors of its own.
        </p>
        <div
          v-for="r in published"
          :key="r.sensor.id"
          class="mb-2 rounded-xl border border-line-soft bg-raised/60 px-3.5 py-2.5"
        >
          <div class="flex items-start gap-2">
            <div class="min-w-0 flex-1">
              <div class="truncate text-[13px] font-medium">{{ r.sensor.name }}</div>
              <div class="mt-0.5 text-xs text-ink-3">
                {{ r.attachment.fields.length ? r.attachment.fields.join(', ') : 'everything it reports' }}
              </div>
            </div>
            <button type="button" class="btn btn-sm shrink-0" @click="attaching = r.sensor"><Pencil class="size-3.5" />Edit</button>
          </div>
          <div class="mt-2">
            <SensorReading :sensor="r.sensor" :fields="data.fields" :only="r.attachment.fields" />
          </div>
        </div>
      </section>

      <section v-if="rest.length" class="mt-5">
        <h3 class="eyebrow mb-2">Other sensors on this host</h3>
        <div v-for="s in rest" :key="s.id" class="mb-2 flex items-center gap-2 rounded-xl border border-line-soft px-3.5 py-2.5">
          <div class="min-w-0 flex-1">
            <div class="truncate text-[13px]">{{ s.name }}</div>
            <div class="mt-0.5"><SensorReading :sensor="s" :fields="data.fields" /></div>
          </div>
          <button type="button" class="btn btn-sm shrink-0" @click="attaching = s"><Link2 class="size-3.5" />Publish</button>
        </div>
      </section>

      <p class="mt-4 text-xs text-ink-3">
        Changing what this persona publishes restarts its node: it only looks for sensors when it starts. Nothing else on the host
        is interrupted.
      </p>
    </template>

    <AttachSensorModal :sensor="attaching" :fields="data?.fields" @close="attaching = null" @saved="() => { void load(); attaching = null }" />
  </div>
</template>
