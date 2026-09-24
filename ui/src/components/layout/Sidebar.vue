<script setup lang="ts">
// Structure after openHop's Sidebar (MIT, © Lloyd Newton), rewritten for Meshtastic identities.
import { computed } from 'vue'
import {
  Activity, Cable, ChartColumn, Layers, LayoutDashboard, MapPinned, MessagesSquare, Puzzle, ScrollText, Settings2, Thermometer, UsersRound, X,
} from '@lucide/vue'
import Logo from '@/components/ui/Logo.vue'
import Sparkline from '@/components/charts/Sparkline.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import AppFooter from '@/components/layout/AppFooter.vue'
import { live } from '@/store/live'
import { MAIN_RADIO } from '@/api/client'
import { num, uptime } from '@/lib/format'

defineProps<{ open: boolean }>()
const emit = defineEmits<{ close: [] }>()

const unread = computed(() => live.identities.reduce((s, i) => s + (i.unread ?? 0), 0))
const nodeCount = computed(() => Object.values(live.nodes).filter((n) => !n.local).length)
const multi = computed(() => live.radios.length > 1)

const groups = computed(() => [
  {
    label: 'Mesh',
    items: [
      { to: '/', name: 'dashboard', label: 'Dashboard', icon: LayoutDashboard },
      { to: '/identities', name: 'identities', label: 'Identities', icon: UsersRound, count: live.identities.length || undefined },
      { to: '/chat', name: 'chat', label: 'Chat', icon: MessagesSquare, badge: unread.value || undefined },
      { to: '/channels', name: 'channels', label: 'Channels', icon: Layers },
      { to: '/nodes', name: 'nodes', label: 'Nodes & map', icon: MapPinned, count: nodeCount.value || undefined },
    ],
  },
  {
    label: 'Traffic',
    items: [
      { to: '/packets', name: 'packets', label: 'Packets', icon: Activity },
      { to: '/statistics', name: 'statistics', label: 'Statistics', icon: ChartColumn },
      { to: '/links', name: 'links', label: 'Links', icon: Cable },
    ],
  },
  {
    label: 'System',
    items: [
      { to: '/config', name: 'config', label: 'Configuration', icon: Settings2 },
      { to: '/sensors', name: 'sensors', label: 'Sensors', icon: Thermometer },
      { to: '/plugins', name: 'plugins', label: 'Plugins', icon: Puzzle },
      { to: '/logs', name: 'logs', label: 'Logs', icon: ScrollText },
    ],
  },
])

const noise = computed(() => [...(live.noiseSeed[MAIN_RADIO] ?? []), ...(live.history[MAIN_RADIO] ?? []).map((h) => h.noise)].slice(-60))
</script>

<template>
  <Transition name="fade">
    <div v-if="open" class="fixed inset-0 z-[249] bg-black/30 backdrop-blur-[2px] lg:hidden" @click="emit('close')" />
  </Transition>
  <aside
    :class="[
      'fixed inset-y-0 left-0 z-[250] w-[272px] p-3 transition-transform duration-300 lg:static lg:z-auto lg:w-[264px] lg:shrink-0 lg:translate-x-0 lg:p-[14px]',
      open ? 'translate-x-0' : '-translate-x-full',
    ]"
  >
    <div class="card flex h-full flex-col overflow-hidden !bg-surface-solid lg:!bg-surface">
      <div class="flex items-center gap-3 px-4 pb-3 pt-4">
        <Logo :size="38" />
        <div class="min-w-0 flex-1 leading-tight">
          <div class="text-[15px] font-bold tracking-tight">Repeater<span class="text-brand">Tastic</span></div>
          <div class="text-2xs text-ink-3">Virtual Meshtastic nodes</div>
        </div>
        <button type="button" class="icon-btn lg:hidden" aria-label="Close menu" @click="emit('close')"><X class="size-4" /></button>
      </div>

      <!-- Single-radio site: the one relay persona, its noise floor and uptime, as before. -->
      <div v-if="!multi" class="mx-3 rounded-xl border border-line-soft bg-raised/70 p-3">
        <div class="flex items-center justify-between gap-2">
          <span class="eyebrow">Relay persona</span>
          <span :class="['inline-flex items-center gap-1.5 text-2xs font-medium', live.connected ? 'text-ok' : 'text-ink-3']">
            <span :class="['dot', live.connected ? 'pulse-dot bg-ok text-ok' : 'bg-ink-3']" />{{ live.connected ? 'Live' : 'Offline' }}
          </span>
        </div>
        <template v-if="live.status">
          <div class="mt-1.5 truncate text-[13px] font-semibold">{{ live.status.relay.long_name }}</div>
          <div class="flex items-center gap-1 text-xs text-ink-3">
            <span class="mono">{{ live.status.relay.node_id }}</span>
            <CopyButton :text="live.status.relay.node_id" label="Node id" />
            <span class="ml-auto">up {{ uptime(live.status.uptime_s) }}</span>
          </div>
          <div class="mt-2 border-t border-line-soft pt-2">
            <div class="flex items-baseline justify-between text-2xs text-ink-3">
              <span>Noise floor</span>
              <span class="font-semibold tabular-nums text-ink">{{ num(live.status.radio.noise_floor_dbm, 0) }} dBm</span>
            </div>
            <Sparkline class="mt-1" :data="noise" :height="22" color="var(--info)" />
          </div>
        </template>
        <div v-else class="mt-2 h-16 animate-pulse rounded-lg bg-sunken" />
      </div>

      <!-- Multi-radio site: every radio's persona and noise floor, compactly, plus the site's overall uptime. -->
      <div v-else class="mx-3 rounded-xl border border-line-soft bg-raised/70 p-3">
        <div class="flex items-center justify-between gap-2">
          <span class="eyebrow">Relay personas</span>
          <span :class="['inline-flex items-center gap-1.5 text-2xs font-medium', live.connected ? 'text-ok' : 'text-ink-3']">
            <span :class="['dot', live.connected ? 'pulse-dot bg-ok text-ok' : 'bg-ink-3']" />{{ live.connected ? 'Live' : 'Offline' }}
          </span>
        </div>
        <template v-if="live.radios.length">
          <div v-for="r in live.radios" :key="r.id" class="mt-2 border-t border-line-soft pt-2 first:mt-1.5 first:border-t-0 first:pt-0">
            <div class="flex items-center justify-between gap-2">
              <span class="truncate text-[13px] font-semibold" :title="r.relay.long_name">{{ r.relay.long_name ?? r.name }}</span>
              <span class="shrink-0 text-2xs tabular-nums text-ink-3">{{ num(r.noise_floor_dbm, 0) }} dBm</span>
            </div>
            <div class="flex items-center gap-1 text-2xs text-ink-3">
              <span :class="['dot size-1.5', r.connected ? 'bg-ok' : 'bg-ink-3']" />
              <span class="truncate">{{ r.name }}</span>
              <span class="mono ml-auto truncate">{{ r.relay.node_id ?? '—' }}</span>
            </div>
          </div>
          <div v-if="live.status" class="mt-2 border-t border-line-soft pt-2 text-2xs text-ink-3">up {{ uptime(live.status.uptime_s) }}</div>
        </template>
        <div v-else class="mt-2 h-16 animate-pulse rounded-lg bg-sunken" />
      </div>

      <nav class="mt-3 min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <div v-for="g in groups" :key="g.label" class="mb-3">
          <div class="eyebrow px-2.5 pb-1 pt-1">{{ g.label }}</div>
          <RouterLink
            v-for="item in g.items"
            :key="item.name"
            v-slot="{ href, navigate, isActive, isExactActive }"
            :to="item.to"
            custom
          >
            <a
              :href="href"
              :class="[
                'group relative flex h-9 items-center gap-2.5 rounded-xl px-2.5 text-[13px] font-medium transition-colors',
                (item.to === '/' ? isExactActive : isActive)
                  ? 'bg-brand/12 text-ink'
                  : 'text-ink-2 hover:bg-sunken hover:text-ink',
              ]"
              @click="navigate"
            >
              <span
                v-if="item.to === '/' ? isExactActive : isActive"
                class="absolute inset-y-2 left-0 w-[3px] rounded-full bg-brand"
              />
              <component
                :is="item.icon"
                :class="['size-[17px] shrink-0', (item.to === '/' ? isExactActive : isActive) ? 'text-brand' : 'text-ink-3 group-hover:text-ink-2']"
              />
              <span class="flex-1 truncate">{{ item.label }}</span>
              <span v-if="item.badge" class="badge">{{ item.badge }}</span>
              <span v-else-if="item.count" class="text-2xs tabular-nums text-ink-3">{{ item.count }}</span>
            </a>
          </RouterLink>
        </div>
      </nav>

      <div class="border-t border-line-soft px-4 py-3">
        <div class="text-2xs leading-snug text-ink-3">
          <div>RepeaterTastic <span class="tabular-nums">v{{ (live.status?.version ?? '…').replace(/^v/, '') }}</span></div>
          <div>UI layout after openHop (MIT)</div>
        </div>
        <AppFooter class="mt-2.5 border-t border-line-soft pt-2.5" />
      </div>
    </div>
  </aside>
</template>
