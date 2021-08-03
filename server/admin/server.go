package adminserver

import (
	"context"
	"net"
	"net/http"

	"github.com/filecoin-project/go-indexer-core"
	httphandler "github.com/filecoin-project/storetheindex/server/handler/http"
	indnet "github.com/filecoin-project/storetheindex/server/net"
	"github.com/gorilla/mux"
	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("adminserver")

type Server struct {
	server *http.Server
	l      net.Listener
	engine *indexer.Engine
}

// Endpoint returns the endpoint of the protocol server.
func (s *Server) Endpoint() indnet.Endpoint {
	return indnet.HTTPEndpoint("http://" + s.l.Addr().String())
}

func New(listen string, e *indexer.Engine, options ...ServerOption) (*Server, error) {
	var cfg serverConfig
	if err := cfg.apply(append([]ServerOption{serverDefaults}, options...)...); err != nil {
		return nil, err
	}
	var err error

	l, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}

	r := mux.NewRouter().StrictSlash(true)
	server := &http.Server{
		Handler:      r,
		WriteTimeout: cfg.apiWriteTimeout,
		ReadTimeout:  cfg.apiReadTimeout,
	}
	s := &Server{server, l, e}

	// Set protocol handlers
	// Import routes
	r.HandleFunc("/import/manifest/{minerid}", httphandler.ImportManifestHandler(e)).Methods("POST")
	r.HandleFunc("/import/cidlist/{minerid}", httphandler.ImportCidListHandler(e)).Methods("POST")

	// Admin routes
	r.HandleFunc("/healthcheck", httphandler.HealthCheckHandler).Methods("GET")

	return s, nil
}

func (s *Server) Start() error {
	log.Infow("api listening", "addr", s.l.Addr())
	return s.server.Serve(s.l)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
