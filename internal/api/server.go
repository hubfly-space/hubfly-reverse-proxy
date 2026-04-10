package api

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/hubfly/hubfly-reverse-proxy/internal/certbot"
	"github.com/hubfly/hubfly-reverse-proxy/internal/dockerengine"
	"github.com/hubfly/hubfly-reverse-proxy/internal/logmanager"
	"github.com/hubfly/hubfly-reverse-proxy/internal/nginx"
	"github.com/hubfly/hubfly-reverse-proxy/internal/store"
)

var certRetrySchedule = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

const certRetrySweepInterval = 30 * time.Second
const dockerFullCheckInterval = 5 * time.Minute
const missingContainerGracePeriod = 5 * time.Minute
const maxLoggedBodyBytes = 32 * 1024

type requestIDContextKey struct{}

type Server struct {
	Store             store.Store
	Nginx             *nginx.Manager
	Certbot           *certbot.Manager
	Docker            *dockerengine.Client
	DockerSync        bool
	LogManager        *logmanager.Manager
	BuildInfo         BuildInfo
	startedAt         time.Time
	retryMu           sync.Mutex
	retrying          map[string]bool
	syncMu            sync.Mutex
	fullCheckRequests chan string
}

type BuildInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

func NewServer(s store.Store, n *nginx.Manager, c *certbot.Manager, d *dockerengine.Client, dockerSync bool, l *logmanager.Manager, buildInfo BuildInfo) *Server {
	srv := &Server{
		Store:             s,
		Nginx:             n,
		Certbot:           c,
		Docker:            d,
		DockerSync:        dockerSync,
		LogManager:        l,
		BuildInfo:         buildInfo,
		startedAt:         time.Now(),
		retrying:          make(map[string]bool),
		fullCheckRequests: make(chan string, 1),
	}
	go srv.certRetryLoop()
	go srv.dockerSyncLoop()
	go srv.dockerEventLoop()
	return srv
}

func (s *Server) Bootstrap() {
	if s.Docker != nil && s.DockerSync {
		_, _, _ = s.syncSitesFromContainers()
		_, _, _ = s.syncStreamsFromContainers()
	}

	sites, err := s.Store.ListSites()
	if err == nil {
		for _, site := range sites {
			siteCopy := site
			s.refreshSiteConfig(&siteCopy)
		}
	}

	streams, err := s.Store.ListStreams()
	if err != nil {
		return
	}

	portsMap := make(map[int]bool)
	for _, stream := range streams {
		portsMap[stream.ListenPort] = true
	}
	ports := make([]int, 0, len(portsMap))
	for p := range portsMap {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for _, p := range ports {
		s.reconcileStreams(p)
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/management/version", s.handleManagementVersion)
	mux.HandleFunc("/v1/management/container-ports", s.handleContainerPorts)
	mux.HandleFunc("/v1/sites", s.handleSites)
	mux.HandleFunc("/v1/sites/", s.handleSiteDetail)
	mux.HandleFunc("/v1/streams", s.handleStreams)
	mux.HandleFunc("/v1/streams/", s.handleStreamDetail)
	mux.HandleFunc("/v1/control/reload", s.handleManualReload)
	mux.HandleFunc("/v1/control/full-check", s.handleManualFullCheckReload)
	mux.HandleFunc("/v1/control/recreate", s.handleManualRecreate)
	mux.HandleFunc("/v1/control/cache/purge", s.handleManualCachePurge)
	return s.loggingMiddleware(mux)
}
