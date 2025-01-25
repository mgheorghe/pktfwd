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
	ETH_P_ALL    = 0x0003
	BUFFER_SIZE  = 1024
	CHANNEL_SIZE = 1000
)

type Packet struct {
	Data []byte
	Err  error
}

type UDPReceiver struct {
	conn    *net.UDPConn
	packets chan<- Packet
}

func NewUDPReceiver(addr string, packets chan<- Packet) (*UDPReceiver, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	
	return &UDPReceiver{
		conn:    conn,
		packets: packets,
	}, nil
}

func (r *UDPReceiver) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				buf := make([]byte, BUFFER_SIZE)
				n, _, err := r.conn.ReadFromUDP(buf)
				if err != nil {
					r.packets <- Packet{Err: err}
					continue
				}
				r.packets <- Packet{Data: buf[:n]}
			}
		}
	}()
}

type RawSender struct {
	fd       int
	ifIndex  int
	packets  <-chan Packet
}

func NewRawSender(ifaceName string, packets <-chan Packet) (*RawSender, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, err
	}

	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, syscall.ETH_P_ALL)
	if err != nil {
		return nil, err
	}

	sll := syscall.SockaddrLinklayer{
		Ifindex:  iface.Index,
		Protocol: syscall.ETH_P_ALL,
	}

	if err := syscall.Bind(fd, &sll); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	return &RawSender{
		fd:      fd,
		ifIndex: iface.Index,
		packets: packets,
	}, nil
}

func (s *RawSender) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case packet := <-s.packets:
				if packet.Err != nil {
					fmt.Println("Received error:", packet.Err)
					continue
				}
				
				_, err := syscall.Write(s.fd, packet.Data)
				if err != nil {
					fmt.Println("Error sending data:", err)
				} else {
					fmt.Println("Packet sent successfully!")
				}
			}
		}
	}()
}

func main() {
	fmt.Println("decap")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	dst_iface := flag.String("dst", "eth0", "Source interface to copy packets from")
	src_ip := flag.String("scr-ip", "10.0.1.7", "Source interface to copy packets from")
	src_port := flag.Int("scr-port", 31982, "Source interface to copy packets from")
	flag.Parse()

	packets := make(chan Packet, CHANNEL_SIZE)

	source := fmt.Sprintf("%s:%d", *dest_ip, *dest_port)
	receiver, err := NewUDPReceiver(source, packets)
	if err != nil {
		log.Fatalf("Error creating receiver: %v", err)
	}
	defer receiver.conn.Close()

	sender, err := NewRawSender(*dst_iface, packets)
	if err != nil {
		log.Fatalf("Error creating sender: %v", err)
	}
	defer syscall.Close(sender.fd)

	fmt.Println("Listening on 10.0.1.7:31982")

	receiver.Start(ctx)
	sender.Start(ctx)

	<-sigs
	fmt.Println("Received SIGINT. Exiting...")
}
