package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"

	gmetrics "github.com/armon/go-metrics"
	gprom "github.com/armon/go-metrics/prometheus"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/oklog/run"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/prober"
	"github.com/thanos-io/thanos/pkg/runutil"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/tracing"
	"github.com/thanos-io/thanos/pkg/tracing/client"
	"go.uber.org/automaxprocs/maxprocs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	logFormatLogfmt = "logfmt"
	logFormatJson   = "json"
)

type setupFunc func(*run.Group, log.Logger, *prometheus.Registry, opentracing.Tracer, bool) error

func main() {
	if os.Getenv("DEBUG") != "" {
		runtime.SetMutexProfileFraction(10)
		runtime.SetBlockProfileRate(10)
	}

	app := kingpin.New(filepath.Base(os.Args[0]), "A block storage based long-term storage for Prometheus")

	app.Version(version.Print("thanos"))
	app.HelpFlag.Short('h')

	debugName := app.Flag("debug.name", "Name to add as prefix to log lines.").Hidden().String()

	logLevel := app.Flag("log.level", "Log filtering level.").
		Default("info").Enum("error", "warn", "info", "debug")
	logFormat := app.Flag("log.format", "Log format to use.").
		Default(logFormatLogfmt).Enum(logFormatLogfmt, logFormatJson)

	tracingConfig := regCommonTracingFlags(app)

	cmds := map[string]setupFunc{}
	registerSidecar(cmds, app)
	registerStore(cmds, app, "store")
	registerQuery(cmds, app)
	registerRule(cmds, app)
	registerCompact(cmds, app)
	registerBucket(cmds, app, "bucket")
	registerDownsample(cmds, app)
	registerReceive(cmds, app)
	registerChecks(cmds, app, "check")

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "Error parsing commandline arguments"))
		app.Usage(os.Args[1:])
		os.Exit(2)
	}

	var logger log.Logger
	{
		var lvl level.Option
		switch *logLevel {
		case "error":
			lvl = level.AllowError()
		case "warn":
			lvl = level.AllowWarn()
		case "info":
			lvl = level.AllowInfo()
		case "debug":
			lvl = level.AllowDebug()
		default:
			panic("unexpected log level")
		}
		logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
		if *logFormat == logFormatJson {
			logger = log.NewJSONLogger(log.NewSyncWriter(os.Stderr))
		}
		logger = level.NewFilter(logger, lvl)

		if *debugName != "" {
			logger = log.With(logger, "name", *debugName)
		}

		logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	}

	loggerAdapter := func(template string, args ...interface{}) {
		level.Debug(logger).Log("msg", fmt.Sprintf(template, args))
	}

	// Running in container with limits but with empty/wrong value of GOMAXPROCS env var could lead to throttling by cpu
	// maxprocs will automate adjustment by using cgroups info about cpu limit if it set as value for runtime.GOMAXPROCS
	undo, err := maxprocs.Set(maxprocs.Logger(loggerAdapter))
	defer undo()
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "failed to set GOMAXPROCS: %v", err))
	}

	metrics := prometheus.NewRegistry()
	metrics.MustRegister(
		version.NewCollector("thanos"),
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	prometheus.DefaultRegisterer = metrics
	// Memberlist uses go-metrics.
	sink, err := gprom.NewPrometheusSink()
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "%s command failed", cmd))
		os.Exit(1)
	}
	gmetricsConfig := gmetrics.DefaultConfig("thanos_" + cmd)
	gmetricsConfig.EnableRuntimeMetrics = false
	if _, err = gmetrics.NewGlobal(gmetricsConfig, sink); err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "%s command failed", cmd))
		os.Exit(1)
	}

	var g run.Group
	var tracer opentracing.Tracer

	// Setup optional tracing.
	{
		ctx := context.Background()

		var closer io.Closer
		var confContentYaml []byte
		confContentYaml, err = tracingConfig.Content()
		if err != nil {
			level.Error(logger).Log("msg", "getting tracing config failed", "err", err)
			os.Exit(1)
		}

		if len(confContentYaml) == 0 {
			level.Info(logger).Log("msg", "Tracing will be disabled")
			tracer = client.NoopTracer()
		} else {
			tracer, closer, err = client.NewTracer(ctx, logger, metrics, confContentYaml)
			if err != nil {
				fmt.Fprintln(os.Stderr, errors.Wrapf(err, "tracing failed"))
				os.Exit(1)
			}
		}

		// This is bad, but Prometheus does not support any other tracer injections than just global one.
		// TODO(bplotka): Work with basictracer to handle gracefully tracker mismatches, and also with Prometheus to allow
		// tracer injection.
		opentracing.SetGlobalTracer(tracer)

		ctx, cancel := context.WithCancel(ctx)
		g.Add(func() error {
			<-ctx.Done()
			return ctx.Err()
		}, func(error) {
			if closer != nil {
				if err := closer.Close(); err != nil {
					level.Warn(logger).Log("msg", "closing tracer failed", "err", err)
				}
			}
			cancel()
		})
	}

	if err := cmds[cmd](&g, logger, metrics, tracer, *logLevel == "debug"); err != nil {
		level.Error(logger).Log("err", errors.Wrapf(err, "%s command failed", cmd))
		os.Exit(1)
	}

	// Listen for termination signals.
	{
		cancel := make(chan struct{})
		g.Add(func() error {
			return interrupt(logger, cancel)
		}, func(error) {
			close(cancel)
		})
	}

	if err := g.Run(); err != nil {
		level.Error(logger).Log("msg", "running command failed", "err", err)
		os.Exit(1)
	}
	level.Info(logger).Log("msg", "exiting")
}

func interrupt(logger log.Logger, cancel <-chan struct{}) error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-c:
		level.Info(logger).Log("msg", "caught signal. Exiting.", "signal", s)
		return nil
	case <-cancel:
		return errors.New("canceled")
	}
}

func registerProfile(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
}

func registerMetrics(mux *http.ServeMux, g prometheus.Gatherer) {
	mux.Handle("/metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
}

func defaultGRPCServerOpts(logger log.Logger, cert, key, clientCA string) ([]grpc.ServerOption, error) {
	opts := []grpc.ServerOption{}

	if key == "" && cert == "" {
		if clientCA != "" {
			return nil, errors.New("when a client CA is used a server key and certificate must also be provided")
		}

		level.Info(logger).Log("msg", "disabled TLS, key and cert must be set to enable")
		return opts, nil
	}

	if key == "" || cert == "" {
		return nil, errors.New("both server key and certificate must be provided")
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	tlsCert, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, errors.Wrap(err, "server credentials")
	}

	level.Info(logger).Log("msg", "enabled gRPC server side TLS")

	tlsCfg.Certificates = []tls.Certificate{tlsCert}

	if clientCA != "" {
		caPEM, err := ioutil.ReadFile(clientCA)
		if err != nil {
			return nil, errors.Wrap(err, "reading client CA")
		}

		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(caPEM) {
			return nil, errors.Wrap(err, "building client CA")
		}
		tlsCfg.ClientCAs = certPool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert

		level.Info(logger).Log("msg", "gRPC server TLS client verification enabled")
	}

	return append(opts, grpc.Creds(credentials.NewTLS(tlsCfg))), nil
}

func newStoreGRPCServer(logger log.Logger, reg *prometheus.Registry, tracer opentracing.Tracer, srv storepb.StoreServer, opts []grpc.ServerOption) *grpc.Server {
	met := grpc_prometheus.NewServerMetrics()
	met.EnableHandlingTimeHistogram(
		grpc_prometheus.WithHistogramBuckets([]float64{
			0.001, 0.01, 0.05, 0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4,
		}),
	)
	panicsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "thanos_grpc_req_panics_recovered_total",
		Help: "Total number of gRPC requests recovered from internal panic.",
	})
	reg.MustRegister(met, panicsTotal)

	grpcPanicRecoveryHandler := func(p interface{}) (err error) {
		panicsTotal.Inc()
		level.Error(logger).Log("msg", "recovered from panic", "panic", p, "stack", debug.Stack())
		return status.Errorf(codes.Internal, "%s", p)
	}
	opts = append(opts,
		grpc.MaxSendMsgSize(math.MaxInt32),
		grpc_middleware.WithUnaryServerChain(
			met.UnaryServerInterceptor(),
			tracing.UnaryServerInterceptor(tracer),
			grpc_recovery.UnaryServerInterceptor(grpc_recovery.WithRecoveryHandler(grpcPanicRecoveryHandler)),
		),
		grpc_middleware.WithStreamServerChain(
			met.StreamServerInterceptor(),
			tracing.StreamServerInterceptor(tracer),
			grpc_recovery.StreamServerInterceptor(grpc_recovery.WithRecoveryHandler(grpcPanicRecoveryHandler)),
		),
	)

	s := grpc.NewServer(opts...)
	storepb.RegisterStoreServer(s, srv)
	met.InitializeMetrics(s)

	return s
}

// TODO Remove once all components are migrated to the new scheduleHTTPServer.
// metricHTTPListenGroup is a run.Group that servers HTTP endpoint with only Prometheus metrics.
func metricHTTPListenGroup(g *run.Group, logger log.Logger, reg *prometheus.Registry, httpBindAddr string) error {
	mux := http.NewServeMux()
	registerMetrics(mux, reg)
	registerProfile(mux)

	l, err := net.Listen("tcp", httpBindAddr)
	if err != nil {
		return errors.Wrap(err, "listen metrics address")
	}

	g.Add(func() error {
		level.Info(logger).Log("msg", "Listening for metrics", "address", httpBindAddr)
		return errors.Wrap(http.Serve(l, mux), "serve metrics")
	}, func(error) {
		runutil.CloseWithLogOnErr(logger, l, "metric listener")
	})
	return nil
}

// scheduleHTTPServer starts a run.Group that servers HTTP endpoint with default endpoints providing Prometheus metrics,
// profiling and liveness/readiness probes.
func scheduleHTTPServer(g *run.Group, logger log.Logger, reg *prometheus.Registry, readinessProber *prober.Prober, httpBindAddr string, handler http.Handler, comp component.Component) error {
	mux := http.NewServeMux()
	registerMetrics(mux, reg)
	registerProfile(mux)
	readinessProber.RegisterInMux(mux)
	if handler != nil {
		mux.Handle("/", handler)
	}

	l, err := net.Listen("tcp", httpBindAddr)
	if err != nil {
		return errors.Wrap(err, "listen metrics address")
	}

	g.Add(func() error {
		level.Info(logger).Log("msg", "listening for requests and metrics", "component", comp.String(), "address", httpBindAddr)
		readinessProber.SetHealthy()
		return errors.Wrapf(http.Serve(l, mux), "serve %s and metrics", comp.String())
	}, func(err error) {
		readinessProber.SetNotHealthy(err)
		runutil.CloseWithLogOnErr(logger, l, "%s and metric listener", comp.String())
	})
	return nil
}
