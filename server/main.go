/******************************************************************************
 *
 *  Description :
 *
 *  Setup & initialization.
 *
 *****************************************************************************/

package main

import (
	"encoding/json"
	_ "expvar"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	_ "github.com/tinode/chat/push_fcm"
	_ "github.com/tinode/chat/server/auth_basic"
	_ "github.com/tinode/chat/server/db/rethinkdb"
	"github.com/tinode/chat/server/push"
	_ "github.com/tinode/chat/server/push_stdout"
	"github.com/tinode/chat/server/store"
	"github.com/tinode/chat/server/store/types"
)

const (
	// Terminate session after this timeout.
	IDLETIMEOUT = time.Second * 55
	// Keep topic alive after the last session detached.
	TOPICTIMEOUT = time.Second * 5

	// API version
	VERSION = "0.9"

	DEFAULT_AUTH_ACCESS = types.ModePublic
	DEFAULT_ANON_ACCESS = types.ModeNone
)

// Build timestamp set by the compiler
var buildstamp = ""

var globals struct {
	hub            *Hub
	sessionStore   *SessionStore
	cluster        *Cluster
	apiKeySalt     []byte
	tokenExpiresIn time.Duration
	indexableTags  []string
}

type configType struct {
	Listen     string `json:"listen"`
	Adapter    string `json:"use_adapter"`
	APIKeySalt []byte `json:"api_key_salt"`
	// Security token expiration time
	TokenExpiresIn int `json:"token_expires_in"`
	// Tags allowed in index (user discovery)
	IndexableTags []string        `json:"indexable_tags"`
	ClusterConfig json.RawMessage `json:"cluster_config"`
	StoreConfig   json.RawMessage `json:"store_config"`
	PushConfig    json.RawMessage `json:"push"`
}

func main() {
	log.Printf("Server pid=%d started with processes: %d", os.Getpid(), runtime.GOMAXPROCS(runtime.NumCPU()))

	var configfile = flag.String("config", "./tinode.conf", "Path to config file.")
	// Path to static content.
	var staticPath = flag.String("static_data", "", "Path to /static data for the server.")
	var listenOn = flag.String("listen", "", "Override tinode.conf address and port to listen on.")
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

	var err = store.Open(config.Adapter, string(config.StoreConfig))
	if err != nil {
		log.Fatal("Failed to connect to DB: ", err)
	}
	defer func() {
		store.Close()
		log.Println("Closed database connection(s)")
	}()

	err = push.Init(string(config.PushConfig))
	if err != nil {
		log.Fatal("Failed to initialize push notifications: ", err)
	}
	defer func() {
		push.Stop()
		log.Println("Stopped push notifications")
	}()

	// Keep inactive LP sessions for 15 seconds
	globals.sessionStore = NewSessionStore(IDLETIMEOUT + 15*time.Second)
	// The hub (the main message router)
	globals.hub = newHub()
	// Cluster initialization
	clusterInit(config.ClusterConfig)
	// Expiration time of login tokens
	globals.tokenExpiresIn = time.Duration(config.TokenExpiresIn) * time.Second
	// API key validation secret
	globals.apiKeySalt = config.APIKeySalt
	// Indexable tags for user discovery
	globals.indexableTags = config.IndexableTags

	// Serve static content from the directory in -static_data flag if that's
	// available, if not assume current dir.
	if *staticPath != "" {
		http.Handle("/x/", http.StripPrefix("/x/", http.FileServer(http.Dir(*staticPath))))
	} else {
		path, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		http.Handle("/x/", http.StripPrefix("/x/", http.FileServer(http.Dir(path+"/static"))))
	}

	// Streaming channels
	// Handle websocket clients. WS must come up first, so reconnecting clients won't fall back to LP
	http.HandleFunc("/v0/channels", serveWebSocket)
	// Handle long polling clients
	http.HandleFunc("/v0/channels/lp", serveLongPoll)

	log.Printf("Listening for client HTTP connections on [%s]", config.Listen)
	if err := listenAndServe(config.Listen, signalHandler()); err != nil {
		log.Fatal(err)
	}
	log.Println("All done, good bye")
}

func getApiKey(req *http.Request) string {
	apikey := req.FormValue("apikey")
	if apikey == "" {
		apikey = req.Header.Get("X-Tinode-APIKey")
	}
	return apikey
}
