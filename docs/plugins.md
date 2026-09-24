# Plugins

[← README](../README.md) · [Plugin API](plugin-api.md) · [Configuration](configuration.md) · [Web GUI](web-gui.md) · [HTTP API](api.md) · [Architecture](architecture.md)

A plugin is a separate program that extends RepeaterTastic. Examples: an uploader that sends what
the site hears to a mapping service, a bot that answers commands, or a dashboard. A plugin talks to
RepeaterTastic over the **Plugin API**, which is gRPC over a local socket. It only sees and does what
the operator allows, and it can't take the daemon down with it.

- [Installing](#installing): GUI upload or URL, dropping a bundle into a folder, the command line,
  or Docker
- [Enabling and permissions](#enabling-and-permissions)
- [Settings](#settings), [status, log and panel](#status-log-and-panel)
- [Attached plugins](#attached-plugins) that run in another container or on another machine
- [Plugins in Docker](#plugins-in-docker)
- [Configuration](#configuration): the `plugins:` section
- [Writing a plugin](#writing-a-plugin): the bundle, `plugin.yaml`, how a plugin works and the Go
  SDK

For plugin authors, the [Plugin API reference](plugin-api.md) covers every message, call, error,
manifest field and setting type. The plugin endpoints of the HTTP API are in
[HTTP API → Plugins](api.md#plugins).

## The store

**Plugins → Browse store** lists plugins from a store index, which is a JSON file someone
publishes over HTTPS: names, logos, permissions and download addresses. Nothing in the store runs
on your node. Press **Install** and RepeaterTastic downloads the bundle from wherever the index
points, **checks it against the sha256 the store lists**, refuses it if that doesn't match, and
then installs it like any other bundle: switched off, waiting for you to grant its permissions.

Before you install, the card shows what the plugin will ask for, what it talks to over the
network, and whether it sends on the radio. A plugin this node can't run says so instead of
offering a button that fails: no build for this CPU, a newer RepeaterTastic needed, or a
permission this version doesn't have.

When the store lists a newer version of something you have installed, the Plugins tab badges it
and the card offers **Update**. An update keeps the plugin's switch, settings and grants.

The default store is [ScotMesh's](https://github.com/ScotMesh/repeatertastic-plugins). Point
`plugins.store_url` at your own index to run a different one, or set it to `off` to hide the tab
entirely. The index is cached on disk with its ETag, so a node that checks regularly costs the
server a 304, and the Browse tab still lists what it last saw when the node is off the internet.

The daemon does the fetching, the downloading and the logos, not your browser, so the store works
on a node you reach over the mesh, a VPN or a private network.

## Installing

A plugin comes as a **bundle**: a `.zip` holding a `plugin.yaml`, its program (usually one
binary per CPU type), and optionally a logo and a web panel. A new plugin is installed **switched
off**.

| How | What to do |
| --- | --- |
| Store | **Plugins → Browse store**, then **Install** (see [The store](#the-store)) |
| GUI | **Plugins → Install plugin**, then drop the .zip or paste a URL |
| Folder | Copy the .zip into the plugins folder's `inbox/` (`<state_dir>/plugins/inbox/`, or `<plugins.dir>/inbox/` when `plugins.dir` is set). It installs within a few seconds; a bundle that can't be used moves to `inbox/.rejected/` next to a `.error.txt` giving the reason |
| Command line | `sudo -u repeatertastic repeatertastic plugin install hello-plugin.zip` (a file or an `http(s)://` URL) |
| Docker | `docker cp hello-plugin.zip repeatertastic:/data/plugins/inbox/`, or the command line inside the container: `docker exec repeatertastic repeatertastic plugin list` |

Installing a bundle with the same `id` upgrades the plugin. The upgrade keeps the plugin's
switch, settings and granted permissions and restarts it. If the new version asks for permissions
you haven't reviewed, it waits in **needs review** until you look at them.

Bundles are checked before anything is written. RepeaterTastic refuses a bundle that:

- has paths outside the bundle, links, or device files;
- is more than 100 MB zipped or 250 MB unpacked;
- has a `plugin.yaml` that doesn't validate;
- has no program for this machine's OS and CPU.

## Enabling and permissions

Turning a plugin on shows what it asks for. Untick anything you don't want: a plugin that calls
for a permission it wasn't granted gets a "permission denied" error.

| Permission | Lets the plugin |
| --- | --- |
| `packets.read` | See every packet the radios hear and send, decoded where RepeaterTastic can read it |
| `nodes.read` | See the node database (and traceroute results) |
| `messages.read` | Read text messages to and from each radio's relay persona |
| `messages.send` | Send text messages from each radio's relay persona |
| `traceroute.send` | Send traceroutes from the identity chosen in the plugin's settings on that radio, or the radio's relay persona when none is chosen |
| `status.read` | See how the radios are doing: airtime, noise floor, channel use and the packet counters |
| `sensors.publish` | Give readings to the host's push sensors, which the identities they're attached to publish as their own ([Sensors](sensors.md)) |

Plugins send text as the **relay persona** of each radio: the node the site already is on the
mesh. Traceroutes go from the identity you choose in the plugin's settings, or the relay persona.
Transmissions go through the normal transmit queue and duty cycle, and are refused while a radio's
relay is in Monitor or Off mode.

A plugin with `sensors.publish` can feed a sensor you created with kind **push** ([Sensors](sensors.md)),
which is how a plugin that owns some hardware, or fetches a reading from elsewhere, gets it on air.
It can't create a sensor or decide which identities publish it: you do that under **Sensors**.

Each plugin also has a send budget, 30 messages and 12 traceroutes an hour by default. A plugin
may send a sixth of its hourly budget at once (at least one), then the budget refills evenly: with
12 traceroutes an hour, that's 2 straight away and then one every 5 minutes. Change them under
**Plugins → Send limits**, which applies at once and saves them to the config file as
`plugins.messages_per_hour` and `traceroutes_per_hour`. The limits are 0-600 messages and 0-120
traceroutes an hour; `0` stops plugins sending at all. Every send is written to the plugin's log. The radio's own limits still apply too, such as one
traceroute per identity every 30 seconds.

A plugin's `network` list (shown before you enable it) names the services it talks to. It is a
declaration, not a firewall: a plugin is a program running as the RepeaterTastic user, so only
install plugins you trust.

## Settings

The **Settings** tab is a form built from the plugin's `plugin.yaml`. Lists (a choice of options,
radios or identities) are tick-box lists. Secrets are write-only:
the API and GUI show that one is saved, never its value. Changes reach a running plugin at once.
Settings and grants are kept in `<state_dir>/plugins/state.json` (mode 0600).

## Status, log and panel

- **Status**: a one-line summary on the plugin's card, for example "Uploading · 1,204 packets
  today", plus up to 40 label/value fields on its page (a status with more is ignored).
- **Log**: what the plugin printed or reported, and what RepeaterTastic did with it (started,
  crashed, sent a message). The last 1000 lines are kept.
- **Panel**: a plugin may ship its own page. It runs in a sandboxed frame on the plugin's page. It
  has no access to the GUI, your login or the API, and exchanges messages with the GUI only through
  `postMessage` (see [Panels](plugin-api.md#panels)).

A managed plugin that exits is restarted with backoff (1 s up to a minute). After five exits
within 15 seconds of starting, it is left **crashed** until you press **Try again**.

## Attached plugins

A plugin can also run somewhere else and connect in over TCP, for example a Python bot in another
container.

1. Set `plugins.listen` (for example `127.0.0.1:4450`, or a LAN address) and restart.
2. **Plugins → Attach**: give its id, name and permissions. The token is shown **once**.
3. Start the plugin with `RT_PLUGIN_ID` (exactly the id you attached), `RT_PLUGIN_ADDR=<host>:4450`
   and `RT_PLUGIN_TOKEN`.

What to expect:

- The id in `Hello`, and in the `plugin.yaml` the plugin sends, must match the attached id, or
  the session is refused.
- The first time it connects, the plugin sends its `plugin.yaml`, and its settings form and
  permissions appear. RepeaterTastic then **refuses the session** until required settings are
  filled in and any permissions it asks for beyond the ones granted at attach are reviewed. An
  attached plugin should keep retrying with a backoff.
- Attached plugins get no data folder (`Welcome.data_dir` is empty); they keep their own state.
- The TCP connection isn't encrypted. Keep it on localhost, a private network or a VPN.
- **New token** on the plugin's page replaces the token and disconnects the old one.

## Plugins in Docker

Managed plugins run inside the RepeaterTastic container, next to meshtasticd:

- **Where they live:** bundles unpack into `/data/plugins/installed/`, and each plugin's data goes
  in `/data/plugins/data/<id>/`, both on the `/data` volume. They survive image upgrades and
  container re-creation.
- **Which program runs:** `{os}` is `linux` and `{arch}` is the container's CPU (`amd64`, `arm64`, or
  `arm` for arm/v7), so a bundle built by `scripts/bundle-plugin.sh` just works.
- **As whom:** the container's unprivileged `repeatertastic` user. A plugin can use the container's
  network, but not the host's devices.
- **What's available:** the image is Debian 13 (glibc 2.41, `sh` and `bash`) with nothing else
  installed. Static binaries are the safe choice; dynamically linked ones work if they only need
  glibc. A plugin written in Python or Node needs either an image of your own built on this one:

  ```dockerfile
  FROM ghcr.io/scotmesh/repeatertastic:latest
  USER root
  RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
  USER repeatertastic
  ```

  or to run as an [attached plugin](#attached-plugins) in its own container.
- **Run the container with `--init`** (`init: true` in Compose). RepeaterTastic is the container's
  first process, and a plugin that starts helper processes and leaves them behind would otherwise
  leave zombies.
- **Installing:** use the GUI, `docker cp hello-plugin.zip repeatertastic:/data/plugins/inbox/`, or
  `docker exec repeatertastic repeatertastic plugin install /data/hello-plugin.zip` (the command
  line already runs as the right user in the container). A bundle mounted from the host needs to be
  readable by that user.
- **Attached plugins in another container:** set `plugins.listen` to `0.0.0.0:4450` and either
  give the plugin container `network_mode: "service:repeatertastic"` and `RT_PLUGIN_ADDR=127.0.0.1:4450`,
  or put both on a Docker network and use `RT_PLUGIN_ADDR=repeatertastic:4450`. Don't publish the
  port beyond that: the connection isn't encrypted.

## Configuration

```yaml
plugins:
    enabled: true                 # the plugin system
    dir: ""                       # "" = <state_dir>/plugins
    listen: ""                    # TCP address for attached plugins, e.g. 127.0.0.1:4450 ("" = off)
    allow_url_install: true       # the GUI may download bundles from a URL
    store_url: ""                 # "" = the ScotMesh store; an index.json URL for your own; "off" = no store tab
    messages_per_hour: 30         # per plugin, 0-600; 0 = plugins may not send messages (also Plugins → Send limits)
    traceroutes_per_hour: 12      # per plugin, 0-120; 0 = plugins may not send traceroutes
    entries:                      # pin plugins: the GUI shows these as "config file" and won't change them
        - id: my-plugin
          enabled: true
          permissions: [packets.read, nodes.read, traceroute.send]
          settings:
              api_key: ${MY_PLUGIN_API_KEY}  # ${VAR} is read from the environment
```

Pinned entries still need the plugin installed. Changes to the `plugins:` section need a restart
(the GUI's restart banner lists them). The folder looks like this:

```
plugins/
  host.sock            the Plugin API socket for managed plugins (in a private temp folder when this path is too long)
  state.json           switches, grants and settings
  inbox/               drop bundles here (.rejected/ holds refused ones)
  installed/<id>/      unpacked bundles
  data/<id>/           each plugin's own folder (its HOME and RT_PLUGIN_DATA)
  store/               the cached store index, its logos and in-flight downloads
```

### Command line

```
repeatertastic plugin [-config <file>] <command>     -config defaults to /etc/repeatertastic/repeatertastic.yaml ($REPEATERTASTIC_CONFIG)
repeatertastic plugin list                          installed plugins, on or off, granted permissions
repeatertastic plugin install <bundle.zip | http(s) URL>
repeatertastic plugin enable <id> [permission ...]  "all" grants everything the plugin asks for
repeatertastic plugin disable <id>
repeatertastic plugin remove <id> [-keep-data]
repeatertastic plugin permissions
```

Run the command as the user RepeaterTastic runs as. A running daemon picks up changes within a few
seconds; `install` hands the bundle to it through the inbox.

## Writing a plugin

### The bundle

```
plugin.yaml
logo.svg                 PNG, SVG or WebP
bin/hello-linux-arm64    one static binary per target (see Plugins in Docker)
bin/hello-linux-arm
bin/hello-linux-amd64
panel/index.html         optional
```

`scripts/bundle-plugin.sh <dir> <go package> <out.zip>` cross-compiles and zips a Go plugin;
`make plugin-example` builds [the example](../examples/plugins/hello).

### `plugin.yaml`

```yaml
id: hello                  # 2-40 lowercase letters, digits, dashes; never change it
name: Hello Mesh
version: 1.0.0
api: 1                     # Plugin API version
description: Counts what each radio hears.
author: ScotMesh
homepage: https://github.com/…
license: GPL-3.0-or-later
logo: logo.svg
permissions: [packets.read, nodes.read]
network: [api.example.org] # services it talks to, shown before enabling
settings:
  - key: api_key           # lowercase, digits, underscores
    label: API key
    type: secret           # string, secret, url, bool, int, number, select, multiselect, radios, identities
    required: true
    help: From your account page.
  - key: region
    type: select
    options: [scotland, england]
    default: scotland
  - key: radios
    label: Radios
    type: radios           # tick boxes of the site's radios; the value is a list of radio IDs
    placeholder: All radios  # what an empty list shows as
  - key: ports
    type: multiselect      # tick boxes of options; the value is a list
    options: [TEXT_MESSAGE_APP, POSITION_APP]
  - key: report_as
    type: identities       # tick boxes of the site's identities; traceroutes are sent from these
run:
  managed:
    exec: bin/hello-{os}-{arch}   # {os} and {arch} are Go's GOOS and GOARCH (arm for 32-bit Pis)
    args: []
ui:
  panel: panel/index.html
```

Every field and setting type is described in the [manifest reference](plugin-api.md#manifest-pluginyaml).

### How a plugin works

A plugin is a gRPC client. The [Plugin API reference](plugin-api.md) has every message, call,
error and manifest field; in short:

1. RepeaterTastic starts a managed plugin with `RT_PLUGIN_ID`, `RT_PLUGIN_SOCKET`, `RT_PLUGIN_TOKEN`
   and `RT_PLUGIN_DATA` in its environment. An attached plugin is given its address and token.
2. The plugin opens a `Session` and sends `Hello`. It gets `Welcome`, with its granted permissions,
   settings and the site's radios, and then the events it may see: packets, node changes, relay
   persona messages, traceroute replies, settings changes and panel actions.
3. It reports `Status`, log lines and panel data on the same stream.
4. While the session is open, it calls `ListRadios`, `ListNodes`, `SendText` and `Traceroute`.
   Sends fail while a radio's relay is in Monitor or Off mode.
5. When RepeaterTastic sends `Stop`, it closes the stream and exits. After 5 seconds it gets SIGTERM,
   and after another 5 SIGKILL.

The Go SDK does the connecting for you:

```go
c, err := sdk.Connect(ctx, sdk.Options{Version: "1.0.0"}) // reads RT_PLUGIN_* from the environment
if err != nil { log.Fatal(err) }
defer c.Close()
_ = c.Status("connected", "ok", nil)
for msg := range c.Events() {
    if p := msg.GetPacket(); p != nil { /* … */ }
}
```

Other languages can generate a client from
[`api/plugin/v1/plugin.proto`](../api/plugin/v1/plugin.proto).
