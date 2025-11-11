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

// Package web prepares router dedicated to ADK REST API for http web server
package web

import (
	"github.com/gorilla/mux"
	"google.golang.org/adk/cmd/launcher/adk"
	"google.golang.org/adk/internal/telemetry"
	"google.golang.org/adk/server/adkrest/controllers"
	"google.golang.org/adk/server/adkrest/internal/routers"
	"google.golang.org/adk/server/adkrest/services"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SetupRouter initiates mux.Router with ADK REST API routers
func SetupRouter(router *mux.Router, routerConfig *adk.Config) *mux.Router {
	adkExporter := services.NewAPIServerSpanExporter()
	telemetry.AddSpanProcessor(sdktrace.NewSimpleSpanProcessor(adkExporter))

	// TODO: Allow taking a prefix to allow customizing the path
	// where the ADK REST API will be served.
	return setupRouter(router,
		routers.NewSessionsAPIRouter(controllers.NewSessionsAPIController(routerConfig.SessionService)),
		routers.NewRuntimeAPIRouter(controllers.NewRuntimeAPIRouter(routerConfig.SessionService, routerConfig.AgentLoader, routerConfig.ArtifactService)),
		routers.NewAppsAPIRouter(controllers.NewAppsAPIController(routerConfig.AgentLoader)),
		routers.NewDebugAPIRouter(controllers.NewDebugAPIController(routerConfig.SessionService, routerConfig.AgentLoader, adkExporter)),
		routers.NewArtifactsAPIRouter(controllers.NewArtifactsAPIController(routerConfig.ArtifactService)),
		&routers.EvalAPIRouter{},
	)
}

func setupRouter(router *mux.Router, subrouters ...routers.Router) *mux.Router {
	routers.SetupSubRouters(router, subrouters...)
	return router
}
