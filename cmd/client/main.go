package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"selune/proto"
	"time"

	"github.com/pion/ice/v2"
	"github.com/pion/stun"
	"google.golang.org/grpc"
)

func getPublicAddr(conn *net.UDPConn) (*net.UDPAddr, error) {
	serverAddr, err := net.ResolveUDPAddr("udp4", "stun1.l.google.com:19302")
	if err != nil {
		return nil, err
	}

	message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	n, err := conn.WriteToUDP(message.Raw, serverAddr)
	if n != len(message.Raw) {
		return nil, errors.New("Incomplete write")
	}

	if err != nil {
		return nil, err
	}

	var buf [4096]byte
	n, sender, err := conn.ReadFromUDP(buf[:])
	if err != nil {
		log.Fatal("Read failed:", err)
	}

	if !sender.IP.Equal(serverAddr.IP) || sender.Port != serverAddr.Port {
		err = errors.New(fmt.Sprintf("Unexpected UDP message from %v", *sender))
		return nil, err
	}

	if !stun.IsMessage(buf[:n]) {
		log.Fatal("Invalid stun response")
	}

	m := new(stun.Message)
	m.Raw = buf[:n]
	err = m.Decode()
	if err != nil {
		return nil, err
	}

	var addr stun.XORMappedAddress
	err = addr.GetFrom(m)
	if err != nil {
		return nil, err
	}

	return &net.UDPAddr{
		IP:   addr.IP,
		Port: addr.Port,
	}, nil
}

// Get preferred outbound ip of this machine
func getOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP
}

func check() {
	// localIP := getOutboundIP()
	// conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP, Port: 1337})
	// if err != nil {
	// 	log.Fatal("Failed to create UDP socket:", err)
	// }
	// defer conn.Close()

	// localAddr := conn.LocalAddr().(*net.UDPAddr)
	// log.Println("Local address (socket):", localAddr)

	// publicAddr, err := getPublicAddr(conn)
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// log.Println("Your public address according to STUN server is", publicAddr)

	// // connect to router
	// d, err := upnp.Discover()
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// // discover external IP
	// ip, err := d.ExternalIP()
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// log.Println("Your external IP according to UPnP is:", ip)

	// Add port mapping
	// err = d.Forward(9001, "Selune test")
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// Remove port mapping
	// d.Clear(9001)
}

//nolint
var (
	isControlling bool
	iceAgent      *ice.Agent
)

func main() { //nolint
	var (
		err  error
		conn *ice.Conn
	)

	flag.BoolVar(&isControlling, "controlling", false, "is ICE Agent controlling")
	flag.Parse()

	var username string
	if isControlling {
		username = "controlling"
		fmt.Println("Local Agent is controlling")
	} else {
		username = "controlled"
		fmt.Println("Local Agent is controlled")
	}
	fmt.Print("Press 'Enter' when both processes have started")
	if _, err = bufio.NewReader(os.Stdin).ReadBytes('\n'); err != nil {
		panic(err)
	}

	stunUrl, err := ice.ParseURL("stun:stun2.l.google.com:19302")
	if err != nil {
		panic(err)
	}

	iceAgent, err = ice.NewAgent(&ice.AgentConfig{
		NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4},
		Urls:         []*ice.URL{stunUrl},
	})
	if err != nil {
		panic(err)
	}

	candidates := make([]ice.Candidate, 0)
	done := make(chan struct{})
	// When we have gathered a new ICE Candidate send it to the remote peer
	if err = iceAgent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			done <- struct{}{}
			return
		}

		candidates = append(candidates, c)
	}); err != nil {
		panic(err)
	}

	log.Println("Waiting while candidates are gathering...")
	if err = iceAgent.GatherCandidates(); err != nil {
		panic(err)
	}

	// wait for candidates to gather
	<-done

	log.Println("Candidates:", candidates)

	// When ICE Connection state has change print to stdout
	if err = iceAgent.OnConnectionStateChange(func(c ice.ConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", c.String())
	}); err != nil {
		panic(err)
	}

	// Get the local auth details and send to remote peer
	localUfrag, localPwd, err := iceAgent.GetLocalUserCredentials()
	if err != nil {
		panic(err)
	}

	log.Println("Local ufrag:", localUfrag)
	log.Println("Local pwd:", localPwd)

	type ConnectionInfo struct {
		Candidates []string `json:"candidates"`
		Ufrag      string   `json:"ufrag"`
		Pwd        string   `json:"pwd"`
	}

	candidatesList := make([]string, 0)
	for _, candidate := range candidates {
		candidatesList = append(candidatesList, candidate.Marshal())
	}

	connectionInfo := ConnectionInfo{
		Candidates: candidatesList,
		Ufrag:      localUfrag,
		Pwd:        localPwd,
	}

	address, err := json.Marshal(&connectionInfo)
	if err != nil {
		panic(err)
	}

	seluneConn, err := grpc.Dial("localhost:8088", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}

	seluneClient := proto.NewSeluneClient(seluneConn)
	notifications, err := seluneClient.GoOnline(context.TODO(), &proto.User{
		Username: username,
		Address:  address,
	})

	newConn, err := notifications.Recv()
	if err != nil {
		panic(err)
	}

	if newConn.GetStatus().GetDescription() != "" {
		panic(newConn.Status.Description)
	}

	userID := newConn.GetId()
	log.Println("My id is:", userID)

	var remoteInfo ConnectionInfo
	if isControlling {
		// wait for controlled user to go online
		var controlled *proto.User
		for {
			users, err := seluneClient.ListOnlineUsers(context.TODO(), &proto.UserListRequest{})
			if err != nil {
				panic(err)
			}

			for _, user := range users.GetUsers() {
				if user.Username != username {
					controlled = user
				}
			}

			if controlled != nil {
				break
			}
		}

		err = json.Unmarshal(controlled.Address, &remoteInfo)
		if err != nil {
			panic(err)
		}

		log.Println("Connecting to:", controlled)
		status, err := seluneClient.Connect(context.TODO(), &proto.ConnectionRequest{
			InitiatorId: userID,
			TargetId:    controlled.GetId(),
		})
		if err != nil {
			panic(err)
		}

		if status.Description != "" {
			panic(status.Description)
		}

	} else {
		log.Println("Waiting for new connection...")

		newConn, err = notifications.Recv()
		if err != nil {
			panic(err)
		}
		if newConn.GetStatus().GetDescription() != "" {
			panic(newConn.Status.Description)
		}

		log.Println("New connection:", newConn)

		err = json.Unmarshal(newConn.Initiator.Address, &remoteInfo)
		if err != nil {
			panic(err)
		}
	}

	remoteUfrag := remoteInfo.Ufrag
	remotePwd := remoteInfo.Pwd
	for _, candidate := range remoteInfo.Candidates {
		c, err := ice.UnmarshalCandidate(candidate)
		if err != nil {
			panic(err)
		}
		iceAgent.AddRemoteCandidate(c)
	}

	// Start the ICE Agent. One side must be controlled, and the other must be controlling
	if isControlling {
		conn, err = iceAgent.Dial(context.TODO(), remoteUfrag, remotePwd)
	} else {
		conn, err = iceAgent.Accept(context.TODO(), remoteUfrag, remotePwd)
	}

	if err != nil {
		panic(err)
	}

	// Send messages in a loop to the remote peer
	go func() {
		idx := 0
		for {
			time.Sleep(time.Second * 3)
			val := fmt.Sprintf("%v", idx)
			idx += 1
			if _, err = conn.Write([]byte(val)); err != nil {
				panic(err)
			}

			fmt.Printf("Sent: '%s'\n", val)
		}
	}()

	// Receive messages in a loop from the remote peer
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			panic(err)
		}

		fmt.Printf("Received: '%s'\n", string(buf[:n]))
	}
}
