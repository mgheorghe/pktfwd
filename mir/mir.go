//go:build linux && !cgo

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
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
	fd           int
	ifIndex      int
	packets      chan<- Packet
	filter       []byte
	filterOffset int
	bmcast       bool
	packetBuffer []byte
}

type UDPSender struct {
	conn    *net.UDPConn
	packets <-chan Packet
}

type Metrics struct {
	RxFrames  uint64
	RxFilter  uint64
	IPv6      uint64
	Multicast uint64
	Broadcast uint64
	TxFrames  uint64
	RxErrors  uint64 // Receive errors
	TxErrors  uint64 // Transmit errors
	RxRate    uint64
	TxRate    uint64
	lastRx    uint64
	lastTx    uint64
	timestamp time.Time
}

var metrics Metrics
var metricsMutex sync.Mutex

func updateMetrics(f func(*Metrics)) {
	metricsMutex.Lock()
	f(&metrics)
	metricsMutex.Unlock()
}

// HostToNetShort converts a 16-bit integer from host to network byte order, aka "htons"
func HostToNetShort(i uint16) uint16 {
	var buf [2]byte
	if runtime.GOARCH == "mips" || runtime.GOARCH == "mips64" {
		binary.BigEndian.PutUint16(buf[:], i)
	} else {
		binary.LittleEndian.PutUint16(buf[:], i)
	}
	return binary.BigEndian.Uint16(buf[:])
}

func NewRawReceiver(ifaceName string, packets chan<- Packet, filter []byte, filterOffset int, bmcast bool) (*RawReceiver, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("error getting interface: %v", err)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(HostToNetShort(ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("error creating socket: %v", err)
	}

	sll := unix.SockaddrLinklayer{
		Ifindex:  iface.Index,
		Protocol: HostToNetShort(ETH_P_ALL),
		Pkttype:  unix.PACKET_HOST,
		Hatype:   unix.ARPHRD_NETROM,
		Halen:    0,
	}
	if err := unix.Bind(fd, &sll); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("error binding socket: %v", err)
	}

	// Set up packet ring buffer
	req := &unix.TpacketReq{}
	req.Block_size = 1 << 22 // 4 MiB
	req.Block_nr = 64
	req.Frame_size = 1 << 11 // 2 KiB
	req.Frame_nr = (1 << 22) * 64 / (1 << 11)
	if err := unix.SetsockoptTpacketReq(fd, unix.SOL_PACKET, unix.PACKET_RX_RING, req); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("error setting up packet ring: %v", err)
	}

	// Set up mmap for zero-copy
	packetBuffer, err := unix.Mmap(fd, 0, int(req.Block_size*req.Block_nr), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("error setting up mmap: %v", err)
	}

	return &RawReceiver{
		fd:           fd,
		ifIndex:      iface.Index,
		packets:      packets,
		filter:       filter,
		bmcast:       bmcast,
		filterOffset: filterOffset,
		packetBuffer: packetBuffer,
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

// Add a sync.Pool for byte buffers
var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, BUFFER_SIZE)
	},
}

func (r *RawReceiver) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Use mmap buffer directly
				n, _, err := unix.Recvfrom(r.fd, r.packetBuffer, 0)
				if err != nil {
					updateMetrics(func(m *Metrics) {
						m.RxErrors++
					})
					r.packets <- Packet{Err: err}
					continue
				}

				updateMetrics(func(m *Metrics) {
					m.RxFrames++
				})

				// Check for broadcast packets
				if r.bmcast && bytes.Equal(r.packetBuffer[:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
					updateMetrics(func(m *Metrics) {
						m.Broadcast++
					})
					// For broadcast packets, skip filter and copy directly
					data := make([]byte, n)
					copy(data, r.packetBuffer[:n])
					r.packets <- Packet{Data: data}
					continue
				}

				// Check for IPv6 packets
				if r.bmcast && n > 14 && r.packetBuffer[12] == 0x86 && r.packetBuffer[13] == 0xDD {
					updateMetrics(func(m *Metrics) {
						m.IPv6++
					})
					if r.packetBuffer[38] == 0xff {
						updateMetrics(func(m *Metrics) {
							m.Multicast++
						})
						// For broadcast packets, skip filter and copy directly
						data := make([]byte, n)
						copy(data, r.packetBuffer[:n])
						r.packets <- Packet{Data: data}
						continue
					}
				}

				// Apply filter if specified
				if r.filter != nil {
					if r.filterOffset+len(r.filter) > n {
						updateMetrics(func(m *Metrics) {
							m.RxFilter++
						})
						continue
					}

					if !bytes.Equal(r.packetBuffer[r.filterOffset:r.filterOffset+len(r.filter)], r.filter) {
						updateMetrics(func(m *Metrics) {
							m.RxFilter++
						})
						continue
					}
				}

				// Only copy data if packet passes filter
				data := make([]byte, n)
				copy(data, r.packetBuffer[:n])

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
					updateMetrics(func(m *Metrics) {
						m.TxErrors++
					})
					continue
				}
				_, err := s.conn.Write(packet.Data)
				if err != nil {
					updateMetrics(func(m *Metrics) {
						m.TxErrors++
					})
				} else {
					updateMetrics(func(m *Metrics) {
						m.TxFrames++
					})
				}
			}
		}
	}()
}

func startMetricsReporter(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				// Calculate rates
				now := time.Now()
				duration := now.Sub(metrics.timestamp).Seconds()
				if duration > 0 {
					metrics.RxRate = uint64(float64(metrics.RxFrames-metrics.lastRx) / duration)
					metrics.TxRate = uint64(float64(metrics.TxFrames-metrics.lastTx) / duration)
				}

				// Update tracking values
				metrics.lastRx = metrics.RxFrames
				metrics.lastTx = metrics.TxFrames
				metrics.timestamp = now

				metricsMutex.Lock()
				fmt.Print("\033[H\033[2J")
				fmt.Printf("%s\n"+
					"Rx Frames : %d\n"+
					"Rx Filter : %d\n"+
					"Tx Frames : %d\n"+
					"Rx Errors : %d\n"+
					"Tx Errors : %d\n"+
					"Rx Rate   : %d\n"+
					"Tx Rate   : %d\n"+
					"IPv6      : %d\n"+
					"Multicast : %d\n"+
					"Broadcast : %d\n",
					os.Args,
					metrics.RxFrames,
					metrics.RxFilter,
					metrics.TxFrames,
					metrics.RxErrors,
					metrics.TxErrors,
					metrics.RxRate,
					metrics.TxRate,
					metrics.IPv6,
					metrics.Multicast,
					metrics.Broadcast)
				metricsMutex.Unlock()
			}
		}
	}()
}

func main() {
	fmt.Println("Starting packet forwarder...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGINT)

	src_iface := flag.String("src-iface", "eth0", "Source interface to copy packets from")
	dst_ip := flag.String("dst-ip", "10.0.1.7", "Destination UDP address")
	dst_port := flag.Int("dst-port", 31982, "Destination UDP address")
	src_ip := flag.String("src-ip", "10.0.1.1", "Source UDP address")
	src_port := flag.Int("src-port", 19849, "Source UDP address")
	filter_str := flag.String("filter", "", "Source UDP address")
	filter_offset := flag.Int("filter-offset", 0, "Source UDP address")
	metrics_enabled := flag.Bool("metrics", false, "Enable metrics collection and reporting")
	bmcast := flag.Bool("bmcast", false, "allow broadcast and multicast packets")
	flag.Parse()

	hexStr := *filter_str
	filter, err := hex.DecodeString(hexStr)
	if err != nil {
		// handle error
		log.Fatalf("Failed to decode hex string: %v", err)
	}

	dst_addr := fmt.Sprintf("%s:%d", *dst_ip, *dst_port)
	src_addr := fmt.Sprintf("%s:%d", *src_ip, *src_port)

	packets := make(chan Packet, CHANNEL_SIZE)

	receiver, err := NewRawReceiver(*src_iface, packets, filter, *filter_offset, *bmcast)
	if err != nil {
		log.Fatalf("Error creating receiver: %v", err)
	}
	defer unix.Close(receiver.fd)

	sender, err := NewUDPSender(src_addr, dst_addr, packets)
	if err != nil {
		log.Fatalf("Error creating sender: %v", err)
	}
	defer sender.conn.Close()

	receiver.Start(ctx)
	sender.Start(ctx)

	// Start metrics reporter (prints every 1 seconds)
	if *metrics_enabled {
		startMetricsReporter(ctx, time.Second)
	}

	<-sigs
	fmt.Println("Received SIGINT. Exiting...")

	// Print memory statistics
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	fmt.Printf("Alloc = %v MiB", bToMb(memStats.Alloc))
	fmt.Printf("\tTotalAlloc = %v MiB", bToMb(memStats.TotalAlloc))
	fmt.Printf("\tSys = %v MiB", bToMb(memStats.Sys))
	fmt.Printf("\tNumGC = %v\n", memStats.NumGC)
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
