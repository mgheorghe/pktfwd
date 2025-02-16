//go:build linux && !cgo

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ETH_P_ALL    = 0x0003
	BUFFER_SIZE  = 8192
	CHANNEL_SIZE = 10000
)

type Metrics struct {
	RxFrames    uint64
	TxFrames    uint64
	RxErrors    uint64 // Receive errors
	TxErrors    uint64 // Transmit errors
	RxRate      uint64
	TxRate      uint64
	lastRx      uint64
	lastTx      uint64
	BufFullDrop uint64
	timestamp   time.Time
}

var (
	metrics      Metrics
	metricsMutex sync.Mutex
	bufferPool   = sync.Pool{
		New: func() interface{} {
			return make([]byte, BUFFER_SIZE)
		},
	}
)

func updateMetrics(f func(*Metrics)) {
	metricsMutex.Lock()
	defer metricsMutex.Unlock()
	f(&metrics)
}

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

	fd, err := conn.File()
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	err = syscall.SetsockoptInt(int(fd.Fd()), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1024*1024)
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
		connFile, err := r.conn.File()
		if err != nil {
			log.Printf("error getting interface: %v", err)
			return
		}
		defer connFile.Close()

		pollFds := []unix.PollFd{
			{
				Fd:     int32(connFile.Fd()),
				Events: unix.POLLIN,
			},
		}

		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Wait for packets with poll
				n, err := unix.Poll(pollFds, -1)
				if err != nil {
					updateMetrics(func(m *Metrics) {
						m.RxErrors++
					})
					continue
				}
				if n == 0 {
					continue
				}

				buf := bufferPool.Get().([]byte)
				n, _, err = r.conn.ReadFromUDP(buf)
				if err != nil {
					updateMetrics(func(m *Metrics) {
						m.RxErrors++
					})
					r.packets <- Packet{Err: err}
					bufferPool.Put(buf)
					continue
				}

				updateMetrics(func(m *Metrics) {
					m.RxFrames++
				})

				select {
				case r.packets <- Packet{Data: buf[:n]}:
				default:
					updateMetrics(func(m *Metrics) {
						m.BufFullDrop++
					})
					bufferPool.Put(buf)
				}
			}
		}
	}()
}

type RawSender struct {
	fd      int
	ifIndex int
	packets <-chan Packet
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
					updateMetrics(func(m *Metrics) {
						m.TxErrors++
					})
					continue
				}

				_, err := syscall.Write(s.fd, packet.Data)
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
				now := time.Now()
				duration := now.Sub(metrics.timestamp).Seconds()
				if duration > 0 {
					metricsMutex.Lock()
					metrics.RxRate = uint64(float64(metrics.RxFrames-metrics.lastRx) / duration)
					metrics.TxRate = uint64(float64(metrics.TxFrames-metrics.lastTx) / duration)
					metrics.lastRx = metrics.RxFrames
					metrics.lastTx = metrics.TxFrames
					metrics.timestamp = now
					metricsMutex.Unlock()
				}

				metricsMutex.Lock()
				fmt.Print("\033[H\033[2J")
				fmt.Printf("%s\n"+
					"Rx Frames: %d\n"+
					"Tx Frames: %d\n"+
					"Rx Errors: %d\n"+
					"Tx Errors: %d\n"+
					"Rx Rate: %d\n"+
					"Tx Rate: %d\n"+
					"Drop BF: %d\n",
					os.Args,
					metrics.RxFrames,
					metrics.TxFrames,
					metrics.RxErrors,
					metrics.TxErrors,
					metrics.RxRate,
					metrics.TxRate,
					metrics.BufFullDrop)
				metricsMutex.Unlock()
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

	dst_iface := flag.String("dst-iface", "eth0", "Source interface to copy packets from")
	src_ip := flag.String("src-ip", "10.0.1.7", "Source interface to copy packets from")
	src_port := flag.Int("src-port", 31982, "Source interface to copy packets from")
	metrics_enabled := flag.Bool("metrics", false, "Enable metrics collection and reporting")
	flag.Parse()

	packets := make(chan Packet, CHANNEL_SIZE)

	source := fmt.Sprintf("%s:%d", *src_ip, *src_port)
	receiver, err := NewUDPReceiver(source, packets)
	if err != nil {
		log.Fatalf("Error creating receiver: %v", err)
	}
	defer receiver.conn.Close()

	fmt.Println("Listening on %s", source)

	sender, err := NewRawSender(*dst_iface, packets)
	if err != nil {
		log.Fatalf("Error creating sender: %v", err)
	}
	defer syscall.Close(sender.fd)

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
