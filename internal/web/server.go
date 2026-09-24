// Package web serves the REST/SSE API in docs/api.md and the embedded GUI.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ScotMesh/RepeaterTastic/internal/config"
	"github.com/ScotMesh/RepeaterTastic/internal/links/mqtt"
	"github.com/ScotMesh/RepeaterTastic/internal/links/udp"
	"github.com/ScotMesh/RepeaterTastic/internal/logbuf"
	"github.com/ScotMesh/RepeaterTastic/internal/mesh"
	"github.com/ScotMesh/RepeaterTastic/internal/nodes"
	"github.com/ScotMesh/RepeaterTastic/internal/phoneapi"
	"github.com/ScotMesh/RepeaterTastic/internal/plugins"
	"github.com/ScotMesh/RepeaterTastic/internal/radio"
	"github.com/ScotMesh/RepeaterTastic/internal/sensors"
	"github.com/ScotMesh/RepeaterTastic/internal/site"
	"github.com/ScotMesh/RepeaterTastic/internal/wire"
)

type Options struct {
	Config  *config.Config
	Host    *mesh.Host // the main radio
	API     *phoneapi.Manager
	Logs    *logbuf.Buffer
	UDP     *udp.Link
	MQTT    []*mqtt.Link
	Radios  []Radio    // additional radios on the same site
	Site    *site.Site // nil with a single radio and no site budget
	Version string
	Log     *slog.Logger
	// MapAPIKey fills {api_key} in the map tile URL (empty drops the api_key parameter).
	MapAPIKey string
	// MapKeySource says where the key came from ("built in", "environment", "none"), never the key.
	MapKeySource string
	// LogLevel is the daemon's live log level; nil when the caller doesn't share it.
	LogLevel *slog.LevelVar
	// Restart shuts the daemon down cleanly for its supervisor to start again (nil = exit 75 at once).
	Restart func()
	// Plugins is the plugin manager (nil when plugins are turned off).
	Plugins *plugins.Manager
	// Hosted reports the meshtasticd instances the daemon runs (nil = none).
	// Hosting runs each radio's nodes on meshtasticd, by radio ID.
	Hosting map[string]*nodes.Hosting
	// Sensors holds the host's sensors (nil when the host has none: GET /sensors then reports
	// enabled false and the rest answer 503).
	Sensors *sensors.Registry
	// SensorsChanged hands a saved sensors section back to the daemon: the registry takes the new
	// sources, and hosted nodes learn who publishes what. Nil applies the sources to the registry
	// and nothing else.
	SensorsChanged func(config.Sensors) error
}

// HostedInstance is a meshtasticd the daemon runs, for Configuration → Nodes.
type HostedInstance struct {
	Radio string `json:"radio"`
	Role  string `json:"role"` // persona or identity
	nodes.HostedStatus
}

// Radio is an additional radio served by the same web GUI.
type Radio struct {
	ID, Name string
	Config   *config.Config // that radio's view of the configuration
	Host     *mesh.Host
	API      *phoneapi.Manager
	UDP      *udp.Link
	MQTT     []*mqtt.Link
}

// radioCtx is everything the web server keeps per radio.
type radioCtx struct {
	id, name string
	cfg      *config.Config // nil for the main radio: it follows Server.cfg, which the config API replaces
	host     *mesh.Host
	api      *phoneapi.Manager
	udp      *udp.Link
	mqtt     []*mqtt.Link

	// Modem stats cost serial round trips; share one poll between all viewers.
	statsMu   sync.Mutex
	statsAt   time.Time
	lastStats radio.Stats

	rf rfHistory
}

type Server struct {
	opt  Options
	cfg  *config.Config
	host *mesh.Host
	auth *Auth
	log  *slog.Logger
	mux  *http.ServeMux

	cfgMu      sync.Mutex
	loginFails sync.Map // ip → *loginState

	radios []*radioCtx // main first
	traces traceWait

	// booted is the configuration the daemon started with, to tell which saved changes still
	// need a restart.
	booted *config.Config
	// restorePending is set once a backup has been staged for the next start.
	restorePending atomic.Bool
	pluginKeys     assetKeys
}

func (rc *radioCtx) stats(ctx context.Context) radio.Stats {
	rc.statsMu.Lock()
	defer rc.statsMu.Unlock()
	if time.Since(rc.statsAt) > 5*time.Second {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		rc.lastStats = rc.host.Radio().Stats(cctx)
		cancel()
		rc.statsAt = time.Now()
	}
	return rc.lastStats
}

// radioStats is the main radio's modem stats (kept for callers without a request).
func (s *Server) radioStats(ctx context.Context) radio.Stats { return s.radios[0].stats(ctx) }

// radioFor picks the radio a request is about: the radio holding the identity in the
// path, else ?radio=<id>, else the main radio. Every endpoint therefore keeps working
// unchanged on a single-radio host.
func (s *Server) radioFor(r *http.Request) *radioCtx {
	if r == nil {
		return s.radios[0]
	}
	if rc := s.radioWithIdentity(r.PathValue("id")); rc != nil {
		return rc
	}
	if want := r.URL.Query().Get("radio"); want != "" {
		if rc := s.radioByID(want); rc != nil {
			return rc
		}
	}
	return s.radios[0]
}

// radioWithIdentity is the radio holding the identity with that node id, or nil.
func (s *Server) radioWithIdentity(raw string) *radioCtx {
	if raw == "" {
		return nil
	}
	num, err := wire.ParseNodeID(raw)
	if err != nil {
		return nil
	}
	return s.radioHolding(num)
}

func (s *Server) hostFor(r *http.Request) *mesh.Host { return s.radioFor(r).host }

// radiosFor is the radios a request is about: every radio with ?radio=all, else one.
func (s *Server) radiosFor(r *http.Request) []*radioCtx {
	if r.URL.Query().Get("radio") == "all" {
		return s.radios
	}
	return []*radioCtx{s.radioFor(r)}
}

// radioOf finds the radio an identity lives on.
func (s *Server) radioOf(id *mesh.Identity) *radioCtx {
	for _, rc := range s.radios {
		if rc.host.Identity(id.NodeNum) == id {
			return rc
		}
	}
	return s.radios[0]
}

// radioConfig is a radio's current configuration view.
func (s *Server) radioConfig(rc *radioCtx) *config.Config {
	if rc.cfg != nil {
		return rc.cfg
	}
	return s.cfg
}

type loginState struct {
	mu    sync.Mutex
	fails int
	until time.Time
}

func New(o Options) (*Server, error) {
	a, err := loadAuth(o.Config.StateDir)
	if err != nil {
		return nil, err
	}
	s := &Server{opt: o, cfg: o.Config, host: o.Host, auth: a, log: o.Log.With("component", "web"), mux: http.NewServeMux()}
	s.booted = cloneConfig(o.Config)
	s.radios = append(s.radios, &radioCtx{id: config.MainRadioID, name: o.Config.RadioConfigs()[0].Name, host: o.Host, api: o.API, udp: o.UDP, mqtt: o.MQTT})
	for _, x := range o.Radios {
		s.radios = append(s.radios, &radioCtx{id: x.ID, name: x.Name, cfg: x.Config, host: x.Host, api: x.API, udp: x.UDP, mqtt: x.MQTT})
	}
	s.traces.pending = map[string]time.Time{}
	a.ttl = func() time.Duration {
		s.cfgMu.Lock()
		defer s.cfgMu.Unlock()
		return s.cfg.Web.SessionTTL
	}
	s.routes()
	return s, nil
}

// Handler exposes the router (tests).
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) Run(ctx context.Context) error {
	for _, rc := range s.radios {
		go s.sampleRF(ctx, rc)
		go s.watchTraceroutes(ctx, rc)
	}
	if s.opt.Sensors != nil {
		go s.forwardSensorReads(ctx) // one subscription, fanned out to every radio's stream
	}
	addr := net.JoinHostPort(s.cfg.Web.Bind, strconv.Itoa(s.cfg.Web.Port))
	srv := &http.Server{Addr: addr, Handler: securityHeaders(s.mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	s.log.Info("web GUI listening", "addr", addr)
	if s.auth.SetupNeeded() {
		s.log.Warn("no admin password yet: open the web GUI to finish setup", "addr", addr)
	}
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		// Not same-origin: map tile servers (OpenStreetMap's policy) refuse requests without a
		// Referer. Cross-origin requests still only see the origin, never paths or queries.
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	pub := func(pattern string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, h) }
	priv := func(pattern string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, s.requireAuth(h)) }

	pub("GET /api/v1/setup", s.getSetup)
	pub("POST /api/v1/setup", s.postSetup)
	pub("POST /api/v1/auth/login", s.login)
	setup := func(pattern string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, s.setupOrAuth(h)) }
	setup("GET /api/v1/serial-ports", s.serialPorts)
	setup("GET /api/v1/boards", s.boards)
	setup("GET /api/v1/regions", s.regions)
	setup("POST /api/v1/phy/preview", s.phyPreview)
	setup("POST /api/v1/setup/probe", s.probe)
	setup("POST /api/v1/setup/meshtasticd", s.checkMeshtasticd)
	setup("GET /api/v1/setup/runtimes", s.runtimes)
	priv("PUT /api/v1/auth/password", s.changePassword)
	priv("POST /api/v1/auth/logout-all", s.logoutAll)

	priv("GET /api/v1/status", s.getStatus)
	priv("GET /api/v1/radios", s.listRadios)
	priv("POST /api/v1/radios", s.addRadio)
	priv("PATCH /api/v1/radios/{id}", s.patchRadio)
	priv("PUT /api/v1/radios/{id}", s.putRadio)
	priv("DELETE /api/v1/radios/{id}", s.deleteRadio)
	priv("GET /api/v1/hosted", s.getHosted)
	priv("PUT /api/v1/hosted", s.putHosted)
	priv("GET /api/v1/hosted/{name}/log", s.hostedLog)
	priv("GET /api/v1/site", s.getSite)
	priv("PUT /api/v1/site", s.putSite)
	priv("POST /api/v1/restart", s.restartDaemon)
	priv("PUT /api/v1/relay", s.putRelay)

	priv("GET /api/v1/identities", s.listIdentities)
	priv("POST /api/v1/identities", s.createIdentity)
	priv("POST /api/v1/identities/preview-key", s.previewKey)
	priv("PATCH /api/v1/identities/{id}", s.patchIdentity)
	priv("DELETE /api/v1/identities/{id}", s.deleteIdentity)
	priv("POST /api/v1/identities/{id}/move", s.moveIdentity)
	priv("GET /api/v1/nodes/{id}/sightings", s.nodeSightings)
	priv("GET /api/v1/identities/{id}/key", s.getKey)
	priv("PUT /api/v1/identities/{id}/channels/{index}", s.putChannel)
	priv("GET /api/v1/identities/{id}/channels/url", s.getChannelURL)
	priv("POST /api/v1/identities/{id}/channels/url", s.postChannelURL)
	priv("GET /api/v1/identities/{id}/conversations", s.conversations)
	priv("GET /api/v1/identities/{id}/messages", s.listMessages)
	priv("POST /api/v1/identities/{id}/messages", s.sendMessage)

	priv("GET /api/v1/nodes", s.listNodes)
	priv("POST /api/v1/nodes/{id}/traceroute", s.traceroute)
	priv("POST /api/v1/nodes/{id}/request-nodeinfo", s.requestNodeInfo)
	priv("DELETE /api/v1/nodes/{id}", s.deleteNode)

	priv("GET /api/v1/packets", s.listPackets)
	priv("GET /api/v1/events", s.events)
	priv("GET /api/v1/stats/airtime", s.statsAirtime)
	priv("GET /api/v1/stats/ports", s.statsPorts)

	priv("GET /api/v1/config", s.getConfig)
	priv("PUT /api/v1/config", s.putConfig)
	priv("GET /api/v1/tokens", s.listTokens)
	priv("POST /api/v1/tokens", s.createToken)
	priv("DELETE /api/v1/tokens/{id}", s.deleteToken)
	priv("GET /api/v1/backup", s.backup)
	priv("POST /api/v1/restore", s.restore)
	priv("GET /api/v1/logs", s.logs)
	priv("GET /api/v1/links", s.links)
	priv("PATCH /api/v1/links/{name}", s.patchLink)
	priv("POST /api/v1/identities/{id}/api/restart", s.restartAPI)
	priv("POST /api/v1/identities/{id}/conversations/{key}/read", s.markRead)
	priv("GET /api/v1/stats/rf", s.statsRF)
	priv("GET /api/v1/stats/identities", s.statsIdentities)
	s.pluginRoutes(priv)
	s.sensorRoutes(priv)

	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	s.mux.Handle("/", s.spa())
}

func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" && strings.HasSuffix(r.URL.Path, "/events") {
			tok = r.URL.Query().Get("token") // EventSource can't send headers; nowhere else, so tokens stay out of URLs and logs
		}
		if !s.auth.Valid(tok) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

// spa serves the embedded GUI with history-mode fallback.
func (s *Server) spa() http.Handler {
	dist, err := fs.Sub(Dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			// A missing asset is a 404, not the app page: a tab still running an older build asks
			// for chunks that no longer exist, and must see the failure so it can reload.
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set(cacheControl, "no-store")
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			p = "index.html"
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set(cacheControl, "public, max-age=31536000, immutable")
		} else {
			w.Header().Set(cacheControl, "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// cacheControl is the header that says how long a browser may keep a response.
const cacheControl = "Cache-Control"

// statusError is an error a handler's helper returns with the HTTP status it should get.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// errStatus makes a statusError.
func errStatus(code int, msg string) error { return &statusError{code: code, msg: msg} }

// writeStatusError writes err with the status it carries, or 400 when it carries none: an error
// from a handler's helper is the caller's fault unless the helper says otherwise.
func writeStatusError(w http.ResponseWriter, err error) {
	var se *statusError
	if errors.As(err, &se) {
		writeError(w, se.code, se.msg)
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
