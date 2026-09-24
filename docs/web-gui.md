# Using the web GUI

[← README](../README.md) · [Hardware](hardware.md) · [Configuration](configuration.md) · [Several radios](radios.md) · [MQTT](mqtt.md) · [Sensors](sensors.md) · [Plugins](plugins.md) · [HTTP API](api.md) · [Architecture](architecture.md)

Open `http://<host>:8080`. The first visit runs the setup wizard: the modem, region and preset,
the relay role (a Meshtastic role such as client, router or client mute) and the admin password. The **account menu** (top right)
changes the password, signs out, or signs every browser out.

Pages update live. A tab left in the background gives up its live connection after 15 seconds, so
many open tabs don't use up the browser's connections to the host, and catches up when shown again.

## Top bar

- **Every radio at once.** With one radio, the bar shows its modem state, frequency and preset, and
  the **airtime gauge** (this hour's transmit time against the duty-cycle budget). With several, each radio
  has a card: connection, preset and frequency, airtime gauge and its own relay mode menu (which
  says what each mode does).
- **Nothing is hidden behind a radio.** Every page shows the whole site. Packets, nodes, links, the
  dashboard and statistics have a **radio filter** to narrow them to one radio, and tables show
  which radio each row belongs to. The per-radio settings under Configuration (Relay, Airtime,
  Position, MQTT) have a radio selector at the top of the tab.
- **Relay switch:** the Meshtastic role of the radio's relay persona (client, client base, client
  mute, router or router late), then **Monitor** (the
  radio only listens and transmits nothing) and **Off** (the radio is ignored). Both ask before
  switching. In either, sends from identities, the relay persona and plugins fail with "the radio
  isn't transmitting". The same switch is under Configuration → Relay, with the rebroadcast mode.
- **meshtasticd chip:** how the site's nodes are doing — green "OK", blue "starting", amber "N of M
  down", or red "meshtasticd not running"/"relay down" (hover for the reasons). Click it for
  Configuration → meshtasticd.
- A **restart banner** appears under the bar when saved changes need a restart, listing them.

## Pages

| Page | What it's for |
| --- | --- |
| **Dashboard** | Radio health, noise floor, airtime, traffic and recent activity at a glance |
| **Identities** | Create, import, edit, move and delete identities. Each shows its app port, connected apps, airtime and channels. The relay persona is created for you and can't be deleted |
| **Chat** | Channel conversations and DMs for any identity, the relay persona included (**Speaking as**), with delivery ticks |
| **Channels** | Every identity's eight channel slots: add, edit and remove channels, or add one to several identities at once |
| **Nodes & map** | Nodes heard, with signal, hops and position on a map. A node's drawer sends a traceroute, NodeInfo request or message from the identity picked in **Send from** |
| **Packets** | Live packet log with decoded summaries |
| **Statistics** | Airtime per identity, traffic and RF history |
| **Links** | UDP multicast and each MQTT connection's state and counters |
| **Configuration** | Radios, Relay, Airtime & duty, Position & hardware, MQTT, Web & API tokens, meshtasticd, Backup & restore |
| **Sensors** | Add host sensors, watch their latest reading, and choose which identities publish each one as their own ([Sensors](sensors.md)) |
| **Plugins** | Install, attach, enable and configure plugins; their status, log and panel; **Send limits** for every plugin ([Plugins](plugins.md)) |
| **Logs** | The daemon's log, live, each line labelled with its radio and identity (filter by either). Each meshtasticd's own output is under Configuration → meshtasticd → Log |

## Common jobs

### Create an identity and connect the app

1. **Identities → New identity**: a name, a short name and a role (`CLIENT_MUTE` by default, so it
   doesn't repeat). A port is suggested.
2. The home radio is where it lives. With several radios you can pick it here or move it later.
3. In the Meshtastic app: **Connect → Network → `<host>:<port>`**. The app sees a normal node with
   this identity's channels and nodes.

**Import key** brings an existing node's identity across. Turn the original device off first.

### Add a channel

**Channels** → **+** on an empty slot. Choose an existing channel (so identities hear each other) or a
new one with a random, default, pasted or no key, and whether it goes to MQTT. **Add channel to
identities** does the same for several identities, each in its first free slot. Click a slot to edit
it, or **×** to remove it. The QR button shares or imports a `meshtastic.org/e/#…` channel URL.

Slot 0 is the primary channel. It's shared by every identity on a radio because its name picks the
frequency; change it under Configuration → Radios → Edit.

### Let an app change a node's settings

Identities → **Edit** → **App can change node settings** (off by default). Off, an app connected to
the identity can read everything and change its names and channels, but not its radio, device,
module or position settings, and it can't reboot or reset the node: those stay RepeaterTastic's to
manage.

### Publish a sensor on an identity

1. **Sensors → Add sensor**: a name, and where the reading comes from — a command to run, a file to
   read, or **push** for something that will send readings to the API.
2. **Read now** shows what came back, so a wrong command is obvious straight away.
3. **Attach** ticks the identities that should publish it and the fields each sends. Attaching
   restarts those identities' nodes — they only look for sensors when they start — and the dialog
   says which ones will bounce. Nothing else on the host is interrupted.

An identity's drawer has the same list from the other side: what this persona publishes.

### Move an identity to another radio

Identities → **Edit** → **Home radio**. Its key, node ID, app port and chats move with it; its
primary channel becomes the new radio's. Connected apps reconnect, and unsent messages are marked failed.

### Add a radio, MQTT, a position

- **Configuration → Radios → Add radio** ([Several radios](radios.md)).
- **Configuration → MQTT → Add connection** ([MQTT](mqtt.md)).
- **Configuration → Position & hardware** for the site position (drop a pin on the map or type it in), broadcast interval and advertised hardware.

### API tokens and backups

- **Configuration → Web & API tokens** creates tokens for scripts and Home Assistant
  (`Authorization: Bearer …`, see [the API](api.md)).
- **Configuration → Backup & restore** downloads everything, or restores a backup at the next restart.

## meshtasticd

**Configuration → meshtasticd** (also reached from the chip in the top bar) shows how every node is
doing, with the problems listed; the installed or Docker choice and the meshtasticd program or
image; the API port base; and a table of every running instance (radio, port, state, restarts, last
error). See [meshtasticd nodes](meshtasticd-nodes.md).
