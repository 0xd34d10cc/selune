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

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type StreamID = string

type Stream struct {
	Address     string `json:"address"`
	Description string `json:"description"`
}

type Server struct {
	addr     string
	dbClient *mongo.Client
	streams  *mongo.Collection
}

type Client struct {
	conn   *websocket.Conn
	server *Server
}

type Request struct {
	Type     string   `json:"type"`
	StreamID StreamID `json:"stream_id,omitempty"`
	Stream   *Stream  `json:"stream,omitempty"`
}

type Response struct {
	Status   string              `json:"status"`
	Message  string              `json:"message,omitempty"`
	StreamID StreamID            `json:"stream_id,omitempty"`
	Streams  map[StreamID]Stream `json:"streams,omitempty"`
}

func NewServer(addr string, dbUri string) (*Server, error) {
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
	return &Server{
		addr:     addr,
		dbClient: dbClient,
		streams:  streams,
	}, nil
}

func (server *Server) Accept(conn *websocket.Conn) Client {
	return Client{
		conn:   conn,
		server: server,
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

func (server *Server) AddStream(stream Stream) (StreamID, error) {
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

	return StreamID(res.InsertedID.(primitive.ObjectID).Hex()), nil
}

func (server *Server) RemoveStream(id StreamID) error {
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return err
	}

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

func (server *Server) Run() error {
	upgrader := websocket.Upgrader{}
	http.HandleFunc("/streams", func(rw http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			log.Println("Upgrade error: ", err)
			return
		}
		defer c.Close()
		client := server.Accept(c)
		client.Run()
	})

	log.Println("Listening on", server.addr)

	// TODO: TLS
	return http.ListenAndServe(server.addr, nil)
}

func (c *Client) Run() {
	c.Log("New connection")

	reader := bytes.NewReader([]byte{})
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	for {
		_, message, err := c.conn.ReadMessage()

		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				c.Log("Connection closed by user")
				return
			}

			c.Log("Failed to read message: %v", err)
			return
		}

		reader.Reset(message)
		request := Request{}
		err = decoder.Decode(&request)
		if err != nil {
			c.Log("Invalid message received: %v", err)
			return
		}

		if decoder.More() {
			c.Log("Incomplete parse")
			return
		}

		err = c.processRequest(request)
		if err != nil {
			c.Log("Failed to process request: %v", err)
			return
		}
	}
}

func (c *Client) Log(f string, args ...interface{}) {
	addr := c.conn.RemoteAddr()
	if len(args) != 0 {
		f = fmt.Sprintf(f, args...)
	}
	log.Printf("[%v] %v", addr, f)
}

func (c *Client) sendFail(message string, args ...interface{}) error {
	if len(args) != 0 {
		message = fmt.Sprintf(message, args...)
	}

	return c.sendResponse(Response{
		Status:  "fail",
		Message: message,
	})
}

func (c *Client) sendResponse(response Response) error {
	data, err := json.Marshal(&response)
	if err != nil {
		log.Fatal(err)
	}

	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *Client) processRequest(request Request) error {
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
			Streams: streams,
		})
	case "add_stream":
		if request.Stream == nil {
			return c.sendFail("No stream provided for add_stream request")
		}

		id, err := c.server.AddStream(*request.Stream)
		if err != nil {
			return c.sendFail("Failed to add stream: %v", err.Error())
		}

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

	server, err := NewServer(addr, mongo)
	if err != nil {
		log.Fatal("Failed to create server instance:", err)
	}

	err = server.Run()
	if err != nil {
		log.Fatal(err)
	}
}
