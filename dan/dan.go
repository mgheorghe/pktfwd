package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

const (
	ETH_P_ALL = 0x0003
	BUFFER_SIZE = 1024
)

type Receiver interface {
	Receive() ([]byte, error)
}

type Sender interface {
	Send([]byte) error
}

type UDPReceiver struct {
	conn *net.UDPConn
}

func NewUDPReceiver(addr string) (*UDPReceiver, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	
	return &UDPReceiver{conn: conn}, nil
}

func (r *UDPReceiver) Receive() ([]byte, error) {
	buf := make([]byte, BUFFER_SIZE)
	n, _, err := r.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (r *UDPReceiver) Close() error {
	return r.conn.Close()
}

type RawSender struct {
	fd      int
	ifIndex int
}

func NewRawSender(ifaceName string) (*RawSender, error) {
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

	return &RawSender{fd: fd, ifIndex: iface.Index}, nil
}

func (s *RawSender) Send(data []byte) error {
	_, err := syscall.Write(s.fd, data)
	return err
}

func (s *RawSender) Close() error {
	return syscall.Close(s.fd)
}

func main() {
	fmt.Println("decap")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	dst_iface := flag.String("dst", "eth0", "Source interface to copy packets from")
	src_ip := flag.String("scr-ip", "10.0.1.7", "Source interface to copy packets from")
	src_port := flag.Int("scr-port", 31982, "Source interface to copy packets from")
	flag.Parse()

	receiver, err := NewUDPReceiver("%s:%d", *dest_ip, *dest_port)
	if err != nil {
		log.Fatalf("Error creating receiver: %v", err)
	}
	defer receiver.Close()

	sender, err := NewRawSender(*dst_iface)
	if err != nil {
		log.Fatalf("Error creating sender: %v", err)
	}
	defer sender.Close()

	fmt.Println("Listening on %s:%d", *dest_ip, *dest_port)

	for {
		select {
		case <-sigs:
			fmt.Println("Received SIGINT. Exiting...")
			return
		default:
			data, err := receiver.Receive()
			if err != nil {
				fmt.Println("Error receiving:", err)
				continue
			}

			if err := sender.Send(data); err != nil {
				fmt.Println("Error sending data:", err)
			} else {
				fmt.Println("Packet sent successfully!")
			}
		}
	}
}
