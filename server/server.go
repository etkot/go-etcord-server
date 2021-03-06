package server

import (
	"fmt"
	"net"
	"sync"

	"etcord/common"
	"etcord/protocol"

	log "github.com/sirupsen/logrus"
)

type Server struct {
	mu sync.Mutex
	wg sync.WaitGroup

	stop         chan struct{}
	port         string
	lastClientID int
	clients      map[uint16]*Client
	channels     map[uint16]*Channel
}

func NewServer(port string) *Server {
	return &Server{
		port:     port,
		stop:     make(chan struct{}),
		clients:  make(map[uint16]*Client),
		channels: make(map[uint16]*Channel),
	}
}

type Request struct {
	sender *Client
	msg    protocol.Serializer
}

func NewRequest(client *Client) *Request {
	return &Request{
		sender: client,
	}
}

type Client struct {
	UserID uint16
	Name   string

	conn net.Conn
}

type Channel struct {
	ID       uint16
	ParentID uint16
	Name     string
	Type     protocol.ChannelType

	mu            sync.RWMutex
	lastMessageID int
	messageIDs    map[uint16]*protocol.ChatMessage
	messages      []*protocol.ChatMessage
}

func NewChannel(channelType protocol.ChannelType) *Channel {
	// TODO
	return &Channel{
		ID:       0,
		ParentID: 0,
		Name:     "txt",
		Type:     channelType,
		messageIDs: make(map[uint16]*protocol.ChatMessage),
	}
}

func (s *Server) Start() {
	s.wg.Add(2) // XXX TODO
	go s.tcpServer()

	log.Info("Starting Etcord server")
}

func (s *Server) Stop() {
	log.Info("Stopping Etcord server")
	close(s.stop)
}

// XXX
func (s *Server) Wait() {
	s.wg.Wait()
}

func (s *Server) NewClient(conn net.Conn) *Client {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := &Client{
		UserID: uint16(s.lastClientID),
		conn:   conn,
	}
	s.lastClientID++

	return c
}

func (s *Server) AddChannel() *Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	chn := NewChannel(protocol.TextChannelType)
	s.channels[chn.ID] = chn
	return chn
}

func (s *Server) SendToOne(c *Client, m protocol.Serializer) error {
	b, err := protocol.Serialize(m)
	if err != nil {
		return fmt.Errorf("serialization failed: %s", err)
	}
	if _, err := c.conn.Write(b); err != nil {
		s.removeClient(c)
	}
	return nil
}

func (s *Server) SendToAll(m protocol.Serializer) error {
	b, err := protocol.Serialize(m)
	if err != nil {
		return fmt.Errorf("serialization failed: %s", err)
	}

	for _, client := range s.clients {
		if _, err := client.conn.Write(b); err != nil {
			s.removeClient(client)
		}
	}
	return nil
}

func (s *Server) tcpServer() {
	defer s.wg.Done()

	l, err := net.Listen("tcp4", ":"+s.port)
	if err != nil {
		log.Errorf("Failed to start listener: %s", err)
		return
	}
	defer l.Close()

	log.Infof("Server listening on %s", s.port)

	go func() {
		defer s.wg.Done()
		select {
		case <-s.stop:
			l.Close()
			return
		}
	}()

	for {
		c, err := l.Accept()
		if err != nil {
			log.Errorf("Failed to accept connection: %s", err)
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				continue
			}
			// Listener is closed
			break
		}
		go s.handleConn(c)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	log.Infof("Connected to %s", conn.RemoteAddr().String())

	client := s.NewClient(conn)
	s.addClient(client)
	defer s.removeClient(client)

	tmp := make([]byte, 1024)
	for {
		n, err := conn.Read(tmp)
		if err != nil {
			log.Error(err)
			break
		}
		log.Debugf("Read %d bytes: [% x]", n, tmp[:n])

		buf := common.NewBuffer(tmp[:n])
		var reqs []*Request
		for {
			if buf.Len() == 0 {
				break
			}
			req := NewRequest(client)
			if req.msg, err = protocol.Deserialize(buf); err != nil {
				log.Errorf("Failed to deserialize msg from buffer: %s", err)
				break
			}
			reqs = append(reqs, req)
		}

		for _, req := range reqs {
			// TODO error handling
			if err := s.msgHandler(req); err != nil {
				log.Errorf("Failed to process msg: %s", err)
			}
		}
	}

	if err := conn.Close(); err != nil {
		log.Errorf("Failed to close connection: %s", err)
	}

	log.Infof("Disconnected from %s", conn.RemoteAddr().String())
}

func (s *Server) addClient(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clients[c.UserID] = c
}

func (s *Server) removeClient(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.clients, c.UserID)
}

func (s *Server) msgHandler(req *Request) error {
	log.Debugf("Recv %s", protocol.GetMsgType(req.msg))

	var err error
	switch req.msg.(type) {
	case *protocol.LoginRequest:
		err = s.handleLoginRequest(req)
	case *protocol.GetClientsRequest:
		err = s.handleGetClientsRequest(req)
	case *protocol.GetChatHistoryRequest:
		err = s.handleGetChatHistoryRequest(req)
	case *protocol.ChatMessageRequest:
		err = s.handleChatMessage(req)
	}

	return err
}

func (s *Server) handleLoginRequest(req *Request) error {
	m := req.msg.(*protocol.LoginRequest)
	req.sender.Name = m.Name
	log.Debugf("Set name of %s to %s", req.sender.conn.LocalAddr().String(), m.Name)
	return nil
}

func (s *Server) handleGetClientsRequest(req *Request) error {
	m := req.msg.(*protocol.GetClientsRequest)

	var clients []protocol.Client

	switch m.Type {
	case protocol.GetClientsAll:
		for _, c := range s.clients {
			clients = append(clients, protocol.Client{
				UserID: c.UserID,
				Name:   c.Name,
			})
		}

	case protocol.GetClientsOne:
		c, ok := s.clients[m.ClientID]
		if !ok {
			return fmt.Errorf("client does not exist")
		}
		clients = append(clients, protocol.Client{
			UserID: c.UserID,
			Name:   c.Name,
		})

	case protocol.GetClientsMany:
		for _, id := range m.ClientIDs {
			c, ok := s.clients[id]
			if !ok {
				log.Debugf("client %d does not exist", id)
				continue
			}
			clients = append(clients, protocol.Client{
				UserID: c.UserID,
				Name:   c.Name,
			})
		}
	}

	res := &protocol.GetClientsResponse{
		Count:   uint16(len(clients)),
		Clients: clients,
	}

	if err := s.SendToOne(req.sender, res); err != nil {
		return fmt.Errorf("failed to respond: %s", err)
	}
}

func (s *Server) handleGetChannelsRequest(req *Request) error {
	var channels []protocol.Channel
	for _, chn := range s.channels {
		channels = append(channels, protocol.Channel{
			ID:       chn.ID,
			ParentID: chn.ParentID,
			Name:     chn.Name,
			Type:     chn.Type,
		})
	}

	res := &protocol.GetChannelsResponse{
		Count:    uint16(len(channels)),
		Channels: channels,
	}

	if err := s.SendToOne(req.sender, res); err != nil {
		return fmt.Errorf("failed to respond: %s", err)
	}
}

func (s *Server) handleGetChatHistoryRequest(req *Request) error {
	m := req.msg.(*protocol.GetChatHistoryRequest)

	chn, ok := s.channels[m.ChannelID]
	if !ok {
		return fmt.Errorf("channel %d does not exist", m.ChannelID)
	}

	// TODO handle offsetID
	var chatMessages []protocol.ChatMessage
	for i, cm := range chn.messages {
		if i == int(m.Count) {
			break
		}
		chatMessages = append(chatMessages, *cm)
	}

	res := &protocol.GetChatHistoryResponse{
		ChannelID: chn.ID,
		Count:     uint16(len(chatMessages)),
		Messages:  chatMessages,
	}

	if err := s.SendToOne(req.sender, res); err != nil {
		return fmt.Errorf("failed to respond: %s", err)
	}
}

func (s *Server) handleChatMessage(req *Request) error {
	m := req.msg.(*protocol.ChatMessageRequest)
	chn, ok := s.channels[m.ChannelID]
	if !ok {
		return fmt.Errorf("channel with ID %d does not exist", m.ChannelID)
	}

	if chn.Type != protocol.TextChannelType {
		return fmt.Errorf("channel type is wrong")
	}

	chn.mu.Lock()
	defer chn.mu.Unlock()

	chn.lastMessageID++
	chatMsg := &protocol.ChatMessage{
		MessageID: uint16(chn.lastMessageID),
		Content:   m.Content,
	}

	chn.messageIDs[chatMsg.MessageID] = chatMsg
	chn.messages = append(chn.messages, chatMsg)

	res := &protocol.ChatMessageResponse{
		ChannelID: chn.ID,
		Message:   *chatMsg,
	}

	if err := s.SendToAll(res); err != nil {
		return fmt.Errorf("failed to respond: %s", err)
	}

	log.Debugf("Processed new chat message by %s", req.sender.Name)

	return nil
}
