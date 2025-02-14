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
	"unsafe"

	"golang.org/x/sys/unix"
)

// ETH_P_ALL captures all protocols
const (
	BUFFER_SIZE  = 2048
	MAX_BUFFER   = 16384
	CHANNEL_SIZE = 1000
	RING_SIZE    = (1 << 22) * 64 / (1 << 11) // 32K frames
)

var ETH_P_ALL uint16

//var logger *log.Logger

func init() {
	if runtime.GOARCH == "mips" || runtime.GOARCH == "mips64" {
		ETH_P_ALL = 0x5FFF // ETH_P_zzzz_CAPTURE
	} else {
		ETH_P_ALL = 0x0003 //unix.ETH_P_ALL
	}
	// logFile, err := os.OpenFile("pktfwd.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// logger = log.New(logFile, "", log.LstdFlags)
}

// Packet represents a network packet with data and error
type Packet struct {
	Data []byte
	Err  error
}

// RawReceiver handles raw packet reception
type RawReceiver struct {
	fd           int
	ifIndex      int
	packets      chan<- Packet
	filter       []byte
	filterOffset int
	bmcast       bool
	packetBuffer []byte
	done         chan struct{}
}

// UDPSender handles UDP packet transmission
type UDPSender struct {
	conn    *net.UDPConn
	packets <-chan Packet
}

// Metrics tracks packet statistics
type Metrics struct {
	RxFrames  uint64
	RxFilter  uint64
	IPv6      uint64
	Multicast uint64
	Broadcast uint64
	TxFrames  uint64
	RxErrors  uint64
	TxErrors  uint64
	RxRate    uint64
	TxRate    uint64
	lastRx    uint64
	lastTx    uint64
	buffer    int
	BufFull   uint64
	timestamp time.Time
}

var metrics Metrics
var metricsMutex sync.Mutex

func updateMetrics(f func(*Metrics)) {
	metricsMutex.Lock()
	f(&metrics)
	metricsMutex.Unlock()
}

// HostToNetShort converts host to network byte order
func HostToNetShort(i uint16) uint16 {
	var buf [2]byte
	if runtime.GOARCH == "mips" || runtime.GOARCH == "mips64" {
		binary.BigEndian.PutUint16(buf[:], i)
	} else {
		binary.LittleEndian.PutUint16(buf[:], i)
	}
	return binary.BigEndian.Uint16(buf[:])
}

// bToMb converts bytes to megabytes
func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}

func getGrowingBuffer(size int) []byte {
	if size > MAX_BUFFER {
		updateMetrics(func(m *Metrics) {
			m.BufFull++
		})
		return nil
	}
	buf := bufferPool.Get().([]byte)
	if cap(buf) < size {
		updateMetrics(func(m *Metrics) {
			m.buffer = size
		})

		// Return old buffer to pool
		bufferPool.Put(buf)
		// Create new larger buffer
		return make([]byte, size)
	}
	return buf[:size]
}

func (r *RawReceiver) forwardPacket(packetData []byte, header *unix.TpacketHdr) {
	data := getGrowingBuffer(len(packetData))
	if data == nil {
		header.Status = unix.TP_STATUS_KERNEL
		return
	}
	copy(data, packetData)

	select {
	case <-r.done:
		return
	case r.packets <- Packet{Data: data}:
		header.Status = unix.TP_STATUS_KERNEL
	}
}

// NewRawReceiver creates raw socket receiver
func NewRawReceiver(ifaceName string, packets chan<- Packet, filter []byte, filterOffset int, bmcast bool) (*RawReceiver, error) {

	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("error getting interface: %v", err)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(HostToNetShort(ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("error creating socket: %v", err)
	}
	// Add cleanup in case of errors
	defer func() {
		if err != nil {
			unix.Close(fd)
		}
	}()

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 1024*1024); err != nil {
		return nil, fmt.Errorf("error setting SO_RCVBUF: %v", err)
	}

	sll := unix.SockaddrLinklayer{
		Ifindex:  iface.Index,
		Protocol: HostToNetShort(ETH_P_ALL),
	}
	if err := unix.Bind(fd, &sll); err != nil {
		return nil, fmt.Errorf("error binding socket: %v", err)
	}

	// Set up packet ring buffer
	req := &unix.TpacketReq{
		Block_size: 1 << 22, // 4 MiB
		Block_nr:   64,
		Frame_size: 1 << 11, // 16 KiB
		Frame_nr:   RING_SIZE,
	}
	if err := unix.SetsockoptTpacketReq(fd, unix.SOL_PACKET, unix.PACKET_RX_RING, req); err != nil {
		return nil, fmt.Errorf("error setting up packet ring: %v", err)
	}

	// Set up mmap for zero-copy
	packetBuffer, err := unix.Mmap(fd, 0, int(req.Block_size*req.Block_nr), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("error setting up mmap: %v", err)
	}
	// Add cleanup in case of errors
	defer func() {
		if err != nil {
			unix.Munmap(packetBuffer)
		}
	}()

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

func (r *RawReceiver) Close() error {
	if err := unix.Munmap(r.packetBuffer); err != nil {
		return fmt.Errorf("error unmapping packet buffer: %v", err)
	}
	if err := unix.Close(r.fd); err != nil {
		return fmt.Errorf("error closing socket: %v", err)
	}
	return nil
}

// NewUDPSender creates UDP packet sender
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

func (r *RawReceiver) filterPacket(packetData []byte, packetLen uint32, header *unix.TpacketHdr) bool {
	// Check for broadcast packets
	if r.bmcast && bytes.Equal(packetData[:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		updateMetrics(func(m *Metrics) {
			m.Broadcast++
		})
		r.forwardPacket(packetData, header)
		return true
	}

	// Check for IPv6 packets
	if r.bmcast && packetLen > 14 && packetData[12] == 0x86 && packetData[13] == 0xDD {
		updateMetrics(func(m *Metrics) {
			m.IPv6++
		})
		if packetData[38] == 0xff {
			updateMetrics(func(m *Metrics) {
				m.Multicast++
			})
			r.forwardPacket(packetData, header)
			return true
		}
	}

	// Apply filter if specified
	if r.filter != nil {
		if r.filterOffset+len(r.filter) > int(packetLen) {
			updateMetrics(func(m *Metrics) {
				m.RxFilter++
			})
			header.Status = unix.TP_STATUS_KERNEL
			return true
		}

		if !bytes.Equal(packetData[r.filterOffset:r.filterOffset+len(r.filter)], r.filter) {
			updateMetrics(func(m *Metrics) {
				m.RxFilter++
			})
			header.Status = unix.TP_STATUS_KERNEL
			return true
		}
	}

	return false
}

// Start begins packet reception
func (r *RawReceiver) Start(ctx context.Context, req *unix.TpacketReq) {
	r.done = make(chan struct{})
	go func() {
		log.Println("RawReceiver started")
		for {
			select {
			case <-ctx.Done():
				log.Println("Context done, stopping RawReceiver")
				return
			default:
				// Use poll to wait for incoming packets
				pollFds := []unix.PollFd{
					{
						Fd:     int32(r.fd),
						Events: unix.POLLIN,
					},
				}
				n, err := unix.Poll(pollFds, -1)
				if err != nil {
					updateMetrics(func(m *Metrics) {
						m.RxErrors++
					})
					r.packets <- Packet{Err: err}
					continue
				}
				if n == 0 {
					continue
				}
				//logger.Println("Poll returned")
				// Read packet from mmap buffer
				for i := 0; i < len(r.packetBuffer); i += int(req.Frame_size) {
					frame := r.packetBuffer[i : i+int(req.Frame_size)]
					header := (*unix.TpacketHdr)(unsafe.Pointer(&frame[0]))
					if header.Status&unix.TP_STATUS_USER == 0 {
						continue
					}

					updateMetrics(func(m *Metrics) {
						m.RxFrames++
					})

					// Get the actual packet data starting from MAC header
					macOffset := uint32(header.Mac)
					packetLen := uint32(header.Len)

					// if r.filterPacket(frame[macOffset:macOffset+packetLen], packetLen, header) {
					// 	continue
					// }

					packetDataBuffer := getGrowingBuffer(int(packetLen))
					if packetDataBuffer == nil {
						header.Status = unix.TP_STATUS_KERNEL // Return frame to kernel
						continue
					}
					packetData := packetDataBuffer[:packetLen]
					copy(packetData, frame[macOffset:macOffset+packetLen])

					//fmt.Printf("Packet Len: %d\n", packetLen)
					//fmt.Printf("Packet Data Hex: \n%s\n", hex.Dump(packetData))

					if r.filterPacket(packetData, packetLen, header) {
						continue
					}

					// Only copy data if packet passes filter
					r.forwardPacket(packetData, header)
				}
			}
		}
	}()
}

// Start begins packet transmission
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
				// Return the buffer to the pool after sending
				bufferPool.Put(packet.Data)
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

// startMetricsReporter prints metrics at intervals
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
					"Broadcast : %d\n"+
					"Buffer    : %d\n"+
					"BufFull   : %d\n",
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
					metrics.Broadcast,
					metrics.buffer,
					metrics.BufFull)
				metricsMutex.Unlock()
			}
		}
	}()
}

const (
	METRICS_BUFFER_SIZE = 1000
)

var metricsUpdateChan = make(chan func(*Metrics), METRICS_BUFFER_SIZE)

func startMetricsProcessor() {
	go func() {
		for update := range metricsUpdateChan {
			metricsMutex.Lock()
			update(&metrics)
			metricsMutex.Unlock()
		}
	}()
}

func queueMetricsUpdate(f func(*Metrics)) {
	select {
	case metricsUpdateChan <- f:
	default:
	}
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
	filter_str := flag.String("filter", "", "filter string in hex format")
	filter_offset := flag.Int("filter-offset", 0, "filter offset from packet beginning")
	metrics_enabled := flag.Bool("metrics", false, "Enable metrics collection and reporting")
	bmcast := flag.Bool("bmcast", false, "allow broadcast and multicast packets")
	flag.Parse()

	hexStr := *filter_str
	filter, err := hex.DecodeString(hexStr)
	if err != nil {
		log.Fatalf("Failed to decode hex string: %v", err)
	}

	dst_addr := fmt.Sprintf("%s:%d", *dst_ip, *dst_port)
	src_addr := fmt.Sprintf("%s:%d", *src_ip, *src_port)

	packets := make(chan Packet, CHANNEL_SIZE)

	startMetricsProcessor()

	updateMetrics(func(m *Metrics) {
		m.buffer = BUFFER_SIZE
	})

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

	receiver.Start(ctx, &unix.TpacketReq{
		Block_size: 1 << 22, // 4 MiB
		Block_nr:   64,
		Frame_size: 1 << 11, // 16 KiB
		Frame_nr:   RING_SIZE,
	})
	sender.Start(ctx)

	if *metrics_enabled {
		startMetricsReporter(ctx, time.Second)
	}

	<-sigs
	fmt.Println("Received SIGINT. Exiting...")

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	fmt.Printf("Alloc = %v MiB", bToMb(memStats.Alloc))
	fmt.Printf("\tTotalAlloc = %v MiB", bToMb(memStats.TotalAlloc))
	fmt.Printf("\tSys = %v MiB", bToMb(memStats.Sys))
	fmt.Printf("\tNumGC = %v\n", memStats.NumGC)
}
