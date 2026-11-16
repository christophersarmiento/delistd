package web

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/drumandbytes/delistd/internal/datadir"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/drumandbytes/delistd/internal/broker"
	"github.com/drumandbytes/delistd/internal/config"
	"github.com/drumandbytes/delistd/internal/history"
	"github.com/drumandbytes/delistd/internal/schedule"
	emaTemplate "github.com/drumandbytes/delistd/internal/template"
)

const (
	defaultRateLimit  = 30
	defaultRateWindow = time.Minute
	defaultSessionTTL = 30 * time.Minute
)

// Version is the build version shown in the web UI footer. main sets it from
// its own -ldflags-injected version at startup; it stays "dev" otherwise.
var Version = "dev"

type Server struct {
	config     atomic.Pointer[config.Config]
	configPath string
	reloadMu   sync.Mutex                            // guards cfgStamp
	cfgStamp   string                                // version of config.yaml the current config came from
	brokerDB   atomic.Pointer[broker.BrokerDatabase] // swapped by the "update broker list" action; read via brokers()
	// BrokerOverride is serve's --brokers flag, so a reload resolves the list
	// the same way startup did.
	BrokerOverride string
	// ListenHost is serve's --listen flag: the interface to bind. Empty means
	// 127.0.0.1. Only a container should set anything else (see listenAddr).
	ListenHost string
	// AllowedHosts is serve's --allowed-hosts flag (or DELISTD_ALLOWED_HOSTS):
	// hostnames/IPs permitted in Host headers. Wildcard "*" allows any.
	AllowedHosts    []string
	brokerUpdateURL string // broker.DefaultUpdateURL; tests point it at a local server
	historyStore    *history.Store
	tmplEngine      *emaTemplate.Engine
	templates       map[string]*template.Template
	httpServer      *http.Server
	port            int
	csrfKey         []byte
	sessions        *SessionStore
	rateLimiter     *RateLimiter
	jobManager      *JobManager
	dataDir         string // config directory: schedule lock and state

	// In-app scheduler (scheduler.go). The OS hooks are fields so tests
	// don't touch the real launchd/systemd setup.
	cycleMu       sync.Mutex
	cycleRunning  bool
	stopScheduler context.CancelFunc
	lifeMu        sync.Mutex // httpServer, stopScheduler, shutDown
	shutDown      bool
	osInstalled   func() bool
	installOS     func(schedule.Job) error
	removeOS      func() error
}

func NewServer(port int, cfg *config.Config, configPath string, brokerDB *broker.BrokerDatabase, historyStore *history.Store, tmplEngine *emaTemplate.Engine) (*Server, error) {
	csrfKey := make([]byte, 32)
	if _, err := rand.Read(csrfKey); err != nil {
		return nil, fmt.Errorf("failed to generate CSRF key: %w", err)
	}

	// job file next to the config, like history.DBPathFor; configPath is
	// "" only in tests
	dataDir := filepath.Dir(configPath)
	if configPath == "" {
		home, _ := os.UserHomeDir()
		dataDir = datadir.Of(home)
	}

	s := &Server{
		configPath:   configPath,
		historyStore: historyStore,
		tmplEngine:   tmplEngine,
		port:         port,
		csrfKey:      csrfKey,
		sessions:     NewSessionStore(defaultSessionTTL),
		rateLimiter:  NewRateLimiter(defaultRateLimit, defaultRateWindow),
		jobManager:   NewJobManager(),
		dataDir:      dataDir,
		osInstalled:  schedule.Installed,
		installOS:    schedule.Install,
		removeOS:     schedule.Remove,
	}
	s.config.Store(cfg)
	if configPath != "" {
		// "none" (no file yet) differs from any real stamp, so a config.yaml
		// created later by the CLI is picked up.
		if s.cfgStamp = fileStamp(configPath); s.cfgStamp == "" {
			s.cfgStamp = "none"
		}
	}

	// Paused send jobs used to be saved here for resume-on-restart; that's
	// gone, so drop any leftovers.
	if configPath != "" {
		leftovers, _ := filepath.Glob(filepath.Join(dataDir, "pending_job*.json"))
		for _, f := range leftovers {
			_ = os.Remove(f)
		}
	}

	tmpl, err := loadTemplates()
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}
	s.templates = tmpl
	s.brokerDB.Store(brokerDB)
	s.brokerUpdateURL = broker.DefaultUpdateURL
	return s, nil
}

// brokers returns the current broker list; see getConfig for why it's atomic.
func (s *Server) brokers() *broker.BrokerDatabase {
	return s.brokerDB.Load()
}

// getConfig returns the current config. It's an atomic.Pointer because
// settings handlers write it while handlers and send jobs read it; writers
// load-copy-mutate-store.
func (s *Server) getConfig() *config.Config {
	return s.config.Load()
}

// Start starts the web server and opens the browser
func (s *Server) Start() error {
	router := s.setupRouter()

	// serve's Ctrl+C handler can call Shutdown before or during this
	s.lifeMu.Lock()
	if s.shutDown {
		s.lifeMu.Unlock()
		return nil
	}
	s.httpServer = &http.Server{
		Addr:         listenAddr(s.ListenHost, s.port),
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stopScheduler = cancel
	srv := s.httpServer
	s.lifeMu.Unlock()

	// listen first: open the browser only once there's a server to load
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		cancel()
		return fmt.Errorf("server error: %w", err)
	}

	go s.runScheduler(ctx)
	// a non-loopback bind means a container: there's no browser to open
	if IsLoopbackListen(s.ListenHost) {
		go OpenBrowser(fmt.Sprintf("http://localhost:%d", s.port))
	}

	fmt.Printf("Starting Delistd web UI at http://localhost:%d\n", s.port)
	fmt.Println("Press Ctrl+C to stop")

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// Shutdown stops the server; called before Start, Start returns at once.
func (s *Server) Shutdown(ctx context.Context) error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	s.shutDown = true
	if s.stopScheduler != nil {
		s.stopScheduler()
	}
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// OpenBrowser opens url in the default browser; tests replace it.
var OpenBrowser = func(url string) {
	if cmd, args := browserCommand(runtime.GOOS, url); cmd != "" {
		_ = exec.Command(cmd, args...).Start()
	}
}

// browserCommand is the command that opens url on goos ("" = unsupported).
func browserCommand(goos, url string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "linux":
		return "xdg-open", []string{url}
	case "windows":
		return "cmd", []string{"/c", "start", url}
	}
	return "", nil
}

// activeProfileCookie holds the web UI's profile choice. Not HttpOnly/Secure:
// no secret, and it's validated against the config (bad value = first profile).
const activeProfileCookie = "delistd_profile"

// activeProfile is the request's profile: the cookie if it names a configured
// one, else the first. Unlike --profile, never ambiguous. Use this, not
// s.config.Profile.
func (s *Server) activeProfile(r *http.Request) config.NamedProfile {
	cfg := s.getConfig()
	if cfg == nil {
		return config.NamedProfile{ID: config.DefaultProfileID}
	}
	profiles := cfg.GetProfiles()
	if cookie, err := r.Cookie(activeProfileCookie); err == nil {
		for _, p := range profiles {
			if p.ID == cookie.Value {
				return p
			}
		}
	}
	return profiles[0]
}

// Helper methods
