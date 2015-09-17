package main

import (
	"github.com/BurntSushi/toml"
	"github.com/codegangsta/negroni"
	"github.com/emilevauge/traefik/middlewares"
	"github.com/gorilla/mux"
	"github.com/mailgun/oxy/forward"
	"github.com/mailgun/oxy/roundrobin"
	"github.com/op/go-logging"
	"github.com/thoas/stats"
	"github.com/tylerb/graceful"
	"github.com/unrolled/render"
	"gopkg.in/alecthomas/kingpin.v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"
)

var (
	globalConfigFile     = kingpin.Arg("conf", "Main configration file.").Default("traefik.toml").String()
	currentConfiguration = new(Configuration)
	metrics              = stats.New()
	log                  = logging.MustGetLogger("traefik")
	oxyLogger			 = &OxyLogger{}
	templatesRenderer    = render.New(render.Options{
		Directory:  "templates",
		Asset:      Asset,
		AssetNames: AssetNames,
	})
)

type OxyLogger struct{
}

func (oxylogger *OxyLogger) Infof(format string, args ...interface{}) {
	log.Info(format, args...)
}

func (oxylogger *OxyLogger) Warningf(format string, args ...interface{}) {
	log.Warning(format, args...)
}

func (oxylogger *OxyLogger) Errorf(format string, args ...interface{}) {
	log.Error(format, args...)
}

func main() {
	kingpin.Parse()
	var srv *graceful.Server
	var configurationRouter *mux.Router
	var configurationChan = make(chan *Configuration)
	defer close(configurationChan)
	var providers = []Provider{}
	var format = logging.MustStringFormatter("%{color}%{time:15:04:05.000} %{shortfile:20.20s} %{level:8.8s} %{id:03x} ▶%{color:reset} %{message}")
	var sigs = make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// load global configuration
	gloablConfiguration := LoadFileConfig(*globalConfigFile)

	loggerMiddleware := middlewares.NewLogger(gloablConfiguration.AccessLogsFile)
	defer loggerMiddleware.Close()

	// logging
	backends := []logging.Backend{}
	level, err := logging.LogLevel(gloablConfiguration.LogLevel)
	if err != nil {
		log.Fatal("Error getting level", err)
	}

	if len(gloablConfiguration.TraefikLogsFile) > 0 {
		fi, err := os.OpenFile(gloablConfiguration.TraefikLogsFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		defer fi.Close()
		if err != nil {
			log.Fatal("Error opening file", err)
		} else {
			logBackend := logging.NewLogBackend(fi, "", 0)
			logBackendFormatter := logging.NewBackendFormatter(logBackend, logging.GlogFormatter)
			logBackendLeveled := logging.AddModuleLevel(logBackend)
			logBackendLeveled.SetLevel(level, "")
			backends = append(backends, logBackendFormatter)
		}
	}
	if gloablConfiguration.TraefikLogsStdout {
		logBackend := logging.NewLogBackend(os.Stdout, "", 0)
		logBackendFormatter := logging.NewBackendFormatter(logBackend, format)
		logBackendLeveled := logging.AddModuleLevel(logBackend)
		logBackendLeveled.SetLevel(level, "")
		backends = append(backends, logBackendFormatter)
	}
	logging.SetBackend(backends...)

	configurationRouter = LoadDefaultConfig(gloablConfiguration)

	// listen new configurations from providers
	go func() {
		for {
			configuration := <-configurationChan
			log.Info("Configuration receveived %+v", configuration)
			if configuration == nil {
				log.Info("Skipping empty configuration")
			} else if reflect.DeepEqual(currentConfiguration, configuration) {
				log.Info("Skipping same configuration")
			} else {
				currentConfiguration = configuration
				configurationRouter = LoadConfig(configuration, gloablConfiguration)
				srv.Stop(time.Duration(gloablConfiguration.GraceTimeOut) * time.Second)
				time.Sleep(3 * time.Second)
			}
		}
	}()

	// configure providers
	if gloablConfiguration.Docker != nil {
		providers = append(providers, gloablConfiguration.Docker)
	}
	if gloablConfiguration.Marathon != nil {
		providers = append(providers, gloablConfiguration.Marathon)
	}
	if gloablConfiguration.File != nil {
		if len(gloablConfiguration.File.Filename) == 0 {
			// no filename, setting to global config file
			gloablConfiguration.File.Filename = *globalConfigFile
		}
		providers = append(providers, gloablConfiguration.File)
	}
	if gloablConfiguration.Web != nil {
		providers = append(providers, gloablConfiguration.Web)
	}

	// start providers
	for _, provider := range providers {
		log.Notice("Starting provider %v %+v", reflect.TypeOf(provider), provider)
		currentProvider := provider
		go func() {
			currentProvider.Provide(configurationChan)
		}()
	}

	goAway := false
	go func() {
		sig := <-sigs
		log.Notice("I have to go... %+v", sig)
		goAway = true
		srv.Stop(time.Duration(gloablConfiguration.GraceTimeOut) * time.Second)
	}()

	for {
		if goAway {
			break
		}

		// middlewares
		var negroni = negroni.New()
		negroni.Use(metrics)
		negroni.Use(loggerMiddleware)
		//negroni.Use(middlewares.NewRoutes(configurationRouter))
		negroni.UseHandler(configurationRouter)

		srv = &graceful.Server{
			Timeout:          time.Duration(gloablConfiguration.GraceTimeOut) * time.Second,
			NoSignalHandling: true,

			Server: &http.Server{
				Addr:    gloablConfiguration.Port,
				Handler: negroni,
			},
		}

		go func() {
			if len(gloablConfiguration.CertFile) > 0 && len(gloablConfiguration.KeyFile) > 0 {
				srv.ListenAndServeTLS(gloablConfiguration.CertFile, gloablConfiguration.KeyFile)
			} else {
				srv.ListenAndServe()
			}
		}()
		log.Notice("Started")
		<-srv.StopChan()
		log.Notice("Stopped")
	}
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
	//templatesRenderer.HTML(w, http.StatusNotFound, "notFound", nil)
}

func LoadDefaultConfig(gloablConfiguration *GlobalConfiguration) *mux.Router {
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(notFoundHandler)
	return router
}

func LoadConfig(configuration *Configuration, gloablConfiguration *GlobalConfiguration) *mux.Router {
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(notFoundHandler)
	backends := map[string]http.Handler{}
	for frontendName, frontend := range configuration.Frontends {
		log.Debug("Creating frontend %s", frontendName)
		fwd, _ := forward.New()
		newRoute := router.NewRoute().Name(frontendName)
		for routeName, route := range frontend.Routes {
			log.Debug("Creating route %s", routeName)
			newRouteReflect := Invoke(newRoute, route.Rule, route.Value)
			newRoute = newRouteReflect[0].Interface().(*mux.Route)
		}
		if backends[frontend.Backend] == nil {
			log.Debug("Creating backend %s", frontend.Backend)
			lb, _ := roundrobin.New(fwd)
			rb, _ := roundrobin.NewRebalancer(lb, roundrobin.RebalancerLogger(oxyLogger))
			for serverName, server := range configuration.Backends[frontend.Backend].Servers {
				log.Debug("Creating server %s", serverName)
				url, _ := url.Parse(server.Url)
				rb.UpsertServer(url, roundrobin.Weight(server.Weight))
			}
			backends[frontend.Backend] = lb
		} else {
			log.Debug("Reusing backend %s", frontend.Backend)
		}
//		stream.New(backends[frontend.Backend], stream.Retry("IsNetworkError() && Attempts() <= " + strconv.Itoa(gloablConfiguration.Replay)), stream.Logger(oxyLogger))
		newRoute.Handler(backends[frontend.Backend])
		err := newRoute.GetError()
		if err != nil {
			log.Error("Error building route ", err)
		}
	}
	return router
}

func Invoke(any interface{}, name string, args ...interface{}) []reflect.Value {
	inputs := make([]reflect.Value, len(args))
	for i := range args {
		inputs[i] = reflect.ValueOf(args[i])
	}
	return reflect.ValueOf(any).MethodByName(name).Call(inputs)
}

func LoadFileConfig(file string) *GlobalConfiguration {
	configuration := NewGlobalConfiguration()
	if _, err := toml.DecodeFile(file, configuration); err != nil {
		log.Fatal("Error reading file ", err)
	}
	log.Debug("Global configuration loaded %+v", configuration)
	return configuration
}