/******************************************************************************
 *
 *  Description :
 *
 *  Setup & initialization.
 *
 *****************************************************************************/

package main

//go:generate protoc --proto_path=../pbx --go_out=plugins=grpc:../pbx ../pbx/model.proto

import (
	"encoding/json"
	_ "expvar"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "github.com/tinode/chat/push_fcm"
	_ "github.com/tinode/chat/server/auth_basic"
	_ "github.com/tinode/chat/server/db/rethinkdb"
	"github.com/tinode/chat/server/push"
	_ "github.com/tinode/chat/server/push_stdout"
	"github.com/tinode/chat/server/store"
	"google.golang.org/grpc"
)

const (
	// idleSessionTimeout defines duration of being idle before terminating a session.
	idleSessionTimeout = time.Second * 55
	// idleTopicTimeout defines now long to keep topic alive after the last session detached.
	idleTopicTimeout = time.Second * 5

	// currentVersion is the current API version
	currentVersion = "0.14"
	// minSupportedVersion is the minimum supported API version
	minSupportedVersion = "0.14"

	// maxMessageSize is the default maximum message size
	maxMessageSize = 1 << 19 // 512K

	// Delay before updating a User Agent
	uaTimerDelay = time.Second * 5

	// maxDeleteCount is the maximum allowed number of messages to delete in one call.
	maxDeleteCount = 1024
)

// Build timestamp set by the compiler
var buildstamp = ""

var globals struct {
	hub           *Hub
	sessionStore  *SessionStore
	cluster       *Cluster
	grpcServer    *grpc.Server
	apiKeySalt    []byte
	indexableTags []string
	// Add Strict-Transport-Security to headers, the value signifies age.
	// Empty string "" turns it off
	tlsStrictMaxAge string
	// Maximum message size allowed from peer.
	maxMessageSize int64
}

// Contentx of the configuration file
type configType struct {
	// Default HTTP(S) address:port to listen on for websocket and long polling clients. Either a
	// numeric or a canonical name, e.g. ":80" or ":https". Could include a host name, e.g.
	// "localhost:80".
	// Could be blank: if TLS is not configured, will use ":80", otherwise ":443".
	// Can be overridden from the command line, see option --listen.
	Listen string `json:"listen"`
	// Address:port to listen for gRPC clients. If blank gRPC support will not be initialized.
	// Could be overridden from the command line with --grpc_listen.
	GrpcListen string `json:"grpc_listen"`
	// Path for mounting the directory with static files.
	StaticMount string `json:"static_mount"`
	// Salt used in signing API keys
	APIKeySalt []byte `json:"api_key_salt"`
	// Maximum message size allowed from client. Intended to prevent malicious client from sending
	// very large files.
	MaxMessageSize int `json:"max_message_size"`
	// Tags allowed in index (user discovery)
	IndexableTags []string                   `json:"indexable_tags"`
	ClusterConfig json.RawMessage            `json:"cluster_config"`
	PluginConfig  json.RawMessage            `json:"plugins"`
	StoreConfig   json.RawMessage            `json:"store_config"`
	PushConfig    json.RawMessage            `json:"push"`
	TLSConfig     json.RawMessage            `json:"tls"`
	AuthConfig    map[string]json.RawMessage `json:"auth_config"`
}

func main() {
	log.Printf("Server v%s:%s pid=%d started with processes: %d", currentVersion, buildstamp, os.Getpid(),
		runtime.GOMAXPROCS(runtime.NumCPU()))

	var configfile = flag.String("config", "./tinode.conf", "Path to config file.")
	// Path to static content.
	var staticPath = flag.String("static_data", "", "Path to /static data for the server.")
	var listenOn = flag.String("listen", "", "Override address and port to listen on for HTTP(S) clients.")
	var listenGrpc = flag.String("grpc_listen", "", "Override address and port to listen on for gRPC clients.")
	var tlsEnabled = flag.Bool("tls_enabled", false, "Override config value for enabling TLS")
	var clusterSelf = flag.String("cluster_self", "", "Override the name of the current cluster node")
	flag.Parse()

	log.Printf("Using config from: '%s'", *configfile)

	var config configType
	if raw, err := ioutil.ReadFile(*configfile); err != nil {
		log.Fatal(err)
	} else if err = json.Unmarshal(raw, &config); err != nil {
		log.Fatal(err)
	}

	if *listenOn != "" {
		config.Listen = *listenOn
	}

	var err = store.Open(string(config.StoreConfig))
	if err != nil {
		log.Fatal("Failed to connect to DB: ", err)
	}
	defer func() {
		store.Close()
		log.Println("Closed database connection(s)")
		log.Println("All done, good bye")
	}()

	for name, jsconf := range config.AuthConfig {
		if authhdl := store.GetAuthHandler(name); authhdl == nil {
			panic("Config provided for unknown authentication scheme '" + name + "'")
		} else if err := authhdl.Init(string(jsconf)); err != nil {
			panic(err)
		}
	}

	err = push.Init(string(config.PushConfig))
	if err != nil {
		log.Fatal("Failed to initialize push notifications: ", err)
	}
	defer func() {
		push.Stop()
		log.Println("Stopped push notifications")
	}()

	// Keep inactive LP sessions for 15 seconds
	globals.sessionStore = NewSessionStore(idleSessionTimeout + 15*time.Second)
	// The hub (the main message router)
	globals.hub = newHub()
	// Cluster initialization
	clusterInit(config.ClusterConfig, clusterSelf)
	// Intialize plugins
	pluginsInit(config.PluginConfig)
	// API key validation secret
	globals.apiKeySalt = config.APIKeySalt
	// Indexable tags for user discovery
	globals.indexableTags = config.IndexableTags
	// Maximum message size
	globals.maxMessageSize = int64(config.MaxMessageSize)
	if globals.maxMessageSize <= 0 {
		globals.maxMessageSize = maxMessageSize
	}

	// Serve static content from the directory in -static_data flag if that's
	// available, otherwise assume '<current dir>/static'. The content is served at
	// the path pointed by 'static_mount' in the config. If that is missing then it's
	// served at '/x/'.
	var staticContent = *staticPath
	if staticContent == "" {
		path, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		staticContent = path + "/static/"
	}
	staticMountPoint := config.StaticMount
	if staticMountPoint == "" {
		staticMountPoint = "/x/"
	} else {
		if !strings.HasPrefix(staticMountPoint, "/") {
			staticMountPoint = "/" + staticMountPoint
		}
		if !strings.HasSuffix(staticMountPoint, "/") {
			staticMountPoint = staticMountPoint + "/"
		}
	}
	http.Handle(staticMountPoint, http.StripPrefix(staticMountPoint,
		hstsHandler(http.FileServer(http.Dir(staticContent)))))
	log.Printf("Serving static content from '%s' at '%s'", staticContent, staticMountPoint)

	// Configure HTTP channels
	// Handle websocket clients.
	http.HandleFunc("/v0/channels", serveWebSocket)
	// Handle long polling clients.
	http.HandleFunc("/v0/channels/lp", serveLongPoll)
	// Serve json-formatted 404 for all other URLs
	http.HandleFunc("/", serve404)

	// Set up gRPC server, if one is configured
	if *listenGrpc == "" {
		*listenGrpc = config.GrpcListen
	}
	if srv, err := serveGrpc(*listenGrpc); err != nil {
		log.Fatal(err)
	} else {
		globals.grpcServer = srv
	}

	if err := listenAndServe(config.Listen, *tlsEnabled, string(config.TLSConfig), signalHandler()); err != nil {
		log.Fatal(err)
	}
}

func getAPIKey(req *http.Request) string {
	apikey := req.FormValue("apikey")
	if apikey == "" {
		apikey = req.Header.Get("X-Tinode-APIKey")
	}
	return apikey
}

// Parses version of format 0.13.xx or 0.13-xx or 0.13
// The major and minor parts must be valid, the last part is ignored if missing or unparceable.
func parseVersion(vers string) int {
	var major, minor, trailer int
	var err error

	dot := strings.Index(vers, ".")
	if dot >= 0 {
		major, err = strconv.Atoi(vers[:dot])
	} else {
		major, err = strconv.Atoi(vers)
	}
	if err != nil {
		return 0
	}

	dot2 := strings.IndexAny(vers[dot+1:], ".-")
	if dot2 > 0 {
		minor, err = strconv.Atoi(vers[dot+1 : dot2])
		// Ignoring the error here
		trailer, _ = strconv.Atoi(vers[dot2+1:])
	} else {
		minor, err = strconv.Atoi(vers[dot+1:])
	}
	if err != nil {
		return 0
	}

	if major < 0 || minor < 0 || trailer < 0 || minor >= 0xff || trailer >= 0xff {
		return 0
	}

	return (major << 16) | (minor << 8) | trailer
}

func versionToString(vers int) string {
	str := strconv.Itoa(vers>>16) + "." + strconv.Itoa((vers>>8)&0xff)
	if vers&0xff != 0 {
		str += "-" + strconv.Itoa(vers&0xff)
	}
	return str
}

// Returns > 0 if v1 > v2; zero if equal; < 0 if v1 < v2
// Only Major and Minor parts are compared, the trailer is ignored.
func versionCompare(v1, v2 int) int {
	return (v1 >> 8) - (v2 >> 8)
}
