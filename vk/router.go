package vk

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/julienschmidt/httprouter"

	"github.com/suborbital/vektor/vlog"
)

const contentTypeHeaderKey = "Content-Type"

// used internally to convey content types
type contentType string

// HandlerFunc is the vk version of http.HandlerFunc
// instead of exposing the ResponseWriter, the function instead returns
// an object and an error, which are handled as described in `With` below
type HandlerFunc func(w http.ResponseWriter, r *http.Request, ctx *Ctx) (interface{}, error)

// WebSocketHandlerFunc is the vk version of http.HandlerFunc, but
// specifically for websockets. Instead of exposing the ResponseWriter,
// the function exposes a Gorilla `Conn`.
type WebSocketHandlerFunc func(*http.Request, *Ctx, *websocket.Conn) error

// Router handles the responses on behalf of the server
type Router struct {
	*RouteGroup                    // the "root" RouteGroup that is mounted at server start
	hrouter     *httprouter.Router // the internal 'actual' router

	fallbackProxy *httputil.ReverseProxy
	quietRoutes   map[string]bool
	finalizeOnce  sync.Once // ensure that the root only gets mounted once

	log *vlog.Logger
}

type defaultScope struct {
	RequestID string `json:"request_id"`
}

// NewRouter creates a new Router
func NewRouter(logger *vlog.Logger, fallback string) *Router {
	var proxy *httputil.ReverseProxy

	if fallback != "" {
		proxyURL, _ := url.Parse(fallback)
		if proxyURL != nil {
			proxy = httputil.NewSingleHostReverseProxy(proxyURL)
		}
	}

	r := &Router{
		RouteGroup:    Group(""),
		hrouter:       httprouter.New(),
		fallbackProxy: proxy,
		quietRoutes:   map[string]bool{},
		finalizeOnce:  sync.Once{},
		log:           logger,
	}

	return r
}

// HandleHTTP handles a classic Go HTTP handlerFunc
func (rt *Router) HandleHTTP(method, path string, handler http.HandlerFunc) {
	rt.hrouter.Handle(method, path, func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		handler(w, r)
	})
}

// Finalize mounts the root group to prepare the Router to handle requests
func (rt *Router) Finalize() {
	rt.finalizeOnce.Do(func() {
		rt.mountGroup(rt.RouteGroup)
	})
}

// ServeHTTP serves HTTP requests
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// check to see if the router has a handler for this path
	handler, params, _ := rt.hrouter.Lookup(r.Method, r.URL.Path)

	if handler != nil {
		handler(w, r, params)
	} else {
		if rt.fallbackProxy != nil {
			rt.fallbackProxy.ServeHTTP(w, r)
			return
		}

		rt.log.Debug("not handled:", r.Method, r.URL.String())

		// let httprouter handle the fallthrough cases
		rt.hrouter.ServeHTTP(w, r)
	}
}

// mountGroup adds a group of handlers to the httprouter
func (rt *Router) mountGroup(group *RouteGroup) {
	for _, r := range group.httpRouteHandlers() {
		rt.log.Debug("mounting route", r.Method, r.Path)
		rt.hrouter.Handle(r.Method, r.Path, rt.httpHandlerWrap(r.Handler))
	}
}

// httpHandlerWrap returns an httprouter.Handle that uses the `inner` vk.HandleFunc to handle the request
//
// inner returns a body and an error;
// the body can can be:
// - a vk.Response object (status and body are written to w)
// - []byte (written directly to w, status 200)
// - a struct (marshalled to JSON and written to w, status 200)
//
// the error can be:
// - a vk.Error type (status and message are written to w)
// - any other error object (status 500 and error.Error() are written to w)
//
func (rt *Router) httpHandlerWrap(inner HandlerFunc) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		var status int
		var body []byte
		var detectedCType contentType

		// create a context handleWrap the configured logger
		// (and use the ctx.Log for all remaining logging
		// in case a scope was set on it)
		ctx := NewCtx(rt.log, params, w.Header())
		ctx.UseScope(defaultScope{ctx.RequestID()})

		logDone := rt.logRequest(r, ctx)

		resp, err := inner(w, r, ctx)
		if err != nil {
			status, body, detectedCType = errorOrOtherToBytes(ctx.Log, err)
		} else {
			status, body, detectedCType = responseOrOtherToBytes(ctx.Log, resp)
		}

		// check if anything in the handler chain set the content type
		// header, and only use the auto-detected value if it wasn't
		headerCType := w.Header().Get(contentTypeHeaderKey)
		shouldSetCType := headerCType == ""

		ctx.Log.Debug("post-handler contenttype:", string(headerCType))

		// if no contentType was set in the middleware chain,
		// then set it here based on the type detected
		if shouldSetCType {
			ctx.Log.Debug("setting auto-detected contenttype:", string(detectedCType))
			w.Header().Set(contentTypeHeaderKey, string(detectedCType))
		}

		w.WriteHeader(status)
		w.Write(body)

		logDone(status)
	}
}

// wsHandlerWrap returns a HandlerFunc that wraps an inner WebSocketHandlerFunc. The purpose of this is to still provide
// a convenience way of writing a websocket connection, but any and all websocket handlers are still ultimately http
// handler functions.
//
// If you choose you can use a HandlerFunc and do the connection upgrade in the handler func directly rather than wrap
// one in this one that does it for you.
//
// inner accepts a Gorilla `Conn` and reads and writes messages to it
//
func (rt *Router) wsHandlerWrap(inner WebSocketHandlerFunc) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, ctx *Ctx) (interface{}, error) {
		upgrader := websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// Vektor accepts all origins—middleware should be used to
			// check origins
			CheckOrigin: func(r *http.Request) bool { return true },
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			status, body, _ := errorOrOtherToBytes(ctx.Log, err)

			return nil, E(status, string(body))
		}

		return nil, inner(r, ctx, conn)
	}
}

// canHandle returns true if there's a registered handler that can
// handle the method and path provided or not
func (rt *Router) canHandle(method, path string) bool {
	handler, _, _ := rt.hrouter.Lookup(method, path)
	return handler != nil
}

// useQuietRoutes sets the 'quiet' routes for the router's logging
func (rt *Router) useQuietRoutes(routes []string) {
	for _, r := range routes {
		rt.quietRoutes[r] = true
	}
}

// logRequest logs a request and returns a function
// that logs the completion of the request handler
func (rt *Router) logRequest(r *http.Request, ctx *Ctx) func(int) {
	start := time.Now()

	logFn := ctx.Log.Info
	if _, beQuiet := rt.quietRoutes[r.URL.Path]; beQuiet {
		logFn = ctx.Log.Debug
	}

	logFn(r.Method, r.URL.String())

	logDone := func(status int) {
		logFn(r.Method, r.URL.String(), fmt.Sprintf("completed (%d: %s) in %dms", status, http.StatusText(status), time.Since(start).Milliseconds()))
	}

	return logDone
}
