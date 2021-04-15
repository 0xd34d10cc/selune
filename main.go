package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

type Username string

type Stream struct {
	Address     string `json:"address"`
	Description string `json:"description"`
}

type Server struct {
	// TODO: store streams in DB
	m       sync.Mutex
	streams map[Username]Stream
}

type Request struct {
	Type     string    `json:"type"`
	Username *Username `json:"username,omitempty"`
	Stream   *Stream   `json:"stream,omitempty"`
}

type Response struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

var SuccessResponse, _ = json.Marshal(Response{
	Status:  "success",
	Message: "",
})

func failure(message string) []byte {
	data, err := json.Marshal(Response{
		Status:  "fail",
		Message: message,
	})
	if err != nil {
		log.Fatal(err)
	}

	return data
}

func IsValidAddress(address string) bool {
	_, err := url.Parse(address)
	return err == nil
}

func (server *Server) Streams() map[Username]Stream {
	server.m.Lock()
	defer server.m.Unlock()
	streams := make(map[Username]Stream)
	for username, stream := range server.streams {
		streams[username] = stream
	}

	return streams
}

func (server *Server) AddStream(username Username, stream Stream) error {
	// TODO: validate that |username| actually belongs to the connected user
	server.m.Lock()
	defer server.m.Unlock()
	if !IsValidAddress(stream.Address) {
		return errors.New("Invalid stream address")
	}

	server.streams[username] = stream
	return nil
}

func (server *Server) RemoveStream(username Username) bool {
	server.m.Lock()
	defer server.m.Unlock()
	_, exist := server.streams[username]
	if !exist {
		return false
	}

	delete(server.streams, username)
	return true
}

func (server *Server) processRequest(c *websocket.Conn, request Request) error {
	switch request.Type {
	case "get_streams":
		streams := server.Streams()
		response, err := json.Marshal(streams)
		if err != nil {
			return err
		}

		return c.WriteMessage(websocket.BinaryMessage, response)
	case "add_stream":
		if request.Username == nil {
			response := failure("No username provided for add_stream request")
			return c.WriteMessage(websocket.BinaryMessage, response)
		}

		if request.Stream == nil {
			response := failure("No stream provided for add_stream request")
			return c.WriteMessage(websocket.BinaryMessage, response)
		}

		err := server.AddStream(*request.Username, *request.Stream)
		if err != nil {
			return c.WriteMessage(websocket.BinaryMessage, failure(err.Error()))
		}
		return c.WriteMessage(websocket.BinaryMessage, SuccessResponse)
	case "remove_stream":
		if request.Username == nil {
			resposne := failure("No username provided for remove_stream request")
			return c.WriteMessage(websocket.BinaryMessage, resposne)
		}

		ok := server.RemoveStream(*request.Username)
		if !ok {
			return c.WriteMessage(websocket.BinaryMessage, failure("No such user"))
		}
		return c.WriteMessage(websocket.BinaryMessage, SuccessResponse)
	default:
		return c.WriteMessage(websocket.BinaryMessage, failure("Invalid request type"))
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

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Failed to load .env file:", err)
	}

	addr := os.Getenv("SELUNE_ADDRESS")
	if addr == "" {
		addr = "localhost:8080"
	}

	server := Server{
		m:       sync.Mutex{},
		streams: make(map[Username]Stream),
	}
	upgrader := websocket.Upgrader{}
	http.HandleFunc("/streams", func(rw http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			log.Println("Upgrade error: ", err)
			return
		}

		server.processConnection(c)
	})

	log.Println("Listening on", addr)

	// TODO: TLS
	log.Fatal(http.ListenAndServe(addr, nil))
}
