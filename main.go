package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/sethgrid/pester"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MIGConfiguration has all the config needed for a single managed instance group to be scaled
type MIGConfiguration struct {
	GCloudProject                string  `json:"gcloudProject"`
	GCloudZone                   string  `json:"gcloudZone"`
	RequestRateQuery             string  `json:"requestRateQuery"`
	InstanceGroupName            string  `json:"instanceGroupName"`
	MinimumNumberOfInstances     int     `json:"minimumNumberOfInstances"`
	NumberOfRequestsPerInstance  float64 `json:"numberOfRequestsPerInstance"`
	NumberOfInstancesBelowTarget int     `json:"numberOfInstancesBelowTarget"`
	EnableSettingMinInstances    bool    `json:"enableSettingMinInstances"`
}

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
	migConfig                = kingpin.Flag("mig-config", "A json array of configuration for all managed instance groups, the Prometheus query to fetch request rate with, the target requests per instance.").Envar("MIG_CONFIG").String()

	// seed random number
	r = rand.New(rand.NewSource(time.Now().UnixNano()))

	// create gauge for tracking minimum number of instances per managed instance group
	minInstancesVector = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_mig_scaler_min_instances",
		Help: "The minimum number of instances per managed instance group as set by this application.",
	}, []string{"mig"})

	// create gauge for tracking actual number of instances per managed instance group
	actualInstancesVector = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_mig_scaler_actual_instances",
		Help: "The actual number of instances per managed instance group as set by this application.",
	}, []string{"mig"})

	// create gauge for tracking request rate used to set minimum number of instances per managed instance group
	requestRateVector = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_mig_scaler_request_rate",
		Help: "The request rate used for setting minimum number of instances per managed instance group as set by this application.",
	}, []string{"mig"})
)

func init() {
	prometheus.MustRegister(minInstancesVector)
	prometheus.MustRegister(actualInstancesVector)
	prometheus.MustRegister(requestRateVector)
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

	var migConfigs []MIGConfiguration

	if err := json.Unmarshal([]byte(*migConfig), &migConfigs); err != nil {
		// couldn't deserialize, setting to default struct
		log.Fatal().Err(err).Msg("Unmarshalling migConfig failed")
	}

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
			// loop through configs
			for _, configItem := range migConfigs {

				log.Info().Msgf("Retrieving data for managed instance group %v scaling...", configItem.InstanceGroupName)

				// get request rate with prometheus query
				// https://prometheus-production.travix.com/api/v1/query?query=sum%28rate%28nginx_http_requests_total%7Bhost%21~%22%5E%28%3F%3A%5B0-9.%5D%2B%29%24%22%2Clocation%3D%22%40searchfareapi_gcloud%22%7D%5B10m%5D%29%29%20by%20%28location%29
				prometheusQueryURL := fmt.Sprintf("%v/api/v1/query?query=%v", *prometheusURL, url.QueryEscape(configItem.RequestRateQuery))
				resp, err := pester.Get(prometheusQueryURL)
				if err != nil {
					log.Warn().Err(err).Msgf("Executing prometheus query for mig %v failed", configItem.InstanceGroupName)
					continue
				}

				defer resp.Body.Close()

				body, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Warn().Err(err).Msgf("Reading prometheus query response body for mig %v failed", configItem.InstanceGroupName)
					continue
				}

				queryResponse, err := UnmarshalPrometheusQueryResponse(body)
				if err != nil {
					log.Warn().Err(err).Msgf("Unmarshalling prometheus query response body for mig %v failed", configItem.InstanceGroupName)
					continue
				}

				requestRate, err := queryResponse.GetRequestRate()
				if err != nil {
					log.Warn().Err(err).Msgf("Retrieving request rate from query response body for mig %v failed", configItem.InstanceGroupName)
					continue
				}

				// calculate target # of instances
				targetNumberOfInstances := int(math.Ceil(requestRate / configItem.NumberOfRequestsPerInstance))

				// substract number of instances below target
				minimumNumberOfInstances := targetNumberOfInstances - configItem.NumberOfInstancesBelowTarget

				// ensure minimumNumberOfInstances is larger than MinimumNumberOfInstances from the config
				if minimumNumberOfInstances < configItem.MinimumNumberOfInstances {
					minimumNumberOfInstances = configItem.MinimumNumberOfInstances
				}

				// get actual number of instances
				instanceGroupManager, err := computeService.InstanceGroupManagers.Get(configItem.GCloudProject, configItem.GCloudZone, configItem.InstanceGroupName).Context(ctx).Do()
				if err != nil {
					log.Warn().Err(err).Msgf("Retrieving instance group manager %v failed", configItem.InstanceGroupName)
					continue
				}
				migTargetSize := instanceGroupManager.TargetSize

				log.Info().Msgf("Setting data for managed instance group %v in prometheus (min: %v, actual: %v, source request rate:%v)...", configItem.InstanceGroupName, minimumNumberOfInstances, migTargetSize, requestRate)

				// set prometheus gauge values
				minInstancesVector.WithLabelValues(configItem.InstanceGroupName).Set(float64(minimumNumberOfInstances))
				actualInstancesVector.WithLabelValues(configItem.InstanceGroupName).Set(float64(migTargetSize))
				requestRateVector.WithLabelValues(configItem.InstanceGroupName).Set(requestRate)

				// set min instances on managed instance group
				if configItem.EnableSettingMinInstances {
					// retrieve autoscaler
					autoScaler, err := computeService.Autoscalers.Get(configItem.GCloudProject, configItem.GCloudZone, configItem.InstanceGroupName).Context(ctx).Do()
					if err != nil {
						log.Warn().Err(err).Msgf("Retrieving autoscaler %v failed", configItem.InstanceGroupName)
						continue
					}

					// update autoscaler
					autoScaler.AutoscalingPolicy.MinNumReplicas = int64(minimumNumberOfInstances)
					operation, err := computeService.Autoscalers.Update(configItem.GCloudProject, configItem.GCloudZone, autoScaler).Context(ctx).Do()
					if err != nil {
						log.Warn().Err(err).Msgf("Updating autoscaler %v failed", configItem.InstanceGroupName)
						continue
					}

					log.Info().Interface("operation", *operation).Msgf("Updated autoscaler %v", configItem.InstanceGroupName)
				}
			}

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
