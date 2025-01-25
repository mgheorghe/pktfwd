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
	ETH_P_ALL    = 0x0003
	BUFFER_SIZE  = 2048
	CHANNEL_SIZE = 1000
)

type Packet struct {
	Data []byte
	Err  error
}

type RawReceiver struct {
	fd      int
	ifIndex int
	packets chan<- Packet
}

type UDPSender struct {
	conn    *net.UDPConn
	packets <-chan Packet
}

func NewRawReceiver(ifaceName string, packets chan<- Packet) (*RawReceiver, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("error getting interface: %v", err)
	}

	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, syscall.ETH_P_ALL)
	if err != nil {
		return nil, fmt.Errorf("error creating socket: %v", err)
	}

	sll := syscall.SockaddrLinklayer{
		Ifindex:  iface.Index,
		Protocol: syscall.ETH_P_ALL,
	}
	if err := syscall.Bind(fd, &sll); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("error binding socket: %v", err)
	}

	return &RawReceiver{
		fd:      fd,
		ifIndex: iface.Index,
		packets: packets,
	}, nil
}

func NewUDPSender(srcAddr, dstAddr string, packets <-chan Packet) (*UDPSender, error) {
	srcUDPAddr, err := net.ResolveUDPAddr("udp", srcAddr)
	if err != nil {
		return nil, err
	}
	dstUDPAddr, err := net.ResolveUDPAddr("udp", dstAddr)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp", srcUDPAddr, dstUDPAddr)
	if err != nil {
		return nil, err
	}

	return &UDPSender{
		conn:    conn,
		packets: packets,
	}, nil
}

func (r *RawReceiver) Start(ctx context.Context) {
	go func() {
		buf := make([]byte, BUFFER_SIZE)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				n, _, err := syscall.Recvfrom(r.fd, buf, 0)
				if err != nil {
					r.packets <- Packet{Err: err}
					continue
				}
				data := make([]byte, n)
				copy(data, buf[:n])
				r.packets <- Packet{Data: data}
			}
		}
	}()
}

func (s *UDPSender) Start(ctx context.Context) {
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
				_, err := s.conn.Write(packet.Data)
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
	fmt.Println("Starting packet forwarder...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	src_iface := flag.String("src-iface", "eth0", "Source interface to copy packets from")
	dst_ip := flag.String("dst-ip", "10.0.1.7", "Destination UDP address")
	dst_port := flag.Int("dst-port", 31982, "Destination UDP address")
	src_ip := flag.String("src-ip", "10.0.1.1", "Source UDP address")
	src_port := flag.Int("src-port", 19849, "Source UDP address")
	flag.Parse()

	dst_addr := fmt.Sprintf("%s:%d", *dst_ip, *dst_port)
	src_addr := fmt.Sprintf("%s:%d", *src_ip, *src_port)

	packets := make(chan Packet, CHANNEL_SIZE)

	receiver, err := NewRawReceiver(*src_iface, packets)
	if err != nil {
		log.Fatalf("Error creating receiver: %v", err)
	}
	defer syscall.Close(receiver.fd)

	sender, err := NewUDPSender(src_addr, dst_addr, packets)
	if err != nil {
		log.Fatalf("Error creating sender: %v", err)
	}
	defer sender.conn.Close()

	receiver.Start(ctx)
	sender.Start(ctx)

	fmt.Printf("Forwarding packets from %s to %s\n", *src_iface, dst_addr)
	<-sigs
	fmt.Println("Received SIGINT. Exiting...")
}
