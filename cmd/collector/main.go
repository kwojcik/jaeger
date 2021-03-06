// Copyright (c) 2017 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/gorilla/mux"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/uber/jaeger-lib/metrics/go-kit"
	"github.com/uber/jaeger-lib/metrics/go-kit/expvar"
	"github.com/uber/tchannel-go"
	"github.com/uber/tchannel-go/thrift"
	"go.uber.org/zap"

	basicB "github.com/uber/jaeger/cmd/builder"
	"github.com/uber/jaeger/cmd/collector/app"
	"github.com/uber/jaeger/cmd/collector/app/builder"
	"github.com/uber/jaeger/cmd/collector/app/zipkin"
	"github.com/uber/jaeger/cmd/flags"
	casFlags "github.com/uber/jaeger/cmd/flags/cassandra"
	esFlags "github.com/uber/jaeger/cmd/flags/es"
	"github.com/uber/jaeger/pkg/config"
	"github.com/uber/jaeger/pkg/healthcheck"
	"github.com/uber/jaeger/pkg/recoveryhandler"
	"github.com/uber/jaeger/pkg/version"
	jc "github.com/uber/jaeger/thrift-gen/jaeger"
	zc "github.com/uber/jaeger/thrift-gen/zipkincore"
)

func main() {
	var signalsChannel = make(chan os.Signal, 0)
	signal.Notify(signalsChannel, os.Interrupt, syscall.SIGTERM)

	logger, _ := zap.NewProduction()
	serviceName := "jaeger-collector"
	casOptions := casFlags.NewOptions("cassandra")
	esOptions := esFlags.NewOptions("es")

	v := viper.New()
	command := &cobra.Command{
		Use:   "jaeger-collector",
		Short: "Jaeger collector receives and processes traces from Jaeger agents and clients",
		Long: `Jaeger collector receives traces from Jaeger agents and agent and runs them through
				a processing pipeline.`,
		Run: func(cmd *cobra.Command, args []string) {
			flags.TryLoadConfigFile(v, logger)

			sFlags := new(flags.SharedFlags).InitFromViper(v)
			casOptions.InitFromViper(v)
			esOptions.InitFromViper(v)

			baseMetrics := xkit.Wrap(serviceName, expvar.NewFactory(10))

			builderOpts := new(builder.CollectorOptions).InitFromViper(v)

			hc, err := healthcheck.Serve(http.StatusServiceUnavailable, builderOpts.CollectorHealthCheckHTTPPort, logger)
			if err != nil {
				logger.Fatal("Could not start the health check server.", zap.Error(err))
			}

			handlerBuilder, err := builder.NewSpanHandlerBuilder(
				builderOpts,
				sFlags,
				basicB.Options.CassandraSessionOption(casOptions.GetPrimary()),
				basicB.Options.ElasticClientOption(esOptions.GetPrimary()),
				basicB.Options.LoggerOption(logger),
				basicB.Options.MetricsFactoryOption(baseMetrics),
			)
			if err != nil {
				logger.Fatal("Unable to set up builder", zap.Error(err))
			}

			ch, err := tchannel.NewChannel(serviceName, &tchannel.ChannelOptions{})
			if err != nil {
				logger.Fatal("Unable to create new TChannel", zap.Error(err))
			}
			server := thrift.NewServer(ch)
			zipkinSpansHandler, jaegerBatchesHandler := handlerBuilder.BuildHandlers()
			server.Register(jc.NewTChanCollectorServer(jaegerBatchesHandler))
			server.Register(zc.NewTChanZipkinCollectorServer(zipkinSpansHandler))

			portStr := ":" + strconv.Itoa(builderOpts.CollectorPort)
			listener, err := net.Listen("tcp", portStr)
			if err != nil {
				logger.Fatal("Unable to start listening on channel", zap.Error(err))
			}
			ch.Serve(listener)

			r := mux.NewRouter()
			apiHandler := app.NewAPIHandler(jaegerBatchesHandler)
			apiHandler.RegisterRoutes(r)
			httpPortStr := ":" + strconv.Itoa(builderOpts.CollectorHTTPPort)
			recoveryHandler := recoveryhandler.NewRecoveryHandler(logger, true)

			go startZipkinHTTPAPI(logger, builderOpts.CollectorZipkinHTTPPort, zipkinSpansHandler, recoveryHandler)

			logger.Info("Starting Jaeger Collector HTTP server", zap.Int("http-port", builderOpts.CollectorHTTPPort))

			go func() {
				if err := http.ListenAndServe(httpPortStr, recoveryHandler(r)); err != nil {
					logger.Fatal("Could not launch service", zap.Error(err))
				}
				hc.Set(http.StatusInternalServerError)
			}()

			hc.Ready()
			select {
			case <-signalsChannel:
				logger.Info("Jaeger Collector is finishing")
			}
		},
	}

	command.AddCommand(version.Command())

	config.AddFlags(
		v,
		command,
		flags.AddConfigFileFlag,
		flags.AddFlags,
		builder.AddFlags,
		casOptions.AddFlags,
		esOptions.AddFlags,
	)

	if error := command.Execute(); error != nil {
		logger.Fatal(error.Error())
	}
}

func startZipkinHTTPAPI(
	logger *zap.Logger,
	zipkinPort int,
	zipkinSpansHandler app.ZipkinSpansHandler,
	recoveryHandler func(http.Handler) http.Handler,
) {
	if zipkinPort != 0 {
		r := mux.NewRouter()
		zipkin.NewAPIHandler(zipkinSpansHandler).RegisterRoutes(r)
		httpPortStr := ":" + strconv.Itoa(zipkinPort)
		logger.Info("Listening for Zipkin HTTP traffic", zap.Int("zipkin.http-port", zipkinPort))

		if err := http.ListenAndServe(httpPortStr, recoveryHandler(r)); err != nil {
			logger.Fatal("Could not launch service", zap.Error(err))
		}
	}
}
