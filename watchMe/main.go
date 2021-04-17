package main

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"log"
	"net/http"
	"os"
)

type StreamID = string
type StreamersT = map[StreamID]*Client

type Client struct {
	conn   *websocket.Conn
	server *Server
}

type Server struct {
	addr     string
	dbClient *mongo.Client
	streams  *mongo.Collection
	streamers StreamersT
}

type ClientHelloMessage struct {
	Type string `json:"type"`
	StreamID string `json:"stream_id"`
}

type ServerHelloMessage struct {
	Status string `json:"status"`
}

type WatchRequest struct {
	Id StreamID `json:"id"`
	Uri string `json:"uri"`
}

type WatchSpectatorResponse struct {
	Status string `json:"status"` // if stream exists or not
}

type WatchStreamerNotification struct {
	Uri string `json:"uri"` // spectator's uri
}

func (c *Client) handleWatchRequest(req* WatchRequest) {
	streamer, ok := c.server.streamers[req.Id]
	// TODO: check if URI is valid
	log.Println("start handling watch request")
	if !ok {
		log.Println("there is no streamer with such stream id: ", req.Id)
		resp := WatchSpectatorResponse{Status: "No such streamer"}
		// TODO: Check if stream is existing in db

		msg, err := json.Marshal(resp)
		if err != nil {
			log.Fatal("Can't marshall streamer notification: ", err)
			return
		}
		err = c.conn.WriteMessage(websocket.BinaryMessage, msg)
		if err != nil {
			log.Println("can't write message to the client: ", err)
		}
		return
	}

	notification := WatchStreamerNotification{Uri: req.Uri}
	msg, err := json.Marshal(notification)
	if err != nil {
		log.Fatal("Can't marshall streamer notification: ", err)
		return
	}
	// TODO: Make sure that we could use connection from another goroutine
	err = streamer.conn.WriteMessage(websocket.BinaryMessage, msg)
	if err != nil {
		log.Println("can't write message to the client: ", err)
		return
	}

	resp := WatchSpectatorResponse{Status: "Sucksex"}
	msg, err = json.Marshal(resp)
	err = c.conn.WriteMessage(websocket.BinaryMessage, msg)
	if err != nil {
		log.Println("can't write message to the client: ", err)
		return
	}

	log.Println("notification is sent to streamer: ", streamer.conn.LocalAddr())
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Failed to load dotenv file:", err)
	}

	addr, ok := os.LookupEnv("SELUNE_ADDRESS")
	if !ok {
		addr = "localhost:8888"
	}

	mongo, ok := os.LookupEnv("SELUNE_MONGODB")
	if !ok {
		mongo = "mongodb://localhost:27017"
	}

	server, err := NewServer(addr, mongo)
	if err != nil {
		log.Fatal("Failed to create server instance:", err)
	}

	log.Fatal(server.Run())
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
		streamers: make(StreamersT),
	}, nil
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
	log.Println("New connection")

	firstReq := ClientHelloMessage{}
	mt, message, err := c.conn.ReadMessage()
	if err != nil {
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			log.Println("Connection closed by user")
			return
		}
		log.Println("Failed to read message: %v", err)
		return
	}
	if mt != websocket.BinaryMessage {
		log.Println("Not a binary message")
		return
	}
	// first message should be about client's type (streamer or not)
	err = json.Unmarshal(message, &firstReq)
	if err != nil {
		log.Println("Can't unmarshal hello message from client")
		return
	}

	if firstReq.Type == "" {
		log.Println("invalid first message from client")
		return
	}

	if firstReq.Type == "streamer" {
		if firstReq.StreamID != "" {
			// todo: mutex here
			c.server.streamers[firstReq.StreamID] = c
		} else {
			log.Println("Streamer didn't send his streamid")
			return
		}
	} else if firstReq.Type != "spectator" {
		log.Println("Invalid client type: ", firstReq.Type)
		return
	}

	hello := ServerHelloMessage{"Welcome"}
	msg, err  := json.Marshal(hello)
	if err != nil {
		log.Fatal("Can't unmarshal watch request: ", err)
		return
	}

	err = c.conn.WriteMessage(websocket.BinaryMessage, msg)
	if err != nil {
		log.Println("can't write message to the client: ", err)
		return
	}


	for {
		mt, message, err := c.conn.ReadMessage()

		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				log.Println("Connection closed by user")
				return
			}
			log.Println("Failed to read message: %v", err)
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			req := WatchRequest{}
			err := json.Unmarshal(message, &req)
			if err != nil {
				log.Fatal("Can't unmarshal watch request: ", err)
				return
			}
			c.handleWatchRequest(&req)
 		default:
			log.Println("Wrong message type")
			return
		}
	}

}
func (server *Server) Accept(conn *websocket.Conn) Client {
	return Client{
		conn:   conn,
		server: server,
	}
}