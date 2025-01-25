//go:build linux && mips64 && !cgo

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

const (
	// Define the Ethernet header structure
	ETH_P_ALL = 0x0003 // Accept all Ethernet frame types
)

// Receiver packet receiver
type Receiver interface {
	Receive() error
}

// Sender packet sender
type Sender interface {
	Send() error
}

func main() {
	fmt.Println("hello world")

	// Create a channel for receiving OS signals
	sigs := make(chan os.Signal, 1)

	// Register the signal handler for SIGINT (Ctrl+C)
	signal.Notify(sigs, syscall.SIGINT)

	src_name := flag.String("src", "eth0", "Source interface to copy packets from")
	//dst_name := flag.String("dst", "ixvnet0", "Destination interface to put raw data from source over ipv4")

	src_iface, err := net.InterfaceByName(*src_name)
	if err != nil {
		fmt.Println("Error getting interface:", err)
		os.Exit(1)
	}

	// dst_iface, err := net.InterfaceByName(*dst_name)
	// if err != nil {
	// 	fmt.Println("Error getting interface:", err)
	// 	os.Exit(1)
	// }

	// Create a raw socket
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, syscall.ETH_P_ALL)
	if err != nil {
		fmt.Println("Error creating socket:", err)
		os.Exit(1)
	}
	defer syscall.Close(fd)

	// Bind the socket to the specific interface
	sll := syscall.SockaddrLinklayer{
		Ifindex:  src_iface.Index,
		Protocol: syscall.ETH_P_ALL,
	}
	if err := syscall.Bind(fd, &sll); err != nil {
		log.Fatalf("Error binding socket to interface %q: %v", *src_name, err)
	}

	// Create a buffer to receive packets
	buf := make([]byte, 2048)

	// Handle graceful shutdown
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP("10.0.1.1"), Port: 19849}, &net.UDPAddr{IP: net.ParseIP("10.0.1.7"), Port: 31982})
	if err != nil {
		fmt.Println("Error creating UDP socket:", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Read packets from the socket
	for {
		select {
		case <-sigs:
			fmt.Println("Received SIGINT. Exiting...")
			return // Exit the program gracefully
		default:
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				log.Printf("Error reading from socket: %v", err)
				continue
			}

			// Send the received packet to the destination
			//fmt.Printf("%s", string(buf[:n]))
			//if n > 0 {
			_, err = conn.Write(buf[:n])
			if err != nil {
				fmt.Println("Error sending data:", err)
			} else {
				fmt.Println("Data sent successfully!")
			}
			//}
			// Process the received packet (e.g., print it)
			// for i := 0; i < n; i++ {
			// 	fmt.Printf("%02x ", buf[i])
			// 	if (i+1)%16 == 0 {
			// 		fmt.Println()
			// 	}
			// }
			// fmt.Println()
		}
	}

}
