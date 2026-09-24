<script setup lang="ts">
// One sensor's latest reading: each field's value with its unit, and how old it is. A reading older
// than three times the sensor's interval is greyed, because it may no longer be true.
import { computed } from 'vue'
import type { Sensor, SensorFieldInfo } from '@/api/types'
import { num, relTime } from '@/lib/format'
import { now } from '@/composables/now'

const props = defineProps<{ sensor: Sensor; fields?: SensorFieldInfo[]; only?: string[] }>()

const unit = (field: string) => props.fields?.find((f) => f.field === field)?.unit ?? ''

const values = computed(() => {
  const last = props.sensor.last
  if (!last) return []
  return Object.entries(last.fields)
    .filter(([f]) => !props.only?.length || props.only.includes(f))
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([field, value]) => ({ field, text: `${num(value, 2)} ${unit(field)}`.trim() }))
})
const age = computed(() => (props.sensor.last ? relTime(Date.parse(props.sensor.last.at), now.value) : 'never read'))
</script>

<template>
  <div v-if="!values.length" class="text-[13px] text-ink-3">
    {{ sensor.kind === 'push' ? 'Nothing pushed yet' : 'No reading yet' }}
  </div>
  <div v-else :class="['flex flex-wrap items-baseline gap-x-2 gap-y-1', sensor.fresh ? '' : 'opacity-50']">
    <span v-for="v in values" :key="v.field" class="text-[13px]">
      <span class="font-semibold tabular-nums">{{ v.text }}</span>
      <span class="ml-1 text-2xs text-ink-3">{{ v.field }}</span>
    </span>
    <span class="text-2xs text-ink-3" :title="sensor.fresh ? '' : 'Older than three times the interval: it may be stale'">
      {{ age }}<template v-if="!sensor.fresh"> · stale</template>
    </span>
  </div>
</template>
