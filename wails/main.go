package main

import (
	"context"
	"embed"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/CtrlSpice/otel-desktop-viewer/desktopexporter"
	"github.com/spf13/cobra"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/processor"
	batchprocessor "go.opentelemetry.io/collector/processor/batchprocessor"
	"go.opentelemetry.io/collector/receiver"
	otlpreceiver "go.opentelemetry.io/collector/receiver/otlpreceiver"

	// This is necessary to avoid ambiguous import error
	// see https://github.com/open-telemetry/opentelemetry-collector/issues/10476
	_ "google.golang.org/genproto/googleapis/type/date"
)

//go:embed all:frontend/dist
var assets embed.FS

var version = "dev" // This will be set by ldflags during build

func main() {
	info := component.BuildInfo{
		Command:     "otel-desktop-viewer",
		Description: "Collector distribution that allows developers to visualize their OTel data locally",
		Version:     version,
	}

	set := otelcol.CollectorSettings{
		BuildInfo: info,
		Factories: components,
		ConfigProviderSettings: otelcol.ConfigProviderSettings{
			ResolverSettings: confmap.ResolverSettings{
				ProviderFactories: []confmap.ProviderFactory{
					envprovider.NewFactory(),
					yamlprovider.NewFactory(),
				},
			},
		},
	}

	go func() {
		if err := runInteractive(set); err != nil {
			log.Fatal(err)
		}

	}()

	router := http.NewServeMux()
	router.HandleFunc("GET /api/traces", redirectUnix())
	router.HandleFunc("GET /api/traces/{id}", redirectUnix())
	router.HandleFunc("GET /api/sampleData", redirectUnix())
	router.HandleFunc("GET /api/clearData", redirectUnix())
	router.HandleFunc("GET /traces/{id}", redirectUnix())

	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:  "otel-desktop-viewer",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: router,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		Bind: []interface{}{
			app,
		},
		EnableDefaultContextMenu: true,
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

func runInteractive(params otelcol.CollectorSettings) error {
	cmd := newCommand(params)
	if err := cmd.Execute(); err != nil {
		log.Fatalf("collector server run finished with error: %v", err)
	}

	return nil
}

func newCommand(set otelcol.CollectorSettings) *cobra.Command {
	var httpPortFlag, grpcPortFlag, browserPortFlag int
	var hostFlag, dbFlag string

	rootCmd := &cobra.Command{
		Use:     set.BuildInfo.Command,
		Version: set.BuildInfo.Version,
		RunE: func(cmd *cobra.Command, args []string) error {
			set.ConfigProviderSettings.ResolverSettings.URIs = []string{
				`yaml:receivers::otlp::protocols::http::endpoint: ` + hostFlag + `:` + strconv.Itoa(httpPortFlag),
				`yaml:receivers::otlp::protocols::grpc::endpoint: ` + hostFlag + `:` + strconv.Itoa(grpcPortFlag),
				`yaml:exporters::desktop::endpoint: ` + hostFlag + `:` + strconv.Itoa(browserPortFlag),
				`yaml:exporters::desktop::db: ` + dbFlag,
				`yaml:service::pipelines::traces::receivers: [otlp]`,
				`yaml:service::pipelines::traces::exporters: [desktop]`,
				`yaml:service::pipelines::metrics::receivers: [otlp]`,
				`yaml:service::pipelines::metrics::exporters: [desktop]`,
				`yaml:service::pipelines::logs::receivers: [otlp]`,
				`yaml:service::pipelines::logs::exporters: [desktop]`,
			}
			set.ConfigProviderSettings.ResolverSettings.DefaultScheme = "env"
			col, err := otelcol.NewCollector(set)
			if err != nil {
				log.Fatal(err)
			}
			return col.Run(cmd.Context())
		},
	}

	rootCmd.Flags().IntVar(&httpPortFlag, "http", 4318, "The port number on which we listen for OTLP http payloads")
	rootCmd.Flags().IntVar(&grpcPortFlag, "grpc", 4317, "The port number on which we listen for OTLP grpc payloads")
	rootCmd.Flags().StringVar(&hostFlag, "host", "localhost", "The host where we expose our all endpoints (OTLP receivers and browser)")
	rootCmd.Flags().IntVar(&browserPortFlag, "browser-port", 8000, "The port number where we expose our data")
	rootCmd.Flags().StringVar(&dbFlag, "db", "", "The path of your database file. Omitting this flag opens DuckDB in in-memory mode, with no data persisted to disk.")
	return rootCmd
}

func components() (otelcol.Factories, error) {
	var err error
	factories := otelcol.Factories{}

	factories.Extensions, err = otelcol.MakeFactoryMap[extension.Factory]()
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ExtensionModules = make(map[component.Type]string, len(factories.Extensions))

	factories.Receivers, err = otelcol.MakeFactoryMap[receiver.Factory](
		otlpreceiver.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ReceiverModules = make(map[component.Type]string, len(factories.Receivers))
	factories.ReceiverModules[otlpreceiver.NewFactory().Type()] = "go.opentelemetry.io/collector/receiver/otlpreceiver v0.125.0"

	factories.Exporters, err = otelcol.MakeFactoryMap[exporter.Factory](
		desktopexporter.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ExporterModules = make(map[component.Type]string, len(factories.Exporters))
	factories.ExporterModules[desktopexporter.NewFactory().Type()] = "github.com/CtrlSpice/otel-desktop-viewer/desktopexporter"

	factories.Processors, err = otelcol.MakeFactoryMap[processor.Factory](
		batchprocessor.NewFactory(),
	)
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ProcessorModules = make(map[component.Type]string, len(factories.Processors))
	factories.ProcessorModules[batchprocessor.NewFactory().Type()] = "go.opentelemetry.io/collector/processor/batchprocessor v0.125.0"

	factories.Connectors, err = otelcol.MakeFactoryMap[connector.Factory]()
	if err != nil {
		return otelcol.Factories{}, err
	}
	factories.ConnectorModules = make(map[component.Type]string, len(factories.Connectors))

	return factories, nil
}

func redirect() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://localhost:8000"+r.URL.Path, nil)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		defer res.Body.Close()

		b, err := io.ReadAll(res.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		if res.StatusCode != http.StatusOK {
			w.WriteHeader(res.StatusCode)
			w.Write(b)
			return
		}

		for k, vv := range res.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}

		w.Write(b)
	}
}

func redirectUnix() func(w http.ResponseWriter, r *http.Request) {
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", "internal.sock")
			},
		},
	}
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://unix"+r.URL.Path, nil)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		res, err := client.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		defer res.Body.Close()

		b, err := io.ReadAll(res.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		if res.StatusCode != http.StatusOK {
			w.WriteHeader(res.StatusCode)
			w.Write(b)
			return
		}

		for k, vv := range res.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}

		w.Write(b)
	}
}
