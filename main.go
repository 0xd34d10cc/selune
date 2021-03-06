package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type StreamID = string

type Server struct {
	mux      *http.ServeMux
	dbClient *mongo.Client
	streams  *mongo.Collection

	m         sync.Mutex
	streamers map[StreamID]chan Response
}

type Session struct {
	conn    *websocket.Conn
	server  *Server
	streams map[StreamID]struct{}

	messages      chan Request
	notifications chan Response
	errors        chan error
}

type Stream struct {
	Address     string `json:"address"`
	Description string `json:"description"`
}

type Request struct {
	Type        string   `json:"type"`
	StreamID    StreamID `json:"stream_id,omitempty"`
	Stream      *Stream  `json:"stream,omitempty"`
	Destination string   `json:"destination,omitempty"`
}

type Response struct {
	Status      string               `json:"status"`
	Message     string               `json:"message,omitempty"`
	StreamID    StreamID             `json:"stream_id,omitempty"`
	Streams     *map[StreamID]Stream `json:"streams,omitempty"`
	Destination string               `json:"destination,omitempty"`
}

func NewServer(path string, dbUri string) (*Server, error) {
	dbClient, err := mongo.Connect(context.Background(), options.Client().ApplyURI(dbUri))
	if err != nil {
		return nil, err
	}

	err = dbClient.Ping(context.Background(), readpref.Primary())
	if err != nil {
		return nil, err
	}

	log.Println("Successfully connected to", dbUri)
	streams := dbClient.Database("selune").Collection("streams")

	server := &Server{
		mux:      http.NewServeMux(),
		dbClient: dbClient,
		streams:  streams,

		m:         sync.Mutex{},
		streamers: make(map[StreamID]chan Response),
	}

	upgrader := websocket.Upgrader{}
	server.mux.HandleFunc(path, func(rw http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			log.Printf("[%v] Failed to upgrade to websocket: %v", r.RemoteAddr, err)
			return
		}
		session := server.Accept(c)
		defer session.Close()
		session.Run()
	})

	return server, nil
}

func (server *Server) Accept(conn *websocket.Conn) Session {
	return Session{
		conn:    conn,
		server:  server,
		streams: make(map[StreamID]struct{}),

		messages:      make(chan Request),
		notifications: make(chan Response),
		errors:        make(chan error),
	}
}

func (server *Server) Streams() (map[StreamID]Stream, error) {
	cur, err := server.streams.Find(context.Background(), bson.D{})
	if err != nil {
		return nil, err
	}

	defer cur.Close(context.Background())
	streams := make(map[StreamID]Stream)

	for cur.Next(context.Background()) {
		var stream bson.M
		err := cur.Decode(&stream)
		if err != nil {
			return nil, err
		}

		objectID := stream["_id"].(primitive.ObjectID)
		streamID := StreamID(objectID.Hex())
		streams[streamID] = Stream{
			Address:     stream["address"].(string),
			Description: stream["description"].(string),
		}
	}

	if cur.Err() != nil {
		return nil, cur.Err()
	}

	return streams, nil
}

func (server *Server) AddStream(stream Stream, notifications chan Response) (StreamID, error) {
	if _, err := url.Parse(stream.Address); err != nil {
		return "", errors.New("Invalid stream address")
	}

	res, err := server.streams.InsertOne(context.Background(), bson.M{
		"address":     stream.Address,
		"description": stream.Description,
	})
	if err != nil {
		return "", err
	}

	id := StreamID(res.InsertedID.(primitive.ObjectID).Hex())

	server.m.Lock()
	server.streamers[id] = notifications
	server.m.Unlock()
	return id, nil
}

func (server *Server) RemoveStream(id StreamID) error {
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return err
	}

	server.m.Lock()
	delete(server.streamers, id)
	server.m.Unlock()

	res, err := server.streams.DeleteOne(context.Background(), bson.M{
		"_id": objectID,
	})
	if err != nil {
		return err
	}

	if res.DeletedCount != 1 {
		return errors.New("No such stream")
	}

	return nil
}

func (server *Server) NotifyStreamer(id StreamID, destination string) error {
	if _, err := primitive.ObjectIDFromHex(id); err != nil {
		return err
	}

	if _, err := url.Parse(destination); err != nil {
		return err
	}

	server.m.Lock()
	notifications, ok := server.streamers[id]
	server.m.Unlock()

	if !ok {
		return errors.New("No such stream")
	}

	notifications <- Response{
		Status:      "new_viewer",
		StreamID:    id,
		Destination: destination,
	}

	return nil
}

func (server *Server) Run(addr string) error {
	log.Println("Listening on", addr)
	// TODO: TLS
	return http.ListenAndServe(addr, server.mux)
}

func (session *Session) readMessages() {
	reader := bytes.NewReader([]byte{})
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	for {
		_, message, err := session.conn.ReadMessage()

		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				session.errors <- nil
				return
			}

			session.errors <- err
			return
		}

		reader.Reset(message)
		request := Request{}
		err = decoder.Decode(&request)
		if err != nil {
			session.errors <- err
			return
		}

		if decoder.More() {
			session.errors <- errors.New("Incomplete parse")
			return
		}

		session.messages <- request
	}
}

func (c *Session) Run() {
	c.Log("New connection")
	go c.readMessages()
	for {
		select {
		case r := <-c.messages:
			err := c.processRequest(r)
			if err != nil {
				c.Log("Request failed: %v", err)
				// TODO: how to kill readMessages() goroutine?
				return
			}
		case n := <-c.notifications:
			err := c.sendResponse(n)
			if err != nil {
				c.Log("Failed to send notification: %v", err)
				// TODO: how to kill readMessages() goroutine?
				return
			}
		case err := <-c.errors:
			if err != nil {
				c.Log("Failed to read request: %v", err)
			} else {
				c.Log("Connection closed by user")
			}
			return
		}
	}
}

func (c *Session) Close() {
	for id := range c.streams {
		c.server.RemoveStream(id)
	}
	c.conn.Close()
}

func (c *Session) Log(f string, args ...interface{}) {
	addr := c.conn.RemoteAddr()
	if len(args) != 0 {
		f = fmt.Sprintf(f, args...)
	}
	log.Printf("[%v] %v", addr, f)
}

func (c *Session) sendFail(message string, args ...interface{}) error {
	if len(args) != 0 {
		message = fmt.Sprintf(message, args...)
	}

	return c.sendResponse(Response{
		Status:  "fail",
		Message: message,
	})
}

func (c *Session) sendResponse(response Response) error {
	data, err := json.Marshal(&response)
	if err != nil {
		log.Fatal(err)
	}

	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *Session) processRequest(request Request) error {
	c.Log("Received \"%v\" request", request.Type)
	switch request.Type {
	case "get_streams":
		streams, err := c.server.Streams()
		if err != nil {
			c.Log("Failed to query list of active streams: %v", err)
			return c.sendFail("Failed to query streams")
		}

		return c.sendResponse(Response{
			Status:  "success",
			Streams: &streams,
		})
	case "add_stream":
		if request.Stream == nil {
			return c.sendFail("No stream provided for add_stream request")
		}

		id, err := c.server.AddStream(*request.Stream, c.notifications)
		if err != nil {
			return c.sendFail("Failed to add stream: %v", err.Error())
		}

		c.streams[id] = struct{}{}
		return c.sendResponse(Response{
			Status:   "success",
			StreamID: id,
		})
	case "remove_stream":
		if request.StreamID == "" {
			return c.sendFail("No stream id provided for remove_stream request")
		}

		err := c.server.RemoveStream(request.StreamID)
		if err != nil {
			return c.sendFail("Failed to remove stream: %v", err.Error())
		}

		delete(c.streams, request.StreamID)
		return c.sendResponse(Response{Status: "success"})
	case "watch":
		if request.StreamID == "" {
			return c.sendFail("No stream id provided for watch request")
		}

		if request.Destination == "" {
			return c.sendFail("No destination provided for watch request")
		}

		err := c.server.NotifyStreamer(request.StreamID, request.Destination)
		if err != nil {
			return c.sendFail("Failed to watch stream: %v", err.Error())
		}
		return c.sendResponse(Response{Status: "success"})
	default:
		return c.sendFail("Invalid request type")
	}
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Failed to load dotenv file:", err)
	}

	addr, ok := os.LookupEnv("SELUNE_ADDRESS")
	if !ok {
		addr = "localhost:8080"
	}

	mongo, ok := os.LookupEnv("SELUNE_MONGODB")
	if !ok {
		mongo = "mongodb://localhost:27017"
	}

	server, err := NewServer("/streams", mongo)
	if err != nil {
		log.Fatal("Failed to create server instance:", err)
	}

	err = server.Run(addr)
	if err != nil {
		log.Fatal(err)
	}
}
