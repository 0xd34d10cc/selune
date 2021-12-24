package main

import (
	"bytes"
	"context"
	"log"
	"net"
	"selune/proto"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

const bufferSize = 1024 * 1024

func runServer(t *testing.T) (*grpc.Server, *bufconn.Listener) {
	s, err := NewServer()
	if err != nil {
		t.Error(err)
	}

	listener := bufconn.Listen(bufferSize)
	server := grpc.NewServer()
	proto.RegisterSeluneServer(server, s)
	go func() {
		err := server.Serve(listener)
		if err != nil {
			log.Fatal("gRPC server exited:", err)
		}
	}()

	return server, listener
}

func connect(t *testing.T, listener *bufconn.Listener) (*grpc.ClientConn, proto.SeluneClient) {
	conn, err := grpc.Dial("bufnet", grpc.WithInsecure(), grpc.WithDialer(func(s string, d time.Duration) (net.Conn, error) {
		return listener.Dial()
	}))

	if err != nil {
		t.Fatal(err)
	}

	return conn, proto.NewSeluneClient(conn)
}

func connectAndGoOnline(t *testing.T, listener *bufconn.Listener, user *proto.User) (*grpc.ClientConn, proto.SeluneClient, proto.Selune_GoOnlineClient) {
	conn, client := connect(t, listener)
	notifications, err := client.GoOnline(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}

	notification, err := notifications.Recv()
	if err != nil {
		t.Fatal(err)
	}

	if notification.GetStatus() != nil {
		t.Error(notification.GetStatus().Description)
	}

	if notification.GetInitiator() != nil {
		t.Error("Unexpected new connection. Expected user id in first notification")
	}

	if notification.Id == 0 {
		t.Error("Expected non-zero user id in first notificatiion")
	}

	user.Id = notification.Id
	return conn, client, notifications
}

func assertUsersEqual(t *testing.T, a, b *proto.User) {
	if a.GetId() != b.GetId() {
		t.Fatalf("%v != %v", a.GetId(), b.GetId())
	}

	if a.GetUsername() != b.GetUsername() {
		t.Fatalf("%v != %v", a.GetUsername(), b.GetUsername())
	}

	if bytes.Compare(a.GetAddress(), b.GetAddress()) != 0 {
		t.Fatalf("%v != %v", a.GetAddress(), b.GetAddress())
	}
}

func TestGoOnline(t *testing.T) {
	server, listener := runServer(t)
	defer server.Stop()

	testUser := proto.User{
		Username: "test_user",
		Address:  []byte("test user address"),
	}

	conn, client, _ := connectAndGoOnline(t, listener, &testUser)
	defer conn.Close()

	response, err := client.ListOnlineUsers(context.Background(), &proto.UserListRequest{})
	if err != nil {
		t.Fatal(err)
	}

	users := response.GetUsers()
	if len(users) != 1 {
		t.Fatalf("Unexpected number of users (%v) in list: %v\n", len(users), users)
	}

	assertUsersEqual(t, &testUser, users[0])

	err = conn.Close()
	if err != nil {
		log.Fatal(err)
	}

	conn, client = connect(t, listener)
	defer conn.Close()
	response, err = client.ListOnlineUsers(context.Background(), &proto.UserListRequest{})
	if err != nil {
		t.Fatal(err)
	}

	users = response.GetUsers()
	if len(users) != 0 {
		t.Fatalf("test_user should not be online (%v active users): %v", len(users), users)
	}
}

func TestConnect(t *testing.T) {
	server, listener := runServer(t)
	defer server.Stop()

	alice := proto.User{
		Username: "Alice",
		Address:  []byte("Alice address"),
	}
	aliceConn, aliceClient, _ := connectAndGoOnline(t, listener, &alice)
	defer aliceConn.Close()

	bob := proto.User{
		Username: "Bob",
		Address:  []byte("Bob address'"),
	}
	bobConn, _, bobNotifications := connectAndGoOnline(t, listener, &bob)
	defer bobConn.Close()

	// Alice wants to connect to Bob
	status, err := aliceClient.Connect(context.Background(), &proto.ConnectionRequest{
		InitiatorId: alice.GetId(),
		TargetId:    bob.GetId(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if status.GetDescription() != "" {
		log.Fatal(status.GetDescription())
	}

	notification, err := bobNotifications.Recv()
	if err != nil {
		t.Fatal(err)
	}

	if notification.GetStatus() != nil {
		t.Fatal(notification.GetStatus().GetDescription())
	}

	assertUsersEqual(t, &alice, notification.Initiator)
}
