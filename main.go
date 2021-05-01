package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
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
	users    *mongo.Collection

	m         sync.Mutex
	streamers map[StreamID]chan Response
}

// Auth info for each user
type User struct {
	ID        string    `json:"id" sql:"id"`
	Username  string    `json:"username" sql:"username"`
	TokenHash string    `json:"tokenhash" sql:"tokenhash"`
	CreatedAt time.Time `json:"createdat" sql:"createdat"`
	UpdatedAt time.Time `json:"updatedat" sql:"updatedat"`
}

// AccessTokenCustomClaims specifies the claims for access token
type AccessTokenCustomClaims struct {
	UserID  string
	KeyType string
	jwt.StandardClaims
}

type RefreshTokenCustomClaims struct {
	UserID    string
	CustomKey string
	KeyType   string
	jwt.StandardClaims
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

	// auth fields
	User         string `json:"user,omitempty"`
	Password     string `json:"password,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type Response struct {
	Status      string              `json:"status"`
	Message     string              `json:"message,omitempty"`
	StreamID    StreamID            `json:"stream_id,omitempty"`
	Streams     map[StreamID]Stream `json:"streams,omitempty"`
	Destination string              `json:"destination,omitempty"`

	// auth fields
	RefreshToken string `json:"refresh_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
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
	users := dbClient.Database("selune").Collection("users")

	server := &Server{
		mux:       nil,
		dbClient:  dbClient,
		streams:   streams,
		users:     users,
		m:         sync.Mutex{},
		streamers: make(map[StreamID]chan Response),
	}

	mux := http.NewServeMux()
	upgrader := websocket.Upgrader{}
	mux.HandleFunc(path, func(rw http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			log.Printf("[%v] Failed to upgrade to websocket: %v", r.RemoteAddr, err)
			return
		}
		session := server.Accept(c)
		defer session.Close()
		session.Run()
	})

	server.mux = mux
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

const TokenDuration = 1200
const AccessTokenPrivateKeyPath = "access.pem"
const AccessTokenPublicKeyPath = "access.pem.pub"
const RefreshTokenPrivateKeyPath = "refresh.pem"
const RefreshTokenPublicKeyPath = "refresh.pem.pub"

func GenerateAccessToken(user *User) (string, error) {
	claims := AccessTokenCustomClaims{
		user.ID,
		"access",
		jwt.StandardClaims{
			ExpiresAt: time.Now().Add(time.Minute * time.Duration(TokenDuration)).Unix(),
			Issuer:    "bookite.auth.service",
		},
	}
	signBytes, err := ioutil.ReadFile(AccessTokenPrivateKeyPath)
	if err != nil {
		log.Printf("unable to read private key", "error", err)
		return "", errors.New("could not generate access token. please try again later")
	}

	signKey, err := jwt.ParseRSAPrivateKeyFromPEM(signBytes)
	if err != nil {
		log.Printf("unable to parse private key", "error", err)
		return "", errors.New("could not generate access token. please try again later")
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	return token.SignedString(signKey)
}

func ValidateAccessToken(tokenString string) (string, error) {
	token, err := jwt.ParseWithClaims(tokenString, &AccessTokenCustomClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			log.Printf("Unexpected signing method in auth token")
			return nil, errors.New("Unexpected signing method in auth token")
		}
		verifyBytes, err := ioutil.ReadFile(AccessTokenPublicKeyPath)
		if err != nil {
			log.Printf("unable to read public key", "error", err)
			return nil, err
		}

		verifyKey, err := jwt.ParseRSAPublicKeyFromPEM(verifyBytes)
		if err != nil {
			log.Printf("unable to parse public key", "error", err)
			return nil, err
		}

		return verifyKey, nil
	})

	if err != nil {
		log.Printf("unable to parse claims", "error", err)
		return "", err
	}

	claims, ok := token.Claims.(*AccessTokenCustomClaims)
	if !ok || !token.Valid || claims.UserID == "" || claims.KeyType != "access" {
		return "", errors.New("invalid token: authentication failed")
	}
	return claims.UserID, nil
}

func GenerateRefreshToken(user *User) (string, error) {
	// TODO: actually generate custom key from id and hash
	cusKey := user.ID
	tokenType := "refresh"

	claims := RefreshTokenCustomClaims{
		user.ID,
		cusKey,
		tokenType,
		jwt.StandardClaims{
			Issuer: "bookite.auth.service",
		},
	}

	signBytes, err := ioutil.ReadFile(RefreshTokenPrivateKeyPath)
	if err != nil {
		log.Printf("unable to read private key", "error", err)
		return "", errors.New("could not generate refresh token. please try again later")
	}

	signKey, err := jwt.ParseRSAPrivateKeyFromPEM(signBytes)
	if err != nil {
		log.Printf("unable to parse private key", "error", err)
		return "", errors.New("could not generate refresh token. please try again later")
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	return token.SignedString(signKey)
}

func ValidateRefreshToken(tokenString string) (string, string, error) {

	token, err := jwt.ParseWithClaims(tokenString, &RefreshTokenCustomClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			log.Printf("Unexpected signing method in auth token")
			return nil, errors.New("Unexpected signing method in auth token")
		}
		verifyBytes, err := ioutil.ReadFile(RefreshTokenPublicKeyPath)
		if err != nil {
			log.Printf("unable to read public key", "error", err)
			return nil, err
		}

		verifyKey, err := jwt.ParseRSAPublicKeyFromPEM(verifyBytes)
		if err != nil {
			log.Printf("unable to parse public key", "error", err)
			return nil, err
		}

		return verifyKey, nil
	})

	if err != nil {
		log.Printf("unable to parse claims", "error", err)
		return "", "", err
	}

	claims, ok := token.Claims.(*RefreshTokenCustomClaims)
	if !ok || !token.Valid || claims.UserID == "" || claims.KeyType != "refresh" {
		log.Printf("could not extract claims from token")
		return "", "", errors.New("invalid token: authentication failed")
	}
	return claims.UserID, claims.CustomKey, nil
}

func (c *Session) processLogin(request *Request) error {
	// TODO: use userID arg as userID

	if request.User == "" || request.Password == "" {
		return fmt.Errorf("login requested, but no password or username provided")
	}

	res := c.server.users.FindOne(context.Background(), bson.M{
		"username": request.User,
		"password": request.Password,
	})

	if res.Err() == mongo.ErrNilDocument {
		return fmt.Errorf("no user with such username and password")
	}

	user := User{}
	user.Username = request.User
	user.ID = request.User
	accessToken, err := GenerateAccessToken(&user)

	if err != nil {
		return err
	}

	refreshToken, err := GenerateRefreshToken(&user)

	if err != nil {
		return err
	}

	log.Printf("Sucksexfully generated access token: %s and refresh token: %s for user: %s", accessToken, refreshToken, user.Username)

	c.sendResponse(Response{
		Status:       "success",
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	})

	return nil
}

func (c *Session) processSignUpRequest(request *Request) error {
	if request.Password == "" || request.User == "" {
		return fmt.Errorf("signup requested but no password or username provided")
	}

	// TODO: Hash password before signuping
	// TODO: Use first arg as userID for token generation
	_, err := c.server.users.InsertOne(context.Background(), bson.M{
		"username": request.User,
		"password": request.Password,
	})

	if err != nil {
		return err
	}

	c.sendResponse(Response{
		Status: "success",
	})

	return nil
}

func (c *Session) processRefreshToken(request *Request) error {
	if request.RefreshToken == "" {
		return fmt.Errorf("Refresh access token is requested, but no refresh token is provided")
	}

	// Since we are using no userid (same as username)  we do not need to check it and custom key
	userId, _, err := ValidateRefreshToken(request.RefreshToken)
	if err != nil {
		return err
	}

	user := User{}
	user.ID = userId

	accessToken, err := GenerateAccessToken(&user)

	if err != nil {
		return err
	}

	c.sendResponse(Response{
		Status:      "success",
		AccessToken: accessToken,
	})

	return nil
}

func (c *Session) processTokenlessRequest(request *Request) error {
	switch request.Type {
	case "login":
		err := c.processLogin(request)
		if err != nil {
			c.sendFail(err.Error())
		}
	case "sign_up":
		err := c.processSignUpRequest(request)
		if err != nil {
			c.sendFail(err.Error())
		}
	case "refresh_token":
		err := c.processRefreshToken(request)
		if err != nil {
			c.sendFail(err.Error())
		}
	default:
		return fmt.Errorf("you need to log in before sending requests regarding streams")
	}
	return nil
}

func (c *Session) processRequest(request Request) error {
	c.Log("Received \"%v\" request", request.Type)

	// handle connections wthout tokens
	if request.AccessToken == "" {
		err := c.processTokenlessRequest(&request)
		return err
	}

	// for now userId is same as username, so skip first param
	// TODO: fix that we use username as userid
	_, err := ValidateAccessToken(request.AccessToken)

	if err != nil {
		c.sendFail(err.Error())
	}

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
