package messenger

import (
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/thetatoken/theta/common"
	"github.com/thetatoken/theta/crypto"
	p2ptypes "github.com/thetatoken/theta/p2p/types"
	p2pcmn "github.com/thetatoken/theta/p2pl/common"

	"github.com/thetatoken/theta/p2pl/peer"

	"github.com/thetatoken/theta/p2pl"
	"github.com/thetatoken/theta/p2pl/transport"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"

	connmgr "github.com/libp2p/go-libp2p-connmgr"
	pr "github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	cr "github.com/libp2p/go-libp2p-crypto"
	peerstore "github.com/libp2p/go-libp2p-peerstore"

	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	dhtopts "github.com/libp2p/go-libp2p-kad-dht/opts"
	ps "github.com/libp2p/go-libp2p-pubsub"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"

	ds "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	ma "github.com/multiformats/go-multiaddr"
)

var logger *log.Entry = log.WithFields(log.Fields{"prefix": "p2pl"})

//
// Messenger implements the Network interface
//
var _ p2pl.Network = (*Messenger)(nil)

const (
	thetaP2PProtocolPrefix            = "/theta/1.0.0/"
	defaultPeerDiscoveryPulseInterval = 30 * time.Second
	discoverInterval                  = 3000 // 3 sec
	maxNumPeers                       = 64
	sufficientNumPeers                = 32
)

type Messenger struct {
	host          host.Host
	msgHandlerMap map[common.ChannelIDEnum](p2pl.MessageHandler)
	config        MessengerConfig
	seedPeers     []*pr.AddrInfo
	pubsub        *ps.PubSub
	dht           *kaddht.IpfsDHT
	needMdns      bool

	peerTable    *peer.PeerTable
	newPeers     chan pr.ID
	peerDead     chan pr.ID
	// newPeerError chan pr.ID

	// Life cycle
	wg      *sync.WaitGroup
	quit    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
}

//
// MessengerConfig specifies the configuration for Messenger
//
type MessengerConfig struct {
	networkProtocol string
}

// GetDefaultMessengerConfig returns the default config for messenger, not necessary
func GetDefaultMessengerConfig() MessengerConfig {
	return MessengerConfig{
		networkProtocol: "tcp",
	}
}

func createP2PAddr(netAddr, networkProtocol string) (ma.Multiaddr, error) {
	ip, port, err := net.SplitHostPort(netAddr)
	if err != nil {
		return nil, err
	}
	ipv := "ip4"
	if strings.Index(ip, ":") > 0 {
		ipv = "ip6"
	}
	multiAddr, err := ma.NewMultiaddr(fmt.Sprintf("/%v/%v/%v/%v", ipv, ip, networkProtocol, port))
	if err != nil {
		return nil, err
	}
	return multiAddr, nil
}

func getPublicIP() (string, error) {
	ipMap := make(map[string]int)
	numSources := 3
	wait := &sync.WaitGroup{}
	mu := &sync.RWMutex{}

	go func() {
		resp, err := http.Get("http://myexternalip.com/raw")
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				bodyBytes, err := ioutil.ReadAll(resp.Body)
				if err == nil {
					ip := strings.TrimSpace(string(bodyBytes))
					mu.Lock()
					defer mu.Unlock()
					ipMap[ip]++
				}
			}
		}
		wait.Done()
	}()
	wait.Add(1)

	go func() {
		resp, err := http.Get("http://whatismyip.akamai.com")
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				bodyBytes, err := ioutil.ReadAll(resp.Body)
				if err == nil {
					ip := strings.TrimSpace(string(bodyBytes))
					mu.Lock()
					defer mu.Unlock()
					ipMap[ip]++
				}
			}
		}
		wait.Done()
	}()
	wait.Add(1)

	go func() {
		if runtime.GOOS == "windows" {
			cmd := exec.Command("cmd", "/c", "nslookup myip.opendns.com resolver1.opendns.com")
			out, err := cmd.CombinedOutput()
			if err == nil {
				res := strings.TrimSpace(string(out))
				ip := res[strings.LastIndex(res, " ")+1:]
				mu.Lock()
				defer mu.Unlock()
				ipMap[ip]++
			}
		} else {
			cmd := exec.Command("bash", "-c", "dig @resolver1.opendns.com ANY myip.opendns.com +short")
			out, err := cmd.CombinedOutput()
			if err == nil {
				ip := strings.TrimSpace(string(out))
				mu.Lock()
				defer mu.Unlock()
				ipMap[ip]++
			}
		}
		wait.Done()
	}()
	wait.Add(1)

	wait.Wait()

	var majorityIP string
	for ip, cnt := range ipMap {
		if cnt > numSources/2 {
			majorityIP = ip
			break
		}
	}

	if majorityIP == "" {
		return "", fmt.Errorf("Can't get external IP")
	}

	return majorityIP, nil
}

// CreateMessenger creates an instance of Messenger
func CreateMessenger(pubKey *crypto.PublicKey, seedPeerMultiAddresses []string,
	port int, peerDiscoverable bool, msgrConfig MessengerConfig, needMdns bool) (*Messenger, error) {

	ctx, cancel := context.WithCancel(context.Background())

	pt := peer.CreatePeerTable()
	messenger := &Messenger{
		peerTable:         &pt,
		newPeers:          make(chan pr.ID),
		peerDead:          make(chan pr.ID),
		// newPeerError:      make(chan pr.ID),
		msgHandlerMap:     make(map[common.ChannelIDEnum](p2pl.MessageHandler)),
		needMdns:          needMdns,
		config:            msgrConfig,
		wg:                &sync.WaitGroup{},
	}

	hostId, _, err := cr.GenerateEd25519Key(strings.NewReader(common.Bytes2Hex(pubKey.ToBytes())))
	if err != nil {
		return messenger, err
	}
	localNetAddress, err := createP2PAddr(fmt.Sprintf("0.0.0.0:%v", strconv.Itoa(port)), msgrConfig.networkProtocol)
	if err != nil {
		return messenger, err
	}

	var extMultiAddr ma.Multiaddr
	if peerDiscoverable {
		externalIP, err := getPublicIP()
		if err != nil {
			return messenger, err
		}

		extMultiAddr, err = createP2PAddr(fmt.Sprintf("%v:%v", externalIP, strconv.Itoa(port)), msgrConfig.networkProtocol)
		if err != nil {
			return messenger, err
		}
	}

	addressFactory := func(addrs []ma.Multiaddr) []ma.Multiaddr {
		if extMultiAddr != nil {
			addrs = append(addrs, extMultiAddr)
		}
		return addrs
	}

	cm := connmgr.NewConnManager(sufficientNumPeers, maxNumPeers, defaultPeerDiscoveryPulseInterval)
	host, err := libp2p.New(
		ctx,
		libp2p.EnableRelay(),
		libp2p.Identity(hostId),
		libp2p.ListenAddrs([]ma.Multiaddr{localNetAddress}...),
		libp2p.AddrsFactory(addressFactory),
		libp2p.ConnectionManager(cm),
	)
	if err != nil {
		cancel()
		return messenger, err
	}
	messenger.host = host

	// seeds
	for _, seedPeerMultiAddrStr := range seedPeerMultiAddresses {
		addr, err := ma.NewMultiaddr(seedPeerMultiAddrStr)
		if err != nil {
			cancel()
			return messenger, err
		}
		peer, err := peerstore.InfoFromP2pAddr(addr)
		if err != nil {
			cancel()
			return messenger, err
		}
		messenger.seedPeers = append(messenger.seedPeers, peer)
	}

	if peerDiscoverable {
		// kad-dht
		dopts := []dhtopts.Option{
			dhtopts.Datastore(dsync.MutexWrap(ds.NewMapDatastore())),
			dhtopts.Protocols(
				protocol.ID(thetaP2PProtocolPrefix + "dht"),
			),
		}

		dht, err := kaddht.New(ctx, host, dopts...)
		if err != nil {
			cancel()
			return messenger, err
		}
		host = rhost.Wrap(host, dht)
		messenger.dht = dht
	}

	// pubsub
	psOpts := []ps.Option{
		ps.WithMessageSigning(false),
		ps.WithStrictSignatureVerification(false),
	}
	pubsub, err := ps.NewGossipSub(ctx, host, psOpts...)
	if err != nil {
		cancel()
		return messenger, err
	}
	messenger.pubsub = pubsub

	host.Network().Notify((*PeerNotif)(messenger))

	logger.Infof("Created node %v, %v, discoverable: %v", host.ID(), host.Addrs(), peerDiscoverable)
	return messenger, nil
}

func (msgr *Messenger) processLoop(ctx context.Context) {
	defer func() {
		// Clean up go routines.
		allPeers := msgr.peerTable.GetAllPeers()
		for _, peer := range *allPeers {
			peer.Stop()
			msgr.peerTable.DeletePeer(peer.ID())
		}
		msgr.cancel()
	}()

	for {
		select {
		case pid := <-msgr.newPeers:
			pr := msgr.host.Peerstore().PeerInfo(pid)
			if pr == nil {
				continue
			}
			isOutbound := strings.Compare(msgr.host.ID().String(), pid.String()) > 0
			peer := peer.CreatePeer(pr, isOutbound)
			peer.Start(msgr.ctx)
			msgr.attachHandlersToPeer(peer)
			msgr.peerTable.AddPeer(peer)
			go peer.OpenStreams()
			logger.Infof("Peer connected, id: %v, addrs: %v", pr.ID, pr.Addrs)
		case pid := <-msgr.peerDead:
			peer := msgr.peerTable.GetPeer(pid)
			if peer == nil {
				continue
			}

			peer.Stop()

			if msgr.host.Network().Connectedness(pid) == network.Connected {
				// still connected, must be a duplicate connection being closed.
				// we respawn the writer as we need to ensure there is a stream active
				log.Warning("peer declared dead but still connected; respawning writer: ", pid)
				continue
			}

			msgr.peerTable.DeletePeer(pid)
			logger.Infof("Peer disconnected, id: %v, addrs: %v", peer.ID(), peer.Addrs())
		case <-ctx.Done():
			log.Debug("messenger processloop shutting down")
			return
		}
	}
}

// Start is called when the Messenger starts
func (msgr *Messenger) Start(ctx context.Context) error {
	c, cancel := context.WithCancel(ctx)
	msgr.ctx = c
	msgr.cancel = cancel

	// seeds
	perm := rand.Perm(len(msgr.seedPeers))
	for i := 0; i < len(perm); i++ { // create outbound peers in a random order
		msgr.wg.Add(1)
		go func(i int) {
			defer msgr.wg.Done()

			time.Sleep(time.Duration(rand.Int63n(discoverInterval)) * time.Millisecond)
			j := perm[i]
			seedPeer := msgr.seedPeers[j]
			var err error
			for i := 0; i < 3; i++ { // try up to 3 times
				err = msgr.host.Connect(ctx, *seedPeer)
				if err == nil {
					logger.Infof("Successfully connected to seed peer: %v", seedPeer)
					break
				}
				time.Sleep(time.Second * 3)
			}

			if err != nil {
				logger.Errorf("Failed to connect to seed peer %v: %v", seedPeer, err)
			}
		}(i)
	}

	// kad-dht
	if len(msgr.seedPeers) > 0 {
		peerinfo := msgr.seedPeers[0]
		if err := msgr.host.Connect(ctx, *peerinfo); err != nil {
			logger.Errorf("Could not start peer discovery via DHT: %v", err)
		}
	}

	if msgr.dht != nil {
		bcfg := kaddht.DefaultBootstrapConfig
		bcfg.Period = time.Duration(defaultPeerDiscoveryPulseInterval)
		if err := msgr.dht.BootstrapWithConfig(ctx, bcfg); err != nil {
			logger.Errorf("Failed to bootstrap DHT: %v", err)
		}
	}

	// mDns
	// if msgr.needMdns {
	// 	mdnsService, err := discovery.NewMdnsService(ctx, msgr.host, defaultPeerDiscoveryPulseInterval, viper.GetString(common.CfgLibP2PRendezvous))
	// 	if err != nil {
	// 		return err
	// 	}
	// 	mdnsService.RegisterNotifee(&discoveryNotifee{ctx, msgr.host})
	// }

	go msgr.processLoop(ctx)

	return nil
}

// Stop is called when the Messenger stops
func (msgr *Messenger) Stop() {
	msgr.cancel()
}

// Wait suspends the caller goroutine
func (msgr *Messenger) Wait() {
	msgr.wg.Wait()
}

// Publish publishes the given message to all the subscribers
func (msgr *Messenger) Publish(message p2ptypes.Message) error {
	logger.Debugf("Publishing messages...")

	msgHandler := msgr.msgHandlerMap[message.ChannelID]
	bytes, err := msgHandler.EncodeMessage(message.Content)
	if err != nil {
		logger.Errorf("Encoding error: %v", err)
		return err
	}

	err = msgr.pubsub.Publish(strconv.Itoa(int(message.ChannelID)), bytes)
	if err != nil {
		log.Errorf("Failed to publish to gossipsub topic: %v", err)
		return err
	}

	return nil
}

// Broadcast broadcasts the given message to all the connected peers
func (msgr *Messenger) Broadcast(message p2ptypes.Message) (successes chan bool) {
	logger.Debugf("Broadcasting messages...")	

	allPeers := msgr.peerTable.GetAllPeers()
	successes = make(chan bool, msgr.peerTable.GetTotalNumPeers())
	for _, peer := range *allPeers {
		if peer.ID() == msgr.host.ID() {
			continue
		}

		logger.Debugf("Broadcasting \"%v\" to %v", message.Content, peer)
		go func(peerID pr.ID) {
			success := msgr.Send(peerID.String(), message)
			successes <- success
		}(peer.ID())
	}
	return successes
}

// Send sends the given message to the specified peer
func (msgr *Messenger) Send(peerID string, message p2ptypes.Message) bool {
	prID, err := pr.IDB58Decode(peerID)
	if err != nil {
		return false
	}
	peer := msgr.peerTable.GetPeer(prID)
	if peer == nil {
		return false
	}

	success := peer.Send(message.ChannelID, message.Content)
	return success
}

// ID returns the ID of the current node
func (msgr *Messenger) ID() string {
	return string(msgr.host.ID())
}

// RegisterMessageHandler registers the message handler
func (msgr *Messenger) RegisterMessageHandler(msgHandler p2pl.MessageHandler) {
	channelIDs := msgHandler.GetChannelIDs()
	for _, channelID := range channelIDs {
		if msgr.msgHandlerMap[channelID] != nil {
			logger.Errorf("Message handler is already added for channelID: %v", channelID)
			return
		}
		msgr.msgHandlerMap[channelID] = msgHandler

		msgr.registerStreamHandler(channelID)

		sub, err := msgr.pubsub.Subscribe(strconv.Itoa(int(channelID)))
		if err != nil {
			logger.Errorf("Failed to subscribe to channel %v, %v", channelID, err)
			continue
		}
		go func() {
			defer sub.Cancel()

			var msg *ps.Message
			var err error

			for {
				msg, err = sub.Next(context.Background())

				if msgr.ctx != nil && msgr.ctx.Err() != nil {
					logger.Errorf("Context error %v", msgr.ctx.Err())
					return
				}
				if err != nil {
					logger.Errorf("Failed to get next message: %v", err)
					continue
				}

				if msg == nil || msg.GetFrom() == msgr.host.ID() {
					continue
				}

				message, err := msgHandler.ParseMessage(msg.GetFrom().String(), channelID, msg.Data)
				if err != nil {
					logger.Errorf("Failed to parse message, %v", err)
					return
				}

				msgHandler.HandleMessage(message)
			}
		}()
	}
}

func (msgr *Messenger) registerStreamHandler(channelID common.ChannelIDEnum) {
	logger.Debugf("Registered stream handler for channel %v", channelID)
	msgr.host.SetStreamHandler(protocol.ID(thetaP2PProtocolPrefix+strconv.Itoa(int(channelID))), func(strm network.Stream) {
		stream := transport.NewBufferedStream(strm, nil)
		stream.Start(msgr.ctx)

		peerID := strm.Conn().RemotePeer()
		go msgr.readPeerMessageRoutine(stream, peerID.String(), channelID)

		peer := msgr.peerTable.GetPeer(peerID)
		if peer == nil {
			logger.Errorf("Can't find peer %v to accept stream")
		} else {
			peer.AcceptStream(channelID, stream)
		}
	})
}

func (msgr *Messenger) readPeerMessageRoutine(stream *transport.BufferedStream, peerID string, channelID common.ChannelIDEnum) {
	bufferSize := p2pcmn.MaxNormalMessageSize
	if channelID == common.ChannelIDBlock || channelID == common.ChannelIDProposal {
		bufferSize = p2pcmn.MaxBlockMessageSize
	}

	msgBuffer := make([]byte, bufferSize)
	for {
		if msgr.ctx != nil {
			select {
			case <-msgr.ctx.Done():
				return
			default:
			}
		}

		msgSize, err := stream.Read(msgBuffer)
		if err != nil {
			continue
		}

		if msgSize > bufferSize {
			logger.Errorf("Message ignored since it exceeds the peer message size limit, size: %v", msgSize)
			continue
		}

		rawPeerMsg := msgBuffer[:msgSize]

		msgHandler := msgr.msgHandlerMap[channelID]
		message, err := msgHandler.ParseMessage(peerID, channelID, rawPeerMsg)
		if err != nil {
			logger.Errorf("Failed to parse message, %v. msgSize: %v, len(): %v, channel: %v, peer: %v, msg: %v", err, msgSize, len(rawPeerMsg), channelID, peerID, rawPeerMsg)
			return
		}
		msgHandler.HandleMessage(message)
	}
}

// attachHandlersToPeer attaches the registerred message/stream handlers to the given peer
func (msgr *Messenger) attachHandlersToPeer(peer *peer.Peer) {
	messageParser := func(channelID common.ChannelIDEnum, rawMessageBytes common.Bytes) (p2ptypes.Message, error) {
		peerID := peer.ID()
		msgHandler := msgr.msgHandlerMap[channelID]
		if msgHandler == nil {
			logger.Errorf("Failed to setup message parser for channelID %v", channelID)
		}
		message, err := msgHandler.ParseMessage(peerID.String(), channelID, rawMessageBytes)
		return message, err
	}
	peer.SetMessageParser(messageParser)

	messageEncoder := func(channelID common.ChannelIDEnum, message interface{}) (common.Bytes, error) {
		msgHandler := msgr.msgHandlerMap[channelID]
		return msgHandler.EncodeMessage(message)
	}
	peer.SetMessageEncoder(messageEncoder)

	receiveHandler := func(message p2ptypes.Message) error {
		channelID := message.ChannelID
		msgHandler := msgr.msgHandlerMap[channelID]
		if msgHandler == nil {
			logger.Errorf("Failed to setup message handler for peer %v on channelID %v", message.PeerID, channelID)
		}
		err := msgHandler.HandleMessage(message)
		return err
	}
	peer.SetReceiveHandler(receiveHandler)

	// errorHandler := func(interface{}) {
	// 	msgr.discMgr.HandlePeerWithErrors(peer)
	// }
	// peer.SetErrorHandler(errorHandler)

	streamCreator := func(channelID common.ChannelIDEnum) (*transport.BufferedStream, error) {
		strm, err := msgr.host.NewStream(msgr.ctx, peer.ID(), protocol.ID(thetaP2PProtocolPrefix+strconv.Itoa(int(channelID))))
		if err != nil {
			logger.Debugf("Stream open failed: %v. peer: %v, addrs: %v", err, peer.ID(), peer.Addrs())
			return nil, err
		}
		if strm == nil {
			logger.Errorf("Can't open stream. peer: %v, addrs: %v", peer.ID(), peer.Addrs())
			return nil, nil
		}

		stream := transport.NewBufferedStream(strm, nil)
		stream.Start(msgr.ctx)
		go msgr.readPeerMessageRoutine(stream, peer.ID().String(), channelID)
		return stream, nil
	}
	peer.SetStreamCreator(streamCreator)
}