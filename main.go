package main

import (
	"context"
	stdlog "log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	// flags
	prometheusMetricsAddress = kingpin.Flag("metrics-listen-address", "The address to listen on for Prometheus metrics requests.").Envar("PROMETHEUS_METRICS_PORT").Default(":9101").String()
	prometheusMetricsPath    = kingpin.Flag("metrics-path", "The path to listen for Prometheus metrics requests.").Envar("PROMETHEUS_METRICS_PATH").Default("/metrics").String()
	prometheusURL            = kingpin.Flag("prometheus-url", "The url to the Prometheus server).").Envar("PROMETHEUS_URL").String()
	googleComputeRegions     = kingpin.Flag("mig-config", "A comma-separated list of all managed instance groups, the Prometheus query to fetch request rate with, the target requests per instance.").Envar("MIG_CONFIG").String()

	// seed random number
	r = rand.New(rand.NewSource(time.Now().UnixNano()))

	// create gauge for tracking minimum number of instances per managed instance group
	minInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_mig_min_instances",
		Help: "The minimum number of instances per managed instance group as set by this application.",
	}, []string{"mig"})

	// create gauge for tracking actual number of instances per managed instance group
	actualInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_actual_min_instances",
		Help: "The actual number of instances per managed instance group as set by this application.",
	}, []string{"mig"})

	// create gauge for tracking request rate used to set minimum number of instances per managed instance group
	requestRate = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_request_rate",
		Help: "The request rate used for setting minimum number of instances per managed instance group as set by this application.",
	}, []string{"mig"})
)

func init() {
	prometheus.MustRegister(minInstances)
	prometheus.MustRegister(actualInstances)
	prometheus.MustRegister(requestRate)
}

func main() {

	// parse command line parameters
	kingpin.Parse()

	// log as severity for stackdriver logging to recognize the level
	zerolog.LevelFieldName = "severity"

	// set some default fields added to all logs
	log.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Str("app", "estafette-gcloud-mig-scaler").
		Str("version", version).
		Logger()

	// use zerolog for any logs sent via standard log library
	stdlog.SetFlags(0)
	stdlog.SetOutput(log.Logger)

	// log startup message
	log.Info().
		Str("branch", branch).
		Str("revision", revision).
		Str("buildDate", buildDate).
		Str("goVersion", goVersion).
		Msg("Starting estafette-gcloud-mig-scaler...")

	// define channel and wait group to gracefully shutdown the application
	gracefulShutdown := make(chan os.Signal)
	signal.Notify(gracefulShutdown, syscall.SIGTERM, syscall.SIGINT)
	waitGroup := &sync.WaitGroup{}

	// start prometheus
	go func() {
		log.Debug().
			Str("port", *prometheusMetricsAddress).
			Msg("Serving Prometheus metrics...")

		http.Handle(*prometheusMetricsPath, promhttp.Handler())

		if err := http.ListenAndServe(*prometheusMetricsAddress, nil); err != nil {
			log.Fatal().Err(err).Msg("Starting Prometheus listener failed")
		}
	}()

	ctx := context.Background()
	client, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
	if err != nil {
		log.Fatal().Err(err).Msg("Creating google cloud client failed")
	}

	computeService, err := compute.New(client)
	if err != nil {
		log.Fatal().Err(err).Msg("Creating google cloud service failed")
	}

	// update minimum instances
	go func(waitGroup *sync.WaitGroup) {
		// loop indefinitely
		for {
			// do work

			// sleep random time between 60s +- 25%
			sleepTime := applyJitter(60)
			log.Info().Msgf("Sleeping for %v seconds...", sleepTime)
			time.Sleep(time.Duration(sleepTime) * time.Second)
		}
	}(waitGroup)

	signalReceived := <-gracefulShutdown
	log.Info().
		Msgf("Received signal %v. Waiting on running tasks to finish...", signalReceived)

	waitGroup.Wait()

	log.Info().Msg("Shutting down...")
}

func applyJitter(input int) (output int) {

	deviation := int(0.25 * float64(input))

	return input - deviation + r.Intn(2*deviation)
}
