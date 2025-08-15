package server

import (
	"embed"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/CtrlSpice/otel-desktop-viewer/desktopexporter/internal/store"
	"github.com/rs/cors"
	"golang.org/x/exp/jsonrpc2"
)

var (
	//go:embed static/*
	assets     embed.FS
	socketFile = "internal.sock"
)

type Server struct {
	listener       net.Listener
	server         http.Server
	jsonrpcHandler *JSONRPCHandler
	staticDir      string
}

func NewServer(endpoint string, store *store.Store) *Server {
	s := Server{
		server: http.Server{
			Addr: endpoint,
		},
		jsonrpcHandler: NewJSONRPCHandler(store),
		staticDir:      getStaticDir(),
	}

	if err := s.initHandler(); err != nil {
		log.Fatalf("Could not initialize desktop exporter server: %v", err)
	}

	return &s
}

func (s *Server) Start() error {
	// attempt to cleanup socket before continuing
	if _, err := os.Stat(socketFile); err == nil {
		if err := os.Remove(socketFile); err != nil {
			return err
		}
	}

	var err error
	s.listener, err = net.Listen("unix", socketFile)
	if err != nil {
		return err
	}

	return s.server.Serve(s.listener)
}

func (s *Server) initHandler() error {
	mux := http.NewServeMux()

	// Handle specific routes first (checked before catch-all)
	mux.HandleFunc("GET /traces/{id}", s.indexHandler)
	mux.HandleFunc("POST /rpc", s.rpcHandler)

	// Then handle static files (catches everything else)
	if s.staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.staticDir)))
	} else {
		staticContent, err := fs.Sub(assets, "static")
		if err != nil {
			log.Fatal(err)
		}
		mux.Handle("/", http.FileServerFS(staticContent))
	}

	// CORS for the Vite frontend
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"http://*", "https://*"},
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders: []string{"*"},
	})
	s.server.Handler = c.Handler(mux)
	return nil
}

// indexHandler serves the frontend application
// It handles both development (staticDir) and production (embedded assets) scenarios
func (s *Server) indexHandler(writer http.ResponseWriter, request *http.Request) {
	if s.staticDir != "" {
		http.ServeFile(writer, request, s.staticDir+"/index.html")
	} else {
		bytes, err := assets.ReadFile("static/index.html")
		if err != nil {
			log.Printf("Error reading static assets: %v", err)
			http.Error(writer, "Failed to load page", http.StatusInternalServerError)
			return
		}
		writer.Write(bytes)
	}
}

func (s *Server) rpcHandler(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		sendJSONRPCResponse(writer, jsonrpc2.ID{}, nil, jsonrpc2.ErrInternal)
		return
	}

	message, err := jsonrpc2.DecodeMessage(body)
	if err != nil {
		sendJSONRPCResponse(writer, jsonrpc2.ID{}, nil, jsonrpc2.ErrParse)
		return
	}

	rpcRequest, ok := message.(*jsonrpc2.Request)
	if !ok {
		sendJSONRPCResponse(writer, jsonrpc2.ID{}, nil, jsonrpc2.ErrInvalidRequest)
		return
	}

	result, err := s.jsonrpcHandler.Handle(request.Context(), rpcRequest)
	if err != nil {
		sendJSONRPCResponse(writer, rpcRequest.ID, nil, err)
		return
	}

	sendJSONRPCResponse(writer, rpcRequest.ID, result, nil)
}

// sendJSONRPCResponse encodes and writes a spec-compliant JSON-RPC response body
// It handles both success and error cases, with fallback http response
// if our error handling creates new and exciting errors
func sendJSONRPCResponse(writer http.ResponseWriter, id jsonrpc2.ID, result any, rpcError error) {
	response, err := jsonrpc2.NewResponse(id, result, rpcError)
	if err != nil {
		log.Printf("Error creating JSON-RPC response: %v", err)
		http.Error(writer, "Internal server error", http.StatusInternalServerError)
		return
	}

	bytes, err := jsonrpc2.EncodeMessage(response)
	if err != nil {
		log.Printf("Error encoding JSON-RPC response: %v", err)
		http.Error(writer, "Internal server error", http.StatusInternalServerError)
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.Write(bytes)
}

// getStaticDir returns the path to the static assets directory we use for development
// If the environment variable is not set, it returns an empty string
func getStaticDir() string {
	staticDir, ok := os.LookupEnv("STATIC_ASSETS_DIR")
	if ok {
		return filepath.Clean(staticDir)
	}

	return ""
}

func (s *Server) Close() error {
	defer func() {
		s.listener.Close()
		if _, err := os.Stat(socketFile); err == nil {
			if err := os.Remove(socketFile); err != nil {
				log.Printf("Encountered error removing socket: %s\n", err)
			}
		}
	}()
	return s.server.Close()
}
