package pcap

import (
	"context"
	"io"
	"log"
	"net"
	"regexp"
	"time"

	"github.com/frozenpine/pkt4go"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	libpcap "github.com/google/gopacket/pcap"
	"github.com/pkg/errors"
)

const (
	defaultTCPBufferLen = 1024 * 1024
)

var (
	dataSourcePattern = regexp.MustCompile(`^(?P<proto>pcap|file)://(?P<source>.*)$`)
)

func CreateHandler(dataSrc string) (handle *libpcap.Handle, err error) {
	srcMatch := dataSourcePattern.FindStringSubmatch(dataSrc)
	if srcMatch == nil {
		return nil, errors.New("invalid data source: " + dataSrc)
	}

	var proto, source string

	for idx, name := range dataSourcePattern.SubexpNames() {
		switch name {
		case "proto":
			proto = srcMatch[idx]
		case "source":
			source = srcMatch[idx]
		}
	}

	// Find inteface name if source is an IP address
	if ip, err := net.ResolveIPAddr("ip", source); err == nil {
		ifaceList, err := libpcap.FindAllDevs()
		if err != nil {
			return nil, errors.WithStack(err)
		}

	FIND_IFACE:
		for _, iface := range ifaceList {
			for _, addr := range iface.Addresses {
				if addr.IP.Equal(ip.IP) {
					source = iface.Name
					break FIND_IFACE
				}
			}
		}
	}

	switch proto {
	case "pcap":
		if handle, err = libpcap.OpenLive(source, 65535, true, time.Hour); err != nil {
			return nil, errors.WithStack(err)
		}
	case "file":
		if handle, err = libpcap.OpenOffline(source); err != nil {
			return nil, errors.WithStack(err)
		}
	default:
		return nil, errors.New("unknown pcap protocol: " + proto)
	}

	return
}

func StartCapture(ctx context.Context, handler *libpcap.Handle, filter string, fn pkt4go.DataHandler) error {
	if err := handler.SetBPFFilter(filter); err != nil {
		return errors.WithStack(err)
	}

	if ctx == nil {
		ctx = context.Background()
	}

	var (
		sessionBuffers = make(map[uint64][]byte)
		err            error
	)

	packets := gopacket.NewPacketSource(handler, handler.LinkType()).Packets()

	for {
		select {
		case <-ctx.Done():
			return nil
		case pkg := <-packets:
			if pkg == nil {
				return nil
			}

			if fn == nil {
				continue
			}

			ip := pkg.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
			if ip == nil {
				return errors.New("captured packet is not a valid IPv4 packet")
			}

			var (
				src, dst    net.Addr
				usedSize    int
				flowHash    uint64
				buffer      []byte
				bufferExist bool
			)

			switch ip.NextLayerType() {
			case layers.LayerTypeTCP:
				tcp, _ := pkg.Layer(layers.LayerTypeTCP).(*layers.TCP)
				src = &net.TCPAddr{IP: ip.SrcIP, Port: int(tcp.SrcPort)}
				dst = &net.TCPAddr{IP: ip.DstIP, Port: int(tcp.DstPort)}

				flowHash = tcp.TransportFlow().FastHash()

				// 检查3次握手的ack, 确保buffer从头开始
				if tcp.SYN && tcp.ACK {
					sessionBuffers[flowHash] = make([]byte, 0, defaultTCPBufferLen)
					continue
				}

				// TCP会话结束, 清理session cache
				if tcp.FIN && tcp.ACK {
					delete(sessionBuffers, flowHash)
					continue
				}

				if len(tcp.Payload) <= 0 {
					continue
				}

				buffer, bufferExist = sessionBuffers[flowHash]
				if !bufferExist {
					continue
				}
				buffer = append(buffer, tcp.Payload...)
			case layers.LayerTypeUDP:
				udp, _ := pkg.Layer(layers.LayerTypeUDP).(*layers.UDP)
				src = &net.UDPAddr{IP: ip.SrcIP, Port: int(udp.SrcPort)}
				dst = &net.UDPAddr{IP: ip.DstIP, Port: int(udp.DstPort)}

				flowHash = udp.TransportFlow().FastHash()
				if buffer, bufferExist = sessionBuffers[flowHash]; bufferExist {
					buffer = append(buffer, udp.Payload...)
				}
			default:
				log.Println("unsupported Transport Layer: " + ip.NextLayerType().String())
			}

			usedSize, err = fn(src, dst, buffer)

			if err != nil {
				if err == io.EOF {
					return nil
				}

				log.Printf("[%s] %s -> %s data handler failed: %v", pkg.Metadata().Timestamp, src, dst, err)
			} else if len(buffer) != usedSize {
				sessionBuffers[flowHash] = buffer[usedSize:]
			}
		}
	}
}
