package main

import (
	"context"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"

	proto "selune/proto"

	"github.com/joho/godotenv"
	"google.golang.org/grpc"
)

func status(err string) *proto.Status {
	return &proto.Status{
		Description: err,
	}
}

type ConnectionID = uint32
type ActiveConnection struct {
	user         *proto.User
	connRequests chan *proto.User
}

type Server struct {
	m           sync.RWMutex
	connections map[ConnectionID]ActiveConnection

	connectionIDCounter ConnectionID

	proto.UnimplementedSeluneServer
}

func NewServer() (*Server, error) {
	return &Server{
		m:           sync.RWMutex{},
		connections: make(map[uint32]ActiveConnection),

		connectionIDCounter: 0,
	}, nil
}

func (s *Server) nextConnectionID() ConnectionID {
	return atomic.AddUint32(&s.connectionIDCounter, 1)
}

func (s *Server) Run(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer()
	proto.RegisterSeluneServer(grpcServer, s)
	log.Println("Starting server on", addr)
	// TODO: tls
	return grpcServer.Serve(listener)
}

func (s *Server) GoOnline(user *proto.User, resp proto.Selune_GoOnlineServer) error {
	sendStatus := func(err string) error {
		return resp.Send(&proto.NewConnection{
			Status: status(err),
		})
	}

	if user.GetId() != 0 {
		return sendStatus("ID should not be set in GoOnline() request")
	}

	if user.GetUsername() == "" {
		return sendStatus("Username should be set in GoOnline() request")
	}

	if user.GetAddress() == nil || len(user.GetAddress()) == 0 {
		return sendStatus("Address should be set in GoOnline() request")
	}

	id := s.nextConnectionID()
	connRequests := make(chan *proto.User, 3)
	user.Id = id
	s.m.Lock()
	s.connections[id] = ActiveConnection{
		user:         user,
		connRequests: connRequests,
	}
	s.m.Unlock()

	log.Printf("[%v] Online", id)

	defer func() {
		s.m.Lock()
		delete(s.connections, id)
		s.m.Unlock()
	}()

	err := resp.Send(&proto.NewConnection{
		Id: id,
	})

	if err != nil {
		return err
	}

	for {
		select {
		case <-resp.Context().Done():
			log.Printf("[%v] Going offline\n", id)
			return nil
		case req := <-connRequests:
			log.Printf("[%v] Got new connection from %v\n", id, req.Id)
			err = resp.Send(&proto.NewConnection{
				Initiator: req,
			})

			if err != nil {
				return err
			}
		}
	}
}

func (s *Server) Connect(c context.Context, req *proto.ConnectionRequest) (*proto.Status, error) {
	s.m.RLock()
	initiator, initiatorFound := s.connections[req.GetInitiatorId()]
	target, targetFound := s.connections[req.GetTargetId()]
	s.m.RUnlock()

	log.Printf("[%v] requests connect() to %v\n", req.GetInitiatorId(), req.GetTargetId())

	if !initiatorFound {
		return status("Invalid initiator_id"), nil
	}

	if !targetFound {
		return status("Invalid target_id"), nil
	}

	target.connRequests <- initiator.user
	return status(""), nil
}

func (s *Server) ListOnlineUsers(c context.Context, req *proto.UserListRequest) (*proto.UserList, error) {
	users := make([]*proto.User, 0)
	s.m.RLock()
	for _, conn := range s.connections {
		users = append(users, conn.user)
	}
	s.m.RUnlock()

	return &proto.UserList{Users: users}, nil
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Failed to load dotenv file:", err)
	}

	addr, ok := os.LookupEnv("SELUNE_ADDRESS")
	if !ok {
		addr = "localhost:8080"
	}

	server, err := NewServer()
	if err != nil {
		log.Fatal("Failed to create server instance:", err)
	}

	err = server.Run(addr)
	if err != nil {
		log.Fatal(err)
	}
}
