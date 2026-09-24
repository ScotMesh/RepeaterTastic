<script setup lang="ts">
// How often a node broadcasts the sensors it carries. One setting for every identity: the firmware's
// own floor is 30 minutes, and a mast full of identities shares the air with real people.
import { computed, ref, watch } from 'vue'
import { api } from '@/api/client'
import Modal from '@/components/ui/Modal.vue'
import Spinner from '@/components/ui/Spinner.vue'
import { toast } from '@/composables/toast'

const props = defineProps<{ open: boolean; interval?: string; nodes?: number }>()
const emit = defineEmits<{ close: []; saved: [interval: string] }>()

const choices = ['30m', '1h', '2h', '6h', '12h']
const value = ref('1h')
const busy = ref(false)
const error = ref('')

watch(
  () => props.open,
  (o) => {
    if (!o) return
    value.value = (props.interval ?? '1h').replace(/(\d+h)0m0s$/, '$1').replace(/(\d+m)0s$/, '$1')
    error.value = ''
  },
)

// One packet per identity per interval, on every radio of the mast.
const perDay = computed(() => {
  const m = /^(\d+)(m|h)$/.exec(value.value.trim())
  if (!m) return null
  const minutes = Number(m[1]) * (m[2] === 'h' ? 60 : 1)
  if (!minutes) return null
  return Math.round(((24 * 60) / minutes) * (props.nodes || 1))
})

async function save() {
  busy.value = true
  error.value = ''
  try {
    const out = await api.put<{ interval: string }>('/sensors/interval', { interval: value.value.trim() })
    toast('Broadcast interval saved')
    emit('saved', out.interval)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Modal :open="open" title="Broadcast interval" subtitle="How often each identity sends the readings it carries." size="sm" @close="emit('close')">
    <label class="label" for="si-iv">Broadcast every</label>
    <input id="si-iv" v-model="value" class="input mono" list="si-choices" spellcheck="false" />
    <datalist id="si-choices">
      <option v-for="c in choices" :key="c" :value="c" />
    </datalist>
    <p class="hint">A length of time: <span class="mono">30m</span>, <span class="mono">1h</span>, <span class="mono">6h</span>. At least 30 minutes — the firmware ignores anything shorter.</p>
    <p v-if="perDay" class="mt-3 rounded-xl bg-raised px-3 py-2.5 text-xs leading-relaxed text-ink-3">
      About {{ perDay }} telemetry packets a day across {{ nodes || 1 }} {{ (nodes || 1) === 1 ? 'identity' : 'identities' }}. Airtime is
      shared with everyone else on the channel, so send no more often than the readings are worth.
    </p>
    <p class="mt-3 text-xs text-ink-3">A node picks this up when it next starts.</p>
    <p v-if="error" class="mt-3 text-[13px] text-bad">{{ error }}</p>
    <template #footer>
      <button type="button" class="btn" @click="emit('close')">Cancel</button>
      <button type="button" class="btn btn-primary" :disabled="busy || !value.trim()" @click="save"><Spinner v-if="busy" />Save interval</button>
    </template>
  </Modal>
</template>
