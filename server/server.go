package server

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // anonymous import to get the pprof handler registered
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/mwitkow/go-grpc-middleware"
	"github.com/opentracing-contrib/go-stdlib/nethttp"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"github.com/weaveworks-experiments/loki/pkg/client"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/middleware"
	"github.com/weaveworks/common/signals"
)

func init() {
	tracer, err := loki.NewTracer()
	if err != nil {
		panic(fmt.Sprintf("Failed to create tracer: %v", err))
	} else {
		opentracing.InitGlobalTracer(tracer)
	}
}

// Config for a Server
type Config struct {
	MetricsNamespace string
	LogSuccess       bool
	HTTPListenPort   int
	GRPCListenPort   int

	ServerGracefulShutdownTimeout time.Duration
	HTTPServerReadTimeout         time.Duration
	HTTPServerWriteTimeout        time.Duration
	HTTPServerIdleTimeout         time.Duration

	GRPCMiddleware []grpc.UnaryServerInterceptor
	HTTPMiddleware []middleware.Interface
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.BoolVar(&cfg.LogSuccess, "server.log-success", false, "Log successful requests")
	f.IntVar(&cfg.HTTPListenPort, "server.http-listen-port", 80, "HTTP server listen port.")
	f.IntVar(&cfg.GRPCListenPort, "server.grpc-listen-port", 9095, "gRPC server listen port.")
	f.DurationVar(&cfg.ServerGracefulShutdownTimeout, "server.graceful-shutdown-timeout", 5*time.Second, "Timeout for graceful shutdowns")
	f.DurationVar(&cfg.HTTPServerReadTimeout, "server.http-read-timeout", 5*time.Second, "Read timeout for HTTP server")
	f.DurationVar(&cfg.HTTPServerWriteTimeout, "server.http-write-timeout", 5*time.Second, "Write timeout for HTTP server")
	f.DurationVar(&cfg.HTTPServerIdleTimeout, "server.http-idle-timeout", 120*time.Second, "Idle timeout for HTTP server")
}

// Server wraps a HTTP and gRPC server, and some common initialization.
//
// Servers will be automatically instrumented for Prometheus metrics
// and Loki tracing.  HTTP over gRPC
type Server struct {
	cfg          Config
	handler      *signals.Handler
	httpListener net.Listener
	grpcListener net.Listener
	httpServer   *http.Server

	HTTP *mux.Router
	GRPC *grpc.Server
}

// New makes a new Server
func New(cfg Config) (*Server, error) {
	// Setup listeners first, so we can fail early if the port is in use.
	httpListener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.HTTPListenPort))
	if err != nil {
		return nil, err
	}

	grpcListener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCListenPort))
	if err != nil {
		return nil, err
	}

	// Prometheus histograms for requests.
	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: cfg.MetricsNamespace,
		Name:      "request_duration_seconds",
		Help:      "Time (in seconds) spent serving HTTP requests.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "route", "status_code", "ws"})
	prometheus.MustRegister(requestDuration)

	// Setup gRPC server
	grpcMiddleware := []grpc.UnaryServerInterceptor{
		middleware.ServerLoggingInterceptor(cfg.LogSuccess),
		middleware.ServerInstrumentInterceptor(requestDuration),
		otgrpc.OpenTracingServerInterceptor(opentracing.GlobalTracer()),
	}
	grpcMiddleware = append(grpcMiddleware, cfg.GRPCMiddleware...)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpcMiddleware...,
		)),
	)

	// Setup HTTP server
	router := mux.NewRouter()
	router.Handle("/metrics", prometheus.Handler())
	router.Handle("/traces", loki.Handler())
	router.PathPrefix("/debug/pprof").Handler(http.DefaultServeMux)
	httpMiddleware := []middleware.Interface{
		middleware.Log{
			LogSuccess: cfg.LogSuccess,
		},
		middleware.Instrument{
			Duration:     requestDuration,
			RouteMatcher: router,
		},
		middleware.Func(func(handler http.Handler) http.Handler {
			return nethttp.Middleware(opentracing.GlobalTracer(), handler)
		}),
	}
	httpMiddleware = append(httpMiddleware, cfg.HTTPMiddleware...)
	httpServer := &http.Server{
		ReadTimeout:  cfg.HTTPServerReadTimeout,
		WriteTimeout: cfg.HTTPServerWriteTimeout,
		IdleTimeout:  cfg.HTTPServerIdleTimeout,
		Handler:      middleware.Merge(httpMiddleware...).Wrap(router),
	}

	return &Server{
		cfg:          cfg,
		httpListener: httpListener,
		grpcListener: grpcListener,
		httpServer:   httpServer,
		handler:      signals.NewHandler(log.StandardLogger()),

		HTTP: router,
		GRPC: grpcServer,
	}, nil
}

// Run the server; blocks until SIGTERM is received.
func (s *Server) Run() {
	go s.httpServer.Serve(s.httpListener)
	httpServerCtx, cancel := context.WithCancel(context.Background())
	defer s.httpServer.Shutdown(httpServerCtx)

	// Setup gRPC server
	// for HTTP over gRPC, ensure we don't double-count the middleware
	httpgrpc.RegisterHTTPServer(s.GRPC, httpgrpc.NewServer(s.HTTP))
	go s.GRPC.Serve(s.grpcListener)
	defer s.GRPC.GracefulStop()

	// Wait for a signal
	s.handler.Loop()

	go func() {
		time.Sleep(s.cfg.ServerGracefulShutdownTimeout)
		cancel()
		s.GRPC.Stop()
	}()
}

// Stop the server, gracefully.  Unblocks Run().
func (s *Server) Stop() {
	s.handler.Stop()
}
