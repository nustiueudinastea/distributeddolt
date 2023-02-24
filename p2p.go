package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	connmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
	noise "github.com/libp2p/go-libp2p/p2p/security/noise"
	cmap "github.com/orcaman/concurrent-map"
	"github.com/pkg/errors"
	"github.com/segmentio/ksuid"
)

type rpcMsgType string
type pubsubMsgType string

const (
	protosRPCProtocol             = "/protos/rpc/0.0.1"
	protosUpdatesTopic            = "/protos/updates/0.0.1"
	rpcRequest         rpcMsgType = "request"
	rpcResponse        rpcMsgType = "response"
)

type rpcMsgProcessor struct {
	WriteQueue chan rpcMsg
	Stop       context.CancelFunc
}

// type rpcPeer struct {
// 	mu      sync.Mutex
// 	machine Machine
// 	client  *Client
// }

// func (peer *rpcPeer) GetClient() *Client {
// 	peer.mu.Lock()
// 	defer peer.mu.Unlock()
// 	return peer.client
// }

// func (peer *rpcPeer) SetClient(client *Client) {
// 	peer.mu.Lock()
// 	defer peer.mu.Unlock()
// 	peer.client = client
// }

// func (peer *rpcPeer) GetMachine() Machine {
// 	peer.mu.Lock()
// 	defer peer.mu.Unlock()
// 	return peer.machine
// }

// func (peer *rpcPeer) SetMachine(machine Machine) {
// 	peer.mu.Lock()
// 	defer peer.mu.Unlock()
// 	peer.machine = machine
// }

// type emptyReq struct{}
// type emptyResp struct{}

type rpcHandler struct {
	Func          func(peer peer.ID, data interface{}) (interface{}, error)
	RequestStruct interface{}
}

type pubsubHandler struct {
	Func          func(peer peer.ID, data interface{}) error
	PayloadStruct interface{}
}

type pubsubMsg struct {
	ID      string
	Type    pubsubMsgType
	Payload json.RawMessage
}

type rpcMsg struct {
	ID      string
	Type    rpcMsgType
	Payload json.RawMessage
}

type rpcPayloadRequest struct {
	Type string
	Data json.RawMessage
}

type rpcPayloadResponse struct {
	Error string
	Data  json.RawMessage
}

type requestTracker struct {
	resp      chan []byte
	err       chan error
	closeSig  chan interface{}
	startTime time.Time
}

type P2P struct {
	*PubSubClient

	host             host.Host
	rpcHandlers      map[string]*rpcHandler
	pubsubHandlers   map[pubsubMsgType]*pubsubHandler
	reqs             cmap.ConcurrentMap
	rpcMsgProcessors cmap.ConcurrentMap
	subscription     *pubsub.Subscription
	topic            *pubsub.Topic
	PeerChan         chan peer.AddrInfo
	peerListChan     chan peer.IDSlice
}

func (p2p *P2P) HandlePeerFound(pi peer.AddrInfo) {
	p2p.PeerChan <- pi
}

func (p2p *P2P) getRPCHandler(msgType string) (*rpcHandler, error) {
	if handler, found := p2p.rpcHandlers[msgType]; found {
		return handler, nil
	}
	return nil, fmt.Errorf("RPC handler for method '%s' not found", msgType)
}

func (p2p *P2P) addRPCHandler(methodName string, handler *rpcHandler) {
	p2p.rpcHandlers[methodName] = handler
}

func (p2p *P2P) getPubSubHandler(msgType pubsubMsgType) (*pubsubHandler, error) {
	if handler, found := p2p.pubsubHandlers[msgType]; found {
		return handler, nil
	}
	return nil, fmt.Errorf("PubSub handler for msg type '%s' not found", msgType)
}

func (p2p *P2P) addPubSubHandler(msgType pubsubMsgType, handler *pubsubHandler) {
	p2p.pubsubHandlers[msgType] = handler
}

func (p2p *P2P) newRPCStreamHandler(s network.Stream) {
	_, found := p2p.rpcMsgProcessors.Get(s.Conn().RemotePeer().String())
	if found {
		return
	}

	writeQueue := make(chan rpcMsg, 200)
	ctx, cancel := context.WithCancel(context.Background())
	inserted := p2p.rpcMsgProcessors.SetIfAbsent(s.Conn().RemotePeer().String(), &rpcMsgProcessor{WriteQueue: writeQueue, Stop: cancel})
	if inserted {
		log.Infof("Starting msg processor for peer '%s'", s.Conn().RemotePeer().String())
		go p2p.rpcMsgReader(s, writeQueue, ctx)
		go p2p.rpcMsgWriter(s, writeQueue, ctx)
	}
}

func (p2p *P2P) rpcMsgReader(s network.Stream, writeQueue chan rpcMsg, ctx context.Context) {
	// we process the request in a separate routine
	msgProcessor := func(msgBytes []byte) {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("Exception whie processing incoming p2p RPC msg from '%s': %v", s.Conn().RemotePeer().String(), r)
			}
		}()

		msg := rpcMsg{}
		err := json.Unmarshal(msgBytes, &msg)
		if err != nil {
			log.Errorf("Failed to decode RPC message from '%s': %s", s.Conn().RemotePeer().String(), err.Error())
			return
		}

		if msg.Type == rpcRequest {
			// unmarshal remote request
			reqMsg := rpcPayloadRequest{}
			err = json.Unmarshal(msg.Payload, &reqMsg)
			if err != nil {
				log.Errorf("Failed to decode request from '%s': %s", s.Conn().RemotePeer().String(), err.Error())
				return
			}
			p2p.requestHandler(msg.ID, s.Conn().RemotePeer(), reqMsg, writeQueue)
		} else if msg.Type == rpcResponse {
			// unmarshal remote request
			respMsg := rpcPayloadResponse{}
			err = json.Unmarshal(msg.Payload, &respMsg)
			if err != nil {
				log.Errorf("Failed to decode response from '%s': %s", s.Conn().RemotePeer().String(), err.Error())
				return
			}
			p2p.responseHandler(msg.ID, s.Conn().RemotePeer(), respMsg)
		} else {
			log.Errorf("Wrong RPC message type from '%s': '%s'", s.Conn().RemotePeer().String(), msg.Type)
		}
	}

	readerChan := delimReader(s, '\n')
	for {
		select {
		case bytes := <-readerChan:
			if len(bytes) == 0 {
				continue
			}
			go msgProcessor(bytes)
		case <-ctx.Done():
			log.Debugf("Stopping RPC msg reader for peer '%s'", s.Conn().RemotePeer().String())
			return
		}
	}
}

func (p2p *P2P) rpcMsgWriter(s network.Stream, writeQueue chan rpcMsg, ctx context.Context) {
	for {
		select {
		case msg := <-writeQueue:
			// encode the full response
			jsonMsg, err := json.Marshal(msg)
			if err != nil {
				log.Errorf("Failed to encode msg '%s'(%s) for '%s': %s", msg.ID, msg.Type, s.Conn().RemotePeer().String(), err.Error())
				continue
			}

			jsonMsg = append(jsonMsg, '\n')
			_, err = s.Write(jsonMsg)
			if err != nil {
				log.Errorf("Failed to send msg '%s'(%s) to '%s': %s", msg.ID, msg.Type, s.Conn().RemotePeer().String(), err.Error())
				continue
			}
		case <-ctx.Done():
			log.Debugf("Stopping RPC msg writer for peer '%s'", s.Conn().RemotePeer().String())
			return
		}

	}
}

func (p2p *P2P) requestHandler(id string, peerID peer.ID, request rpcPayloadRequest, writeQueue chan rpcMsg) {
	log.Tracef("Remote request '%s' from peer '%s': %v", id, peerID.String(), request)

	msg := rpcMsg{
		ID:   id,
		Type: rpcResponse,
	}

	response := rpcPayloadResponse{}

	// find handler
	handler, err := p2p.getRPCHandler(request.Type)
	if err != nil {
		log.Errorf("Failed to process request '%s' from '%s': %s", id, peerID.String(), err.Error())
		response.Error = err.Error()

		// encode the response
		jsonResp, err := json.Marshal(response)
		if err != nil {
			log.Errorf("Failed to encode response for request '%s' from '%s': %s", id, peerID.String(), err.Error())
			return
		}
		msg.Payload = jsonResp
		writeQueue <- msg
		return
	}

	// execute handler method
	data := reflect.New(reflect.ValueOf(handler.RequestStruct).Elem().Type()).Interface()
	err = json.Unmarshal(request.Data, &data)
	if err != nil {
		response.Error = fmt.Errorf("failed to decode data struct: %s", err.Error()).Error()

		// encode the response
		jsonResp, err := json.Marshal(response)
		if err != nil {
			log.Errorf("Failed to encode response for request '%s' from '%s': %s", id, peerID.String(), err.Error())
			return
		}

		msg.Payload = jsonResp
		writeQueue <- msg
		return
	}

	var jsonHandlerResponse []byte
	handlerResponse, err := handler.Func(peerID, data)
	if err != nil {
		log.Errorf("Failed to process request '%s' from '%s': %s", id, peerID.String(), err.Error())
	} else {
		// encode the returned handler response
		jsonHandlerResponse, err = json.Marshal(handlerResponse)
		if err != nil {
			log.Errorf("Failed to encode response for request '%s' from '%s': %s", id, peerID.String(), err.Error())
		}
	}

	// add response data or error
	if err != nil {
		response.Error = fmt.Sprintf("Internal error: %s", err)
	} else {
		response.Data = jsonHandlerResponse
	}

	// encode the response
	jsonResp, err := json.Marshal(response)
	if err != nil {
		log.Errorf("Failed to encode response for request '%s' from '%s': %s", id, peerID.String(), err.Error())
		return
	}
	msg.Payload = jsonResp
	log.Tracef("Sending response for msg '%s' to peer '%s': %v", id, peerID.String(), response)

	// send the response
	writeQueue <- msg
}

func (p2p *P2P) responseHandler(id string, peerID peer.ID, response rpcPayloadResponse) {
	log.Tracef("Received response '%s' from peer '%s': %v", id, peerID.String(), response)

	reqInteface, found := p2p.reqs.Get(id)
	if !found {
		log.Errorf("Failed to process response '%s' from '%s': request not found", id, peerID.String())
		return
	}

	req := reqInteface.(*requestTracker)

	// if the closeSig channel is closed, the request has timed out, so we return without sending the response received
	select {
	case <-req.closeSig:
		return
	default:
	}

	close(req.closeSig)

	if response.Error != "" {
		req.err <- fmt.Errorf("error returned by '%s': %s", peerID.String(), response.Error)
	} else {
		req.resp <- response.Data
	}

	close(req.resp)
	close(req.err)
}

func (p2p *P2P) sendRequest(peerID peer.ID, msgType string, requestData interface{}, responseData interface{}) error {
	msg := rpcMsg{
		ID:   ksuid.New().String(),
		Type: rpcRequest,
	}

	// encode the request data
	jsonReqData, err := json.Marshal(requestData)
	if err != nil {
		return fmt.Errorf("failed to encode data for request '%s' for peer '%s': %s", msg.ID, peerID.String(), err.Error())
	}

	request := &rpcPayloadRequest{
		Type: msgType,
		Data: jsonReqData,
	}

	// encode the request
	jsonReq, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to encode request '%s' for peer '%s': %s", msg.ID, peerID.String(), err.Error())
	}
	msg.Payload = jsonReq

	// create the request tracker
	reqTracker := &requestTracker{
		resp:      make(chan []byte),
		err:       make(chan error),
		closeSig:  make(chan interface{}),
		startTime: time.Now(),
	}
	p2p.reqs.Set(msg.ID, reqTracker)

	log.Tracef("Sending request '%s' to '%s': %s", msgType, peerID.String(), string(jsonReq))

	rpcMsgProcessorI, found := p2p.rpcMsgProcessors.Get(peerID.String())
	if !found {
		return fmt.Errorf("failed to send request '%s' for peer '%s': peer writer not found", msg.ID, peerID.String())
	}

	msgProcessor := rpcMsgProcessorI.(*rpcMsgProcessor)
	// send the request
	msgProcessor.WriteQueue <- msg

	go func() {
		// we sleep for the timeout period
		time.Sleep(time.Second * 5)

		// if the closeSig channel is closed, the request has been processed, so we return without sending the timeout error and closing the chans
		select {
		case <-reqTracker.closeSig:
			return
		default:
		}

		// we close the closeSig channel so any response from the handler is discarded
		close(reqTracker.closeSig)

		reqTracker.err <- fmt.Errorf("timeout waiting for request '%s'(%s) to peer '%s'", msg.ID, request.Type, peerID.String())
		close(reqTracker.resp)
		close(reqTracker.err)
	}()

	// wait for response or error and return it, while also deleting the request
	defer p2p.reqs.Remove(msg.ID)
	select {
	case resp := <-reqTracker.resp:
		err := json.Unmarshal(resp, responseData)
		if err != nil {
			return fmt.Errorf("failed to decode response payload: %w", err)
		}
		return nil
	case err := <-reqTracker.err:
		return err
	}

}

func (p2p *P2P) pubsubMsgProcessor() func() error {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		log.Info("Starting PubSub processor")
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("Exception whie processing incoming p2p message: %v", r)
			}
		}()

		for {
			msg, err := p2p.subscription.Next(ctx)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Errorf("Failed to retrieve pub sub message: %s", err.Error())
				}
				return
			}
			peerID := msg.ReceivedFrom.String()
			if msg.ReceivedFrom == p2p.host.ID() {
				continue
			}

			go func(data []byte, peerID string) {
				defer func() {
					if r := recover(); r != nil {
						log.Errorf("Exception whie processing incoming p2p message from peer '%s': %v", peerID, r)
					}
				}()

				var pubsubMsg pubsubMsg
				err = json.Unmarshal(data, &pubsubMsg)
				if err != nil {
					log.Errorf("Failed to decode pub sub message from '%s': %v", peerID, err.Error())
					return
				}

				handler, err := p2p.getPubSubHandler(pubsubMsg.Type)
				if err != nil {
					log.Errorf("Failed to process message from '%s': %v", peerID, err.Error())
					return
				}

				payload := reflect.New(reflect.ValueOf(handler.PayloadStruct).Elem().Type()).Interface()
				err = json.Unmarshal(pubsubMsg.Payload, &payload)
				if err != nil {
					log.Errorf("Failed to process message from '%s': %v", peerID, err.Error())
					return
				}

				err = handler.Func(msg.ReceivedFrom, payload)
				if err != nil {
					log.Errorf("Failed calling pubsub handler for message '%s' from '%s': %v", pubsubMsg.Type, peerID, err.Error())
					return
				}

			}(msg.Data, peerID)
		}
	}()

	stopper := func() error {
		log.Info("Stopping PubSub processor")
		cancel()
		return nil
	}
	return stopper
}

func (p2p *P2P) BroadcastMsg(msgType pubsubMsgType, data interface{}) error {
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	msg := pubsubMsg{
		ID:      ksuid.New().String(),
		Type:    msgType,
		Payload: dataBytes,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return p2p.topic.Publish(context.Background(), msgBytes)
}

// // GetPeerID adds a peer to the p2p manager
// func (p2p *P2P) PubKeyToPeerID(pubKey []byte) (string, error) {
// 	pk, err := crypto.UnmarshalEd25519PublicKey(pubKey)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to unmarshall public key: %w", err)
// 	}
// 	peerID, err := peer.IDFromPublicKey(pk)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to create peer ID from public key: %w", err)
// 	}
// 	return peerID.String(), nil
// }

// // AddPeer adds a peer to the p2p manager
// func (p2p *P2P) AddPeer(machine Machine) (*Client, error) {
// 	pk, err := crypto.UnmarshalEd25519PublicKey(machine.GetPublicKey())
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to unmarshall public key: %w", err)
// 	}
// 	peerID, err := peer.IDFromPublicKey(pk)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to create peer ID from public key: %w", err)
// 	}

// 	destinationString := ""
// 	if machine.GetPublicIP() != "" {
// 		destinationString = fmt.Sprintf("/ip4/%s/tcp/%d/p2p/%s", machine.GetPublicIP(), p2pPort, peerID.String())
// 	} else {
// 		destinationString = fmt.Sprintf("/p2p/%s", peerID.String())
// 	}
// 	maddr, err := multiaddr.NewMultiaddr(destinationString)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to create multi address: %w", err)
// 	}

// 	peerInfo, err := peer.AddrInfoFromP2pAddr(maddr)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to extract info from address: %w", err)
// 	}
// 	rpcpeer := &rpcPeer{machine: machine}
// 	p2p.peers.Set(peerID.String(), rpcpeer)

// 	log.Debugf("Adding peer id '%s'(%s) at ip '%s'", machine.GetName(), peerInfo.ID.String(), machine.GetPublicIP())

// 	err = p2p.host.Connect(context.Background(), *peerInfo)
// 	if err != nil {
// 		log.Errorf("Failed to connect to peer '%s'(%s): %s", machine.GetName(), peerID.String(), err.Error())
// 	}

// 	client, err := p2p.createClientForPeer(peerID)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to add peer '%s': %w", peerID.String(), err)
// 	}
// 	rpcpeer.SetClient(client)

// 	return client, nil
// }

// func (p2p *P2P) GetClient(name string) (*Client, error) {
// 	for peerItem := range p2p.peers.IterBuffered() {
// 		rpcpeer := peerItem.Val.(*rpcPeer)
// 		client := rpcpeer.GetClient()
// 		machine := rpcpeer.GetMachine()
// 		if machine != nil && client != nil && machine.GetName() == name {
// 			return client, nil
// 		}
// 	}

// 	return nil, fmt.Errorf("could not find RPC client for instance '%s'", name)
// }

// // getRPCPeer returns the rpc client for a peer
// func (p2p *P2P) getRPCPeer(peerID peer.ID) (*rpcPeer, error) {
// 	rpcpeerI, found := p2p.peers.Get(peerID.String())
// 	rpcpeer := rpcpeerI.(*rpcPeer)
// 	if found {
// 		return rpcpeer, nil
// 	}
// 	return nil, fmt.Errorf("could not find RPC peer '%s'", peerID.String())
// }

// createClientForPeer returns the remote client that can reach all remote handlers
func (p2p *P2P) createClientForPeer(peerID peer.ID) (client *rpcClient, err error) {

	// err should be nil, and if there is a panic, we change it in the defer function
	// this is required because noms implements control flow using panics
	err = nil
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("exception whie building p2p client: %v", r)
			if strings.Contains(err.Error(), "timeout waiting for request") {
				err = fmt.Errorf("timeout waiting for p2p client")
			}
		}
	}()

	client = &rpcClient{
		peer: peerID,
		p2p:  p2p,
	}

	tries := 0
	for {
		_, err = client.Ping()
		if err != nil {
			if tries < 19 {
				time.Sleep(200 * time.Millisecond)
				tries++
				continue
			} else {
				return nil, err
			}
		} else {
			break
		}
	}

	return client, nil
}

func (p2p *P2P) peerDiscoveryProcessor() func() error {
	stopSignal := make(chan struct{})
	go func() {
		log.Info("Starting peer discovery processor")
		for {
			select {
			case peer := <-p2p.PeerChan:
				log.Debugf("New peer. Connecting: %s", peer)
				ctx := context.Background()
				if err := p2p.host.Connect(ctx, peer); err != nil {
					log.Error("Connection failed: ", err)
					continue
				}

				stream, err := p2p.host.NewStream(ctx, peer.ID, protocol.ID(protosRPCProtocol))
				if err != nil {
					log.Error("Stream open failed: ", err)
				} else {
					p2p.newRPCStreamHandler(stream)
					log.Debugf("Connected to: ", peer)

					p2p.peerListChan <- p2p.host.Network().Peers()
				}

			case <-stopSignal:
				log.Info("Stopping peer discovery processor")
				return
			}
		}
	}()
	stopper := func() error {
		stopSignal <- struct{}{}
		return nil
	}
	return stopper
}

func (p2p *P2P) closeConnectionHandler(netw network.Network, conn network.Conn) {
	rpcMsgProcessorI, found := p2p.rpcMsgProcessors.Pop(conn.RemotePeer().String())
	if found {
		log.Infof("Stopping msg processor for peer '%s'.", conn.RemotePeer().String())
		p2p.peerListChan <- p2p.host.Network().Peers()
		msgProcessor := rpcMsgProcessorI.(*rpcMsgProcessor)
		msgProcessor.Stop()
	}
}

// StartServer starts listening for p2p connections
func (p2p *P2P) StartServer() (func() error, error) {

	p2pServer := &Server{p2p: p2p}

	// add rpc handlers
	p2p.addRPCHandler(pingHandler, &rpcHandler{Func: p2pServer.HandlerPing, RequestStruct: &PingReq{}})

	// add pubsub handlers
	p2p.addPubSubHandler(pubsubEcho, &pubsubHandler{Func: p2pServer.HandlerEcho, PayloadStruct: &EchoReq{}})

	err := p2p.host.Network().Listen()
	if err != nil {
		return func() error { return nil }, fmt.Errorf("failed to listen: %w", err)
	}

	pubsubStopper := p2p.pubsubMsgProcessor()
	peerDiscoveryStopper := p2p.peerDiscoveryProcessor()

	ser := mdns.NewMdnsService(p2p.host, "protos", p2p)
	if err := ser.Start(); err != nil {
		panic(err)
	}

	stopper := func() error {
		log.Debug("Stopping p2p server")
		pubsubStopper()
		peerDiscoveryStopper()
		ser.Close()
		return p2p.host.Close()
	}

	return stopper, nil

}

// NewManager creates and returns a new p2p manager
func NewManager(initMode bool, port int, peerListChan chan peer.IDSlice) (*P2P, error) {
	p2p := &P2P{
		rpcHandlers:      map[string]*rpcHandler{},
		pubsubHandlers:   map[pubsubMsgType]*pubsubHandler{},
		reqs:             cmap.New(),
		rpcMsgProcessors: cmap.New(),
		PeerChan:         make(chan peer.AddrInfo),
		peerListChan:     peerListChan,
	}

	p2p.PubSubClient = &PubSubClient{p2p: p2p}

	prvKey, _, err := crypto.GenerateKeyPair(crypto.Ed25519, 0)
	if err != nil {
		return nil, err
	}

	con, err := connmgr.NewConnManager(100, 400)
	if err != nil {
		return nil, err
	}

	host, err := libp2p.New(
		libp2p.Identity(prvKey),
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic", port),
		),
		libp2p.Security(noise.ID, noise.New),
		libp2p.DefaultTransports,
		libp2p.ConnectionManager(con),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to setup p2p host: %w", err)
	}

	log.Infof("Starting p2p server using id %s", host.ID())

	p2p.host = host
	p2p.host.SetStreamHandler(protocol.ID(protosRPCProtocol), p2p.newRPCStreamHandler)
	pubSub, err := pubsub.NewFloodSub(context.Background(), host)
	if err != nil {
		return nil, fmt.Errorf("failed to setup PubSub channel: %w", err)
	}

	nb := network.NotifyBundle{
		DisconnectedF: p2p.closeConnectionHandler,
	}
	p2p.host.Network().Notify(&nb)

	p2p.topic, err = pubSub.Join(protosUpdatesTopic)
	if err != nil {
		return nil, fmt.Errorf("failed to join PubSub topic '%s': %w", protosUpdatesTopic, err)
	}

	p2p.subscription, err = p2p.topic.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to PubSub topic '%s': %w", protosUpdatesTopic, err)
	}

	log.Debugf("Using host with ID '%s'", host.ID().String())
	return p2p, nil
}

func delimReader(r io.Reader, delim byte) <-chan []byte {
	ch := make(chan []byte)

	go func() {
		buf := bufio.NewReader(r)

		for {
			bytes, err := buf.ReadBytes('\n')
			if len(bytes) != 0 {
				ch <- bytes
			}

			if err != nil {
				break
			}
		}

		close(ch)
	}()

	return ch
}
