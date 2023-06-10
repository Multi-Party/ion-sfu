// Package cmd contains an entrypoint for running an ion-sfu instance.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/ion-sfu/cmd/signal/json-rpc/server"
	log "github.com/pion/ion-sfu/pkg/logger"
	"github.com/pion/ion-sfu/pkg/middlewares/datachannel"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sourcegraph/jsonrpc2"
	websocketjsonrpc2 "github.com/sourcegraph/jsonrpc2/websocket"
	"github.com/spf13/viper"
)

// logC need to get logger options from config
type logC struct {
	Config log.GlobalConfig `mapstructure:"log"`
}

var (
	conf           = sfu.Config{}
	file           string
	cert           string
	key            string
	addr           string
	metricsAddr    string
	verbosityLevel int
	logConfig      logC
	logger         = log.New()
)

const (
	portRangeLimit = 100
)

func showHelp() {
	fmt.Printf("Usage:%s {params}\n", os.Args[0])
	fmt.Println("      -c {config file}")
	fmt.Println("      -cert {cert file}")
	fmt.Println("      -key {key file}")
	fmt.Println("      -a {listen addr}")
	fmt.Println("      -h (show help info)")
	fmt.Println("      -v {0-10} (verbosity level, default 0)")
}

func load() bool {
	_, err := os.Stat(file)
	if err != nil {
		return false
	}

	viper.SetConfigFile(file)
	viper.SetConfigType("toml")

	err = viper.ReadInConfig()
	if err != nil {
		logger.Error(err, "config file read failed", "file", file)
		return false
	}
	err = viper.GetViper().Unmarshal(&conf)
	if err != nil {
		logger.Error(err, "sfu config file loaded failed", "file", file)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) > 2 {
		logger.Error(nil, "config file loaded failed. webrtc port must be [min,max]", "file", file)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) != 0 && conf.WebRTC.ICEPortRange[1]-conf.WebRTC.ICEPortRange[0] < portRangeLimit {
		logger.Error(nil, "config file loaded failed. webrtc port must be [min, max] and max - min >= portRangeLimit", "file", file, "portRangeLimit", portRangeLimit)
		return false
	}

	if len(conf.Turn.PortRange) > 2 {
		logger.Error(nil, "config file loaded failed. turn port must be [min,max]", "file", file)
		return false
	}

	if logConfig.Config.V < 0 {
		logger.Error(nil, "Logger V-Level cannot be less than 0")
		return false
	}

	logger.V(0).Info("Config file loaded", "file", file)
	return true
}

func parse() bool {
	flag.StringVar(&file, "c", "config.toml", "config file")
	flag.StringVar(&cert, "cert", "", "cert file")
	flag.StringVar(&key, "key", "", "key file")
	flag.StringVar(&addr, "a", ":7000", "address to use")
	flag.StringVar(&metricsAddr, "m", ":8100", "merics to use")
	flag.IntVar(&verbosityLevel, "v", -1, "verbosity level, higher value - more logs")
	help := flag.Bool("h", false, "help info")
	flag.Parse()
	if !load() {
		return false
	}

	if *help {
		return false
	}
	return true
}

func startMetrics(addr string) {
	// start metrics server
	m := http.NewServeMux()
	m.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Handler: m,
	}

	metricsLis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error(err, "cannot bind to metrics endpoint", "addr", addr)
		os.Exit(1)
	}
	logger.Info("Metrics Listening", "addr", addr)

	err = srv.Serve(metricsLis)
	if err != nil {
		logger.Error(err, "Metrics server stopped")
	}
}

type Stream struct {
	SessionID string   `json:"session_id"`
	NPeers    int      `json:"n_peers"`
	LivePeers []string `json:"live_peers"`
}

func main() {

	if !parse() {
		showHelp()
		os.Exit(-1)
	}

	// Check that the -v is not set (default -1)
	if verbosityLevel < 0 {
		verbosityLevel = logConfig.Config.V
	}

	log.SetGlobalOptions(log.GlobalConfig{V: verbosityLevel})
	logger.Info("--- Starting SFU Node ---")

	// Pass logr instance
	sfu.Logger = logger
	s := sfu.NewSFU(conf)
	dc := s.NewDatachannel(sfu.APIChannelLabel)
	dc.Use(datachannel.SubscriberAPI)

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	// Used with the session ID: /session/ID
	http.Handle("/session/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(path) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			logger.Info("session: bad request wrong number of path elements", "path", path)
			return
		}

		sessionID := path[1]
		session, _ := s.GetSession(sessionID)

		if len(session.Peers()) == 0 {
			w.WriteHeader(http.StatusNotFound)
			logger.Info("session: not found", "path", r.URL.Path)
			return
		}

		livePeers := []string{}
		for _, peer := range session.Peers() {
			pub := peer.Publisher()
			if len(pub.Tracks()) > 0 {
				livePeers = append(livePeers, peer.ID())
			}
		}

		responseSession := Stream{
			SessionID: session.ID(),
			NPeers:    len(session.Peers()),
			LivePeers: livePeers,
		}

		w.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(w)
		err := encoder.Encode(responseSession)
		if err != nil {
			logger.Error(err, "session: error encoding session")
		}
	}))

	// Used with the session ID: /record/ID
	http.Handle("/record/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(path) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			logger.Info("session: bad request wrong number of path elements", "path", path)
			return
		}

		sessionID := path[1]
		session, webrtcConfig := s.GetSession(sessionID)

		pub, err := sfu.NewPublisher("recorder", session, &webrtcConfig)
		if err != nil {
			logger.Error(err, "record: creating publisher failed")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("failed to start recording"))
			return
		}

		pub.OnPublisherTrack(func(track sfu.PublisherTrack) {
			if track.Track.Kind() != webrtc.RTPCodecTypeAudio {
				logger.Info("track is not audio", "track", track.Track.ID(), "kind", track.Track.Kind())
			}

			go func() {
				oggFile, err := oggwriter.New(
					fmt.Sprintf(
						"%s_%s_%s.ogg",
						sessionID,
						track.Receiver.TrackID(),
						time.Now().UTC().Format(time.RFC3339Nano),
					), 48000, 2)
				if err != nil {
					logger.Error(err, "on publisher track: creating audio file failed")
					return
				}
				defer oggFile.Close()

				for {
					packet, _, err := track.Track.ReadRTP()
					if err != nil {
						logger.Error(err, "recording error: reading rtp packet")
						return
					}

					err = oggFile.WriteRTP(packet)
					if err != nil {
						logger.Error(err, "recording error: writing rtp packet")
						return
					}
				}
			}()
		})
	}))

	http.Handle("/sessions", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessions := s.GetSessions()
		responseSessions := make([]Stream, len(sessions))

		for i, session := range sessions {
			livePeers := []string{}
			for _, peer := range session.Peers() {
				pub := peer.Publisher()
				if len(pub.Tracks()) > 0 {
					livePeers = append(livePeers, peer.ID())
				}
			}

			responseSessions[i] = Stream{
				SessionID: session.ID(),
				NPeers:    len(session.Peers()),
				LivePeers: livePeers,
			}
		}

		w.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(w)
		err := encoder.Encode(responseSessions)
		if err != nil {
			logger.Error(err, "sessions: error encoding sessions")
		}
	}))

	http.Handle("/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			panic(err)
		}
		defer c.Close()

		p := server.NewJSONSignal(sfu.NewPeer(s), logger)
		defer p.Close()

		jc := jsonrpc2.NewConn(r.Context(), websocketjsonrpc2.NewObjectStream(c), p)
		<-jc.DisconnectNotify()
	}))

	go startMetrics(metricsAddr)

	var err error
	if key != "" && cert != "" {
		logger.Info("Started listening", "addr", "https://"+addr)
		err = http.ListenAndServeTLS(addr, cert, key, nil)
	} else {
		logger.Info("Started listening", "addr", "http://"+addr)
		err = http.ListenAndServe(addr, nil)
	}
	if err != nil {
		panic(err)
	}
}
