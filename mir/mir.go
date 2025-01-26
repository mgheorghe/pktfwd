//go:build linux && mips64 && !cgo

package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
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
}

type UDPSender struct {
	conn    *net.UDPConn
	packets <-chan Packet
}

type Metrics struct {
	RxFrames  uint64
	RxFilter  uint64
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

func NewRawReceiver(ifaceName string, packets chan<- Packet, filter []byte, filterOffset int) (*RawReceiver, error) {
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
		fd:           fd,
		ifIndex:      iface.Index,
		packets:      packets,
		filter:       filter,
		filterOffset: filterOffset,
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
				// Get buffer from pool
				buf := bufferPool.Get().([]byte)

				n, _, err := syscall.Recvfrom(r.fd, buf, 0)
				if err != nil {
					bufferPool.Put(buf) // Return buffer to pool on error
					updateMetrics(func(m *Metrics) {
						m.RxErrors++
					})
					r.packets <- Packet{Err: err}
					continue
				}

				updateMetrics(func(m *Metrics) {
					m.RxFrames++
				})

				//fmt.Printf("Buffer: %x\n", buf[:n])
				//fmt.Printf("Filter: %x\n", r.filter)

				// Apply filter if specified
				if r.filter != nil {
					if r.filterOffset+len(r.filter) > n {
						bufferPool.Put(buf) // Return buffer to pool if filtered
						updateMetrics(func(m *Metrics) {
							m.RxFilter++
						})
						continue
					}

					matched := true
					for i := 0; i < len(r.filter); i++ {
						if buf[r.filterOffset+i] != r.filter[i] {
							//fmt.Printf("B: %x -> F: %x\n", buf[r.filterOffset+i], r.filter[i])
							matched = false
							break
						}
					}

					if !matched {
						bufferPool.Put(buf) // Return buffer to pool if filtered
						updateMetrics(func(m *Metrics) {
							m.RxFilter++
						})
						continue
					}
				}

				// Only copy data if packet passes filter
				data := make([]byte, n)
				copy(data, buf[:n])
				bufferPool.Put(buf) // Return buffer to pool after copying

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
				fmt.Printf("Packets Received: %d\n"+
					"Packets Filtered: %d\n"+
					"Packets Sent: %d\n"+
					"RX Errors: %d\n"+
					"TX Errors: %d\n"+
					"RX Rate: %d\n"+
					"TX Rate: %d\n",
					metrics.RxFrames,
					metrics.RxFilter,
					metrics.TxFrames,
					metrics.RxErrors,
					metrics.TxErrors,
					metrics.RxRate,
					metrics.TxRate)
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
	signal.Notify(sigs, syscall.SIGINT)

	src_iface := flag.String("src-iface", "eth0", "Source interface to copy packets from")
	dst_ip := flag.String("dst-ip", "10.0.1.7", "Destination UDP address")
	dst_port := flag.Int("dst-port", 31982, "Destination UDP address")
	src_ip := flag.String("src-ip", "10.0.1.1", "Source UDP address")
	src_port := flag.Int("src-port", 19849, "Source UDP address")
	filter_str := flag.String("filter", "", "Source UDP address")
	filter_offset := flag.Int("filter-offset", 0, "Source UDP address")
	metrics_enabled := flag.Bool("metrics", false, "Enable metrics collection and reporting")
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

	receiver, err := NewRawReceiver(*src_iface, packets, filter, *filter_offset)
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

	// Start metrics reporter (prints every 1 seconds)
	if *metrics_enabled {
		startMetricsReporter(ctx, time.Second)
	}

	<-sigs
	fmt.Println("Received SIGINT. Exiting...")
}
