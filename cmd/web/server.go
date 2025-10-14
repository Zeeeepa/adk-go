// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// package web provides an ability to parse command line flags and easily run server for both ADK WEB UI and ADK REST API
package web

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/a2aproject/a2a-go/a2agrpc"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/gorilla/mux"
	"google.golang.org/adk/artifact"
	"google.golang.org/adk/cmd/restapi/config"
	"google.golang.org/adk/cmd/restapi/handlers"
	"google.golang.org/adk/cmd/restapi/services"
	restapiweb "google.golang.org/adk/cmd/restapi/web"
	"google.golang.org/adk/session"
)

// WebConfig is a struct with parameters to run a WebServer.
type WebConfig struct {
	LocalPort       int
	FrontendAddress string
	BackendAddress  string
}

// ParseArgs parses the arguments for the ADK API server.
func ParseArgs() *WebConfig {
	localPortFlag := flag.Int("port", 8080, "Localhost port for the server")
	frontendAddressFlag := flag.String("front_address", "localhost:8080", "Front address to allow CORS requests from as seen from the user browser. Please specify only hostname and (optionally) port")
	backendAddressFlag := flag.String("backend_address", "http://localhost:8080/api", "Backend server as seen from the user browser. Please specify the whole URL, i.e. 'http://localhost:8080/api'. ")

	flag.Parse()
	if !flag.Parsed() {
		flag.Usage()
		panic("Failed to parse flags")
	}
	return &(WebConfig{
		LocalPort:       *localPortFlag,
		FrontendAddress: *frontendAddressFlag,
		BackendAddress:  *backendAddressFlag,
	})
}

func Logger(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		inner.ServeHTTP(w, r)

		log.Printf(
			"%s %s %s",
			r.Method,
			r.RequestURI,
			time.Since(start),
		)
	})
}

type ServeConfig struct {
	SessionService  session.Service
	AgentLoader     services.AgentLoader
	ArtifactService artifact.Service
}

func corsWithArgs(c *WebConfig) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", c.FrontendAddress)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// embed web UI files into the executable

//go:embed distr/*
var content embed.FS

// Serve initiates the http server and starts it according to WebConfig parameters
func Serve(c *WebConfig, serveConfig *ServeConfig) {
	serverConfig := config.ADKAPIRouterConfigs{
		SessionService:  serveConfig.SessionService,
		AgentLoader:     serveConfig.AgentLoader,
		ArtifactService: serveConfig.ArtifactService,
	}

	rBase := mux.NewRouter().StrictSlash(true)
	rBase.Use(Logger)

	// Setup serving of ADK Web UI
	rUi := rBase.Methods("GET").PathPrefix("/ui/").Subrouter()

	//   generate /assets/config/runtime-config.json in the runtime.
	//   It removes the need to prepare this file during deployment and update the distribution files.
	runtimeConfigResponse := struct {
		BackendUrl string `json:"backendUrl"`
	}{BackendUrl: c.BackendAddress}
	rUi.Methods("GET").Path("/assets/config/runtime-config.json").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.EncodeJSONResponse(runtimeConfigResponse, http.StatusOK, w)
	})

	//   redirect the user from / to /ui/
	rBase.Methods("GET").Path("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	// serve web ui from the embedded resources
	ui, err := fs.Sub(content, "distr")
	if err != nil {
		log.Fatalf("cannot prepare ADK Web UI files as embedded content: %v", err)
	}
	rUi.Methods("GET").Handler(http.StripPrefix("/ui/", http.FileServer(http.FS(ui))))

	// Setup serving of ADK REST API
	rApi := rBase.Methods("GET", "POST", "DELETE", "OPTIONS").PathPrefix("/api/").Subrouter()
	rApi.Use(corsWithArgs(c))
	restapiweb.SetupRouter(rApi, &serverConfig)

	var handler http.Handler
	if c.StartA2A {
		grpcSrv := grpc.NewServer()
		newA2AHandler(serveConfig).RegisterWith(grpcSrv)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				grpcSrv.ServeHTTP(w, r)
			} else {
				rBase.ServeHTTP(w, r)
			}
		})
	} else {
		handler = rBase
	}

	handler = h2c.NewHandler(handler, &http2.Server{})

	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(c.LocalPort), handler))
}

func newA2AHandler(serveConfig *ServeConfig) *a2agrpc.GRPCHandler {
	agent := serveConfig.AgentLoader.Root()
	executor := adka2a.NewExecutor(&adka2a.ExecutorConfig{
		AppName:         agent.Name(),
		Agent:           agent,
		SessionService:  serveConfig.SessionService,
		ArtifactService: serveConfig.ArtifactService,
	})
	reqHandler := a2asrv.NewHandler(executor, serveConfig.A2AOptions...)
	grpcHandler := a2agrpc.NewHandler(&adka2a.CardProducer{Agent: agent}, reqHandler)
	return grpcHandler
}
