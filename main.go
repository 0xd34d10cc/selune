package main

import (
	"context"
	"encoding/json"
	"errors"
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

type Request struct {
	Type     string   `json:"type"`
	StreamID StreamID `json:"stream_id,omitempty"`
	Stream   *Stream  `json:"stream,omitempty"`
}

type Response struct {
	Status   string   `json:"status"`
	Message  string   `json:"message,omitempty"`
	StreamID StreamID `json:"stream_id,omitempty"`
}

func failure(message string) Response {
	return Response{
		Status:  "fail",
		Message: message,
	}
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

func (server *Server) processRequest(c *websocket.Conn, request Request) error {
	sendResponse := func(response interface{}) error {
		data, err := json.Marshal(&response)
		if err != nil {
			log.Fatal(err)
		}

		return c.WriteMessage(websocket.BinaryMessage, data)
	}

	switch request.Type {
	case "get_streams":
		streams, err := server.Streams()
		if err != nil {
			log.Println("Failed to query list of active streams:", err)
			return sendResponse(failure("Failed to query streams"))
		}

		return sendResponse(streams)
	case "add_stream":
		if request.Stream == nil {
			response := failure("No stream provided for add_stream request")
			return sendResponse(response)
		}

		id, err := server.AddStream(*request.Stream)
		if err != nil {
			return sendResponse(failure(err.Error()))
		}

		return sendResponse(Response{
			Status:   "success",
			StreamID: id,
		})
	case "remove_stream":
		if request.StreamID == "" {
			resposne := failure("No stream id provided for remove_stream request")
			return sendResponse(resposne)
		}

		err := server.RemoveStream(request.StreamID)
		if err != nil {
			return sendResponse(failure(err.Error()))
		}
		return sendResponse(Response{Status: "success"})
	default:
		return sendResponse(failure("Invalid request type"))
	}
}

func (server *Server) processConnection(c *websocket.Conn) {
	defer c.Close()
	log.Println("Received a new connection from:", c.RemoteAddr())

	for {
		mt, message, err := c.ReadMessage()

		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				log.Printf("Connection %v has been successfully closed by user", c.RemoteAddr())
				return
			}

			log.Println("Failed to read message:", err)
			return
		}

		switch mt {
		case websocket.TextMessage:
			log.Println("Received unexpected text message")
			return
		case websocket.BinaryMessage:
			request := Request{}
			err = json.Unmarshal(message, &request)
			if err != nil {
				log.Println("Invalid message received:", err)
				return
			}

			err := server.processRequest(c, request)
			if err != nil {
				log.Println("Failed to process request:", err)
				return
			}
		case websocket.CloseMessage:
			log.Println("Connection successfully closed")
			return
		case websocket.PingMessage:
			c.WriteMessage(websocket.PongMessage, message)
		case websocket.PongMessage:
			// ignore
		}
	}
}

func (server *Server) Run() error {
	upgrader := websocket.Upgrader{}
	http.HandleFunc("/streams", func(rw http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			log.Println("Upgrade error: ", err)
			return
		}

		server.processConnection(c)
	})

	log.Println("Listening on", server.addr)

	// TODO: TLS
	return http.ListenAndServe(server.addr, nil)
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

	log.Fatal(server.Run())

}
