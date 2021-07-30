package connection

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v4"
	ice "github.com/pion/ice/v2"
	log "github.com/sirupsen/logrus"
	"github.com/wiretrustee/wiretrustee/iface"
	"github.com/wiretrustee/wiretrustee/signal"
	sProto "github.com/wiretrustee/wiretrustee/signal/proto"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"sync"
)

// PeerConnectionTimeout is a timeout of an initial connection attempt to a remote peer.
// E.g. this peer will wait PeerConnectionTimeout for the remote peer to respond, if not successful then it will retry the connection attempt.
const PeerConnectionTimeout = 60 * time.Second

// Engine is an instance of the Connection Engine
type Engine struct {
	// a list of STUN and TURN servers
	stunsTurns []*ice.URL
	// signal server client
	signal *signal.Client
	// peer agents indexed by local public key of the remote peers
	conns map[string]*Connection
	// Wireguard interface
	wgIface string
	// Wireguard local address
	wgIP string
	// Network Interfaces to ignore
	iFaceBlackList map[string]struct{}
	// PeerMux is used to sync peer operations (e.g. open connection, peer removal)
	PeerMux *sync.Mutex
}

// Peer is an instance of the Connection Peer
type Peer struct {
	WgPubKey     string
	WgAllowedIps string
}

// NewEngine creates a new Connection Engine
func NewEngine(signal *signal.Client, stunsTurns []*ice.URL, wgIface string, wgAddr string,
	iFaceBlackList map[string]struct{}) *Engine {
	return &Engine{
		stunsTurns:     stunsTurns,
		signal:         signal,
		wgIface:        wgIface,
		wgIP:           wgAddr,
		conns:          map[string]*Connection{},
		iFaceBlackList: iFaceBlackList,
		PeerMux:        &sync.Mutex{},
	}
}

// Start creates a new tunnel interface and listens to signals from the Signal service.
// It also creates an Go routine to handle each peer communication from the config file
func (e *Engine) Start(myKey wgtypes.Key, peers []Peer) error {

	err := iface.Create(e.wgIface, e.wgIP)
	if err != nil {
		log.Errorf("error while creating interface %s: [%s]", e.wgIface, err.Error())
		return err
	}

	err = iface.Configure(e.wgIface, myKey.String())
	if err != nil {
		log.Errorf("error while configuring Wireguard interface [%s]: %s", e.wgIface, err.Error())
		return err
	}

	wgPort, err := iface.GetListenPort(e.wgIface)
	if err != nil {
		log.Errorf("error while getting Wireguard interface port [%s]: %s", e.wgIface, err.Error())
		return err
	}

	e.receiveSignal()

	for _, peer := range peers {
		peer := peer
		go e.InitializePeer(*wgPort, myKey, peer)
	}

	go func() {
		http.HandleFunc("/peer", func(w http.ResponseWriter, r *http.Request) {
			body, err := ioutil.ReadAll(r.Body)
			if err != nil {
				log.Error("%s", err)
				return
			}
			var peer Peer
			err = json.Unmarshal(body, &peer)
			if err != nil {
				log.Error("%s", err)
				return
			}
			go e.InitializePeer(*wgPort, myKey, peer)
		})
		err := http.ListenAndServe("127.0.0.1:7777", nil)
		if err != nil {
			log.Fatal(err)
		}
	}()
	return nil
}

// InitializePeer peer agent attempt to open connection
func (e *Engine) InitializePeer(wgPort int, myKey wgtypes.Key, peer Peer) {
	var backOff = &backoff.ExponentialBackOff{
		InitialInterval:     backoff.DefaultInitialInterval,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          backoff.DefaultMultiplier,
		MaxInterval:         5 * time.Second,
		MaxElapsedTime:      time.Duration(0), //never stop
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}
	operation := func() error {
		_, err := e.openPeerConnection(wgPort, myKey, peer)
		e.PeerMux.Lock()
		defer e.PeerMux.Unlock()
		if _, ok := e.conns[peer.WgPubKey]; !ok {
			log.Infof("removing connection attempt with Peer: %v, not retrying", peer.WgPubKey)
			return nil
		}

		if err != nil {
			log.Warnln(err)
			log.Warnln("retrying connection because of error: ", err.Error())
			return err
		}
		return nil
	}

	err := backoff.Retry(operation, backOff)
	if err != nil {
		// should actually never happen
		panic(err)
	}
}

// RemovePeerConnection closes existing peer connection and removes peer
func (e *Engine) RemovePeerConnection(peer Peer) error {
	e.PeerMux.Lock()
	defer e.PeerMux.Unlock()
	conn, exists := e.conns[peer.WgPubKey]
	if exists && conn != nil {
		delete(e.conns, peer.WgPubKey)
		return conn.Close()
	}
	return nil
}

// GetPeerConnectionStatus returns a connection Status or nil if peer connection wasn't found
func (e *Engine) GetPeerConnectionStatus(peerKey string) *Status {
	e.PeerMux.Lock()
	defer e.PeerMux.Unlock()

	conn, exists := e.conns[peerKey]
	if exists && conn != nil {
		return &conn.Status
	}

	return nil
}

// opens a new peer connection
func (e *Engine) openPeerConnection(wgPort int, myKey wgtypes.Key, peer Peer) (*Connection, error) {
	e.PeerMux.Lock()

	remoteKey, _ := wgtypes.ParseKey(peer.WgPubKey)
	connConfig := &ConnConfig{
		WgListenAddr:   fmt.Sprintf("127.0.0.1:%d", wgPort),
		WgPeerIP:       e.wgIP,
		WgIface:        e.wgIface,
		WgAllowedIPs:   peer.WgAllowedIps,
		WgKey:          myKey,
		RemoteWgKey:    remoteKey,
		StunTurnURLS:   e.stunsTurns,
		iFaceBlackList: e.iFaceBlackList,
	}

	signalOffer := func(uFrag string, pwd string) error {
		return signalAuth(uFrag, pwd, myKey, remoteKey, e.signal, false)
	}

	signalAnswer := func(uFrag string, pwd string) error {
		return signalAuth(uFrag, pwd, myKey, remoteKey, e.signal, true)
	}
	signalCandidate := func(candidate ice.Candidate) error {
		return signalCandidate(candidate, myKey, remoteKey, e.signal)
	}
	conn := NewConnection(*connConfig, signalCandidate, signalOffer, signalAnswer)
	e.conns[remoteKey.String()] = conn
	e.PeerMux.Unlock()

	// blocks until the connection is open (or timeout)
	err := conn.Open(PeerConnectionTimeout)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func signalCandidate(candidate ice.Candidate, myKey wgtypes.Key, remoteKey wgtypes.Key, s *signal.Client) error {
	err := s.Send(&sProto.Message{
		Key:       myKey.PublicKey().String(),
		RemoteKey: remoteKey.String(),
		Body: &sProto.Body{
			Type:    sProto.Body_CANDIDATE,
			Payload: candidate.Marshal(),
		},
	})
	if err != nil {
		log.Errorf("failed signaling candidate to the remote peer %s %s", remoteKey.String(), err)
		//todo ??
		return err
	}

	return nil
}

func signalAuth(uFrag string, pwd string, myKey wgtypes.Key, remoteKey wgtypes.Key, s *signal.Client, isAnswer bool) error {

	var t sProto.Body_Type
	if isAnswer {
		t = sProto.Body_ANSWER
	} else {
		t = sProto.Body_OFFER
	}

	msg, err := signal.MarshalCredential(myKey, remoteKey, &signal.Credential{
		UFrag: uFrag,
		Pwd:   pwd}, t)
	if err != nil {
		return err
	}
	err = s.Send(msg)
	if err != nil {
		return err
	}

	return nil
}

func (e *Engine) receiveSignal() {
	// connect to a stream of messages coming from the signal server
	e.signal.Receive(func(msg *sProto.Message) error {

		conn := e.conns[msg.Key]
		if conn == nil {
			return fmt.Errorf("wrongly addressed message %s", msg.Key)
		}

		if conn.Config.RemoteWgKey.String() != msg.Key {
			return fmt.Errorf("unknown peer %s", msg.Key)
		}

		switch msg.GetBody().Type {
		case sProto.Body_OFFER:
			remoteCred, err := signal.UnMarshalCredential(msg)
			if err != nil {
				return err
			}
			err = conn.OnOffer(IceCredentials{
				uFrag: remoteCred.UFrag,
				pwd:   remoteCred.Pwd,
			})

			if err != nil {
				return err
			}

			return nil
		case sProto.Body_ANSWER:
			remoteCred, err := signal.UnMarshalCredential(msg)
			if err != nil {
				return err
			}
			err = conn.OnAnswer(IceCredentials{
				uFrag: remoteCred.UFrag,
				pwd:   remoteCred.Pwd,
			})

			if err != nil {
				return err
			}

		case sProto.Body_CANDIDATE:

			candidate, err := ice.UnmarshalCandidate(msg.GetBody().Payload)
			if err != nil {
				log.Errorf("failed on parsing remote candidate %s -> %s", candidate, err)
				return err
			}

			err = conn.OnRemoteCandidate(candidate)
			if err != nil {
				log.Errorf("error handling CANDIATE from %s", msg.Key)
				return err
			}
		}

		return nil
	})

	e.signal.WaitConnected()
}
