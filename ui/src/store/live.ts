// Small reactive store fed by REST snapshots plus the SSE stream (/api/v1/events).
import { markRaw, reactive, shallowRef } from 'vue'
import { API_BASE, MAIN_RADIO, api, token, withRadio } from '@/api/client'
import type { Identity, LogLine, MeshNode, Message, Packet, Plugin, RadioSummary, RadiosResponse, RfStats, Sensor, Status, TracerouteEvent } from '@/api/types'

const PACKET_BUFFER = 400
const LOG_BUFFER = 1500
const HISTORY = 90 // status samples (5 s apart) ≈ 7.5 min

export interface StatusSample {
  time: number
  noise: number
  txPct: number
  chUtil: number
  rx: number
  tx: number
  dupe: number
  relayed: number
  undecryptable: number
  ackOk: number
  ackFail: number
}

// The GUI shows the whole site: every radio's identities, nodes, packets and status.
export const live = reactive({
  connected: false,
  /** The main radio's status (version, map tiles, restart reasons, meshtasticd health). */
  status: null as Status | null,
  /** Every radio's status, by radio id. */
  statuses: {} as Record<string, Status>,
  /** Every radio's identities. */
  identities: [] as Identity[],
  /** Nodes heard by any radio (heard_by says which). */
  nodes: {} as Record<string, MeshNode>,
  /** Status samples by radio id. */
  history: {} as Record<string, StatusSample[]>,
  /** Noise-floor points from /stats/rf by radio id, so sparklines aren't empty right after login. */
  noiseSeed: {} as Record<string, number[]>,
  lastEvent: 0,
  /** Every radio on this host (one entry on a single-radio host). */
  radios: [] as RadioSummary[],
  site: null as RadiosResponse['site'],
  /** Radios added in the config that start at the next restart. */
  pendingRadios: [] as { id: string; name: string; preset?: string }[],
})

/** A radio's display name from its id (running or waiting to start). */
export function radioName(id: string): string {
  return live.radios.find((r) => r.id === id)?.name ?? live.pendingRadios.find((r) => r.id === id)?.name ?? id
}


export async function refreshRadios() {
  try {
    const r = await api.get<RadiosResponse>('/radios')
    live.radios = r.radios
    live.site = r.site
    live.pendingRadios = (r.pending ?? []).filter((p) => p.action === 'start').map((p) => ({ id: p.id, name: p.name, preset: p.preset }))
  } catch {
    live.radios = []
  }
}

/** Newest first. Shallow so hundreds of packets don't become deep proxies. */
export const packets = shallowRef<Packet[]>([])
export const logs = shallowRef<LogLine[]>([])

type Handler<T> = (data: T) => void
const listeners = {
  packet: new Set<Handler<Packet>>(),
  message: new Set<Handler<{ identity: string; message: Message }>>(),
  traceroute: new Set<Handler<TracerouteEvent>>(),
  log: new Set<Handler<LogLine>>(),
  plugin: new Set<Handler<Plugin>>(),
  /** A sensor read, failed or was edited (or `deleted`). */
  sensor: new Set<Handler<Sensor>>(),
  /** The event stream reconnected after a pause: views that load their own data should reload. */
  resync: new Set<Handler<void>>(),
}
type Events = typeof listeners

export function on<K extends keyof Events>(event: K, fn: Events[K] extends Set<infer H> ? H : never): () => void {
  const set = listeners[event] as Set<typeof fn>
  set.add(fn)
  return () => set.delete(fn)
}

/** A status's radio (the main radio's when it doesn't say). */
export const statusRadio = (s: Status) => s.radio_id || MAIN_RADIO

function sample(s: Status) {
  const hist = (live.history[statusRadio(s)] ??= [])
  hist.push({
    time: Date.now(), noise: s.radio.noise_floor_dbm, txPct: s.airtime.tx_pct, chUtil: s.airtime.channel_util_pct,
    rx: s.counters.rx, tx: s.counters.tx, dupe: s.counters.rx_dupe, relayed: s.counters.relayed,
    undecryptable: s.counters.rx_undecryptable, ackOk: s.counters.ack_ok, ackFail: s.counters.ack_fail,
  })
  if (hist.length > HISTORY) hist.splice(0, hist.length - HISTORY)
}

// The daemon version this page was loaded against. When status reports another one the daemon
// was updated, and the next navigation loads the new GUI instead of running the old one.
let loadedVersion: string | null = null
export const newBuild = { available: false }

export function setStatus(s: Status) {
  if (s.version) {
    loadedVersion ??= s.version
    if (s.version !== loadedVersion) newBuild.available = true
  }
  live.statuses[statusRadio(s)] = s
  if (statusRadio(s) === MAIN_RADIO) live.status = s
  sample(s)
}

export function upsertIdentity(i: Identity) {
  const idx = live.identities.findIndex((x) => x.node_id === i.node_id)
  if (idx >= 0) live.identities[idx] = i
  else live.identities.push(i)
}

export function removeIdentity(id: string) {
  live.identities = live.identities.filter((i) => i.node_id !== id)
}

export async function refreshStatus() {
  setStatus(await api.get<Status>('/status'))
}

/** Every radio's status, one by one (the event stream sends them all every few seconds). */
export async function refreshStatuses() {
  await Promise.allSettled(live.radios.map((r) => api.get<Status>(withRadio('/status', r.id)).then(setStatus)))
}
export async function refreshIdentities() {
  live.identities = await api.get<Identity[]>('/identities?radio=all')
}
// Servers before the NodeInfo-defaults fix left names out for nodes heard without a
// NodeInfo; views sort and filter on them, so fill the firmware's placeholders here too.
export function normalizeNode(n: MeshNode): MeshNode {
  const short = n.node_id.slice(-4)
  n.long_name ??= `Meshtastic ${short}`
  n.short_name ??= short
  n.hw_model ??= 'UNSET'
  n.role ??= 'CLIENT'
  return n
}
export async function refreshNodes() {
  const list = await api.get<MeshNode[]>('/nodes?radio=all')
  const map: Record<string, MeshNode> = {}
  for (const n of list) map[n.node_id] = normalizeNode(n)
  live.nodes = map
}
export async function refreshPackets() {
  const list = await api.get<Packet[]>('/packets?limit=100&radio=all')
  packets.value = list.map((p) => markRaw(p))
}
export async function refreshLogs() {
  logs.value = await api.get<LogLine[]>('/logs?limit=500')
}

let source: EventSource | null = null
let retryTimer: number | undefined

function parse<T>(e: MessageEvent): T | null {
  try {
    return JSON.parse(e.data) as T
  } catch {
    return null
  }
}

export function connect() {
  disconnect()
  if (!token.value) return
  const es = new EventSource(API_BASE + `/events?radio=all&token=${encodeURIComponent(token.value)}`)
  source = es
  es.onopen = () => (live.connected = true)
  es.onerror = () => {
    live.connected = false
    if (es.readyState === EventSource.CLOSED) {
      // Browser gave up (e.g. 401 or daemon restart): probe status (triggers login on 401) and retry.
      clearTimeout(retryTimer)
      retryTimer = window.setTimeout(() => {
        refreshStatus().then(connect, () => token.value && connect())
      }, 4000)
    }
  }
  es.addEventListener('status', (e) => {
    const s = parse<Status>(e as MessageEvent)
    if (s) setStatus(s)
  })
  es.addEventListener('packet', (e) => {
    const p = parse<Packet>(e as MessageEvent)
    if (!p) return
    live.lastEvent = Date.now()
    const next = [markRaw(p), ...packets.value]
    if (next.length > PACKET_BUFFER) next.length = PACKET_BUFFER
    packets.value = next
    listeners.packet.forEach((fn) => fn(p))
  })
  es.addEventListener('identity', (e) => {
    const i = parse<Identity>(e as MessageEvent)
    if (i) upsertIdentity(i)
  })
  es.addEventListener('node', (e) => {
    const n = parse<MeshNode>(e as MessageEvent)
    if (n) live.nodes[n.node_id] = normalizeNode(n)
  })
  es.addEventListener('message', (e) => {
    const m = parse<{ identity: string; message: Message }>(e as MessageEvent)
    if (m) listeners.message.forEach((fn) => fn(m))
  })
  es.addEventListener('traceroute', (e) => {
    const t = parse<TracerouteEvent>(e as MessageEvent)
    if (t) listeners.traceroute.forEach((fn) => fn(t))
  })
  es.addEventListener('plugin', (e) => {
    const p = parse<Plugin>(e as MessageEvent)
    if (p) listeners.plugin.forEach((fn) => fn(p))
  })
  es.addEventListener('sensor', (e) => {
    const sn = parse<Sensor>(e as MessageEvent)
    if (sn) listeners.sensor.forEach((fn) => fn(sn))
  })
  es.addEventListener('log', (e) => {
    const l = parse<LogLine>(e as MessageEvent)
    if (!l) return
    // A new array each time: views read the lines through computed values, which only update
    // when the array they return is a different one.
    const next = logs.value.length >= LOG_BUFFER ? logs.value.slice(1 - LOG_BUFFER) : logs.value.slice()
    next.push(l)
    logs.value = next
    listeners.log.forEach((fn) => fn(l))
  })
}

export function disconnect() {
  clearTimeout(retryTimer)
  source?.close()
  source = null
  live.connected = false
}

let started = false
export async function startLive() {
  if (started) return
  started = true
  connect()
  await Promise.allSettled([
    refreshStatus(), refreshIdentities(), refreshNodes(), refreshPackets(), refreshLogs(), refreshRadios(),
  ])
  // Each radio's recent noise floor, for the sparklines.
  await Promise.allSettled(
    live.radios.map((r) =>
      api.get<RfStats>(withRadio('/stats/rf?window=1h', r.id)).then((s) => (live.noiseSeed[r.id] = s.points.slice(-40).map((p) => p.noise_floor_dbm))),
    ),
  )
}

// A browser allows only six connections to one address, and every open tab's event stream holds
// one for good: with six tabs open, nothing else can load. Tabs in the background give their
// stream up after a short while and catch up when they're shown again.
const HIDDEN_GRACE_MS = 15_000
let hiddenTimer: number | undefined
let pausedHidden = false
document.addEventListener('visibilitychange', () => {
  if (!started) return
  if (document.hidden) {
    clearTimeout(hiddenTimer)
    hiddenTimer = window.setTimeout(() => {
      if (document.hidden && started) {
        pausedHidden = true
        disconnect()
      }
    }, HIDDEN_GRACE_MS)
    return
  }
  clearTimeout(hiddenTimer)
  if (pausedHidden) {
    pausedHidden = false
    connect()
    void Promise.allSettled([refreshStatus(), refreshIdentities(), refreshNodes(), refreshPackets(), refreshLogs(), refreshRadios()])
    listeners.resync.forEach((fn) => fn())
  }
})

export function stopLive() {
  started = false
  disconnect()
  live.status = null
  live.statuses = {}
  live.identities = []
  live.nodes = {}
  live.history = {}
  live.noiseSeed = {}
  packets.value = []
  logs.value = []
}

// ---------------------------------------------------------------- lookups

export function nodeLabel(id: string): { short: string; long: string; local: boolean } {
  if (id === '!ffffffff') return { short: 'ALL', long: 'Broadcast', local: false }
  const n = live.nodes[id]
  if (n) return { short: n.short_name, long: n.long_name, local: n.local }
  const i = live.identities.find((x) => x.node_id === id)
  if (i) return { short: i.short_name, long: i.long_name, local: true }
  return { short: id.slice(-4), long: id, local: false }
}

/** Resolve a last-byte (next_hop / relay_node) to a known node, if unambiguous. */
export function nodeByLastByte(b: number): MeshNode | undefined {
  const hits = Object.values(live.nodes).filter((n) => (n.node_num & 0xff) === b)
  return hits.length === 1 ? hits[0] : undefined
}
