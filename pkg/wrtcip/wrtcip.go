package wrtcip

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/pojntfx/webrtcfd/pkg/wrtcconn"
	"github.com/songgao/water"
)

const (
	dataChannelName      = "webrtcfd"
	ethernetHeaderLength = 14
)

type AdapterConfig struct {
	*wrtcconn.AdapterConfig
	Device             string
	OnSignalerConnect  func(string)
	OnPeerConnect      func(string)
	OnPeerDisconnected func(string)
	IPs                []string
}

type Adapter struct {
	signaler string
	key      string
	ice      []string
	config   *AdapterConfig
	ctx      context.Context

	cancel  context.CancelFunc
	adapter *wrtcconn.Adapter
	tun     *water.Interface
	mtu     int
	ids     chan string
}

type peerWithIP struct {
	*wrtcconn.Peer
	ip  net.IP
	net *net.IPNet
}

func NewAdapter(
	signaler string,
	key string,
	ice []string,
	config *AdapterConfig,
	ctx context.Context,
) *Adapter {
	ictx, cancel := context.WithCancel(ctx)

	return &Adapter{
		signaler: signaler,
		key:      key,
		ice:      ice,
		config:   config,
		ctx:      ictx,

		cancel: cancel,
		ids:    make(chan string),
	}
}

func (a *Adapter) Open() error {
	var err error
	a.tun, err = water.New(water.Config{
		DeviceType:             water.TAP,
		PlatformSpecificParams: getPlatformSpecificParams(a.config.Device),
	})
	if err != nil {
		return err
	}

	for _, ip := range a.config.IPs {
		if err = setIPAddress(a.tun.Name(), ip); err != nil {
			return err
		}
	}

	data, err := json.Marshal(a.config.IPs)
	if err != nil {
		return err
	}
	a.config.AdapterConfig.ID = string(data)

	a.adapter = wrtcconn.NewAdapter(
		a.signaler,
		a.key,
		strings.Split(strings.Join(a.ice, ","), ","),
		a.config.AdapterConfig,
		a.ctx,
	)

	a.ids, err = a.adapter.Open()
	if err != nil {
		return err
	}

	a.mtu, err = getMTU(a.tun.Name())

	return err
}

func (a *Adapter) Close() error {
	if err := a.tun.Close(); err != nil {
		return err
	}

	return a.adapter.Close()
}

func (a *Adapter) Wait() error {
	peers := map[string]*peerWithIP{}
	var peersLock sync.Mutex

	go func() {
		for {
			buf := make([]byte, a.mtu+ethernetHeaderLength)

			if _, err := a.tun.Read(buf); err != nil {
				if a.config.Verbose {
					log.Println("Could not read from TAP device, skipping")
				}

				continue
			}

			var frame layers.Ethernet
			if err := frame.DecodeFromBytes(buf, gopacket.NilDecodeFeedback); err != nil {
				if a.config.Verbose {
					log.Println("Could not unmarshal frame, skipping")
				}

				continue
			}

			var dst net.IP
			if frame.NextLayerType().Contains(layers.LayerTypeIPv6) {
				var packet layers.IPv6
				if err := packet.DecodeFromBytes(frame.LayerPayload(), gopacket.NilDecodeFeedback); err != nil {
					if a.config.Verbose {
						log.Println("Could not unmarshal packet, skipping")
					}

					continue
				}

				dst = packet.DstIP
			} else if frame.NextLayerType().Contains(layers.LayerTypeIPv4) {
				var packet layers.IPv4
				if err := packet.DecodeFromBytes(frame.LayerPayload(), gopacket.NilDecodeFeedback); err != nil {
					if a.config.Verbose {
						log.Println("Could not unmarshal packet, skipping")
					}

					continue
				}

				dst = packet.DstIP
			} else if frame.NextLayerType().Contains(layers.LayerTypeARP) {
				var packet layers.ARP
				if err := packet.DecodeFromBytes(frame.LayerPayload(), gopacket.NilDecodeFeedback); err != nil {
					if a.config.Verbose {
						log.Println("Could not unmarshal packet, skipping")
					}

					continue
				}

				if len(packet.DstProtAddress) < 4 {
					if a.config.Verbose {
						log.Println("Could not unmarshal protocol address, skipping")
					}

					continue
				}

				dst = net.IP{packet.DstProtAddress[0], packet.DstProtAddress[1], packet.DstProtAddress[2], packet.DstProtAddress[3]}
			} else {
				if a.config.Verbose {
					log.Println("Got unknown layer type, skipping:", frame.NextLayerType())
				}

				continue
			}

			peersLock.Lock()
			for _, peer := range peers {
				// Send if matching destination, multicast or broadcast IP
				if dst.Equal(peer.ip) || ((dst.IsMulticast() || dst.IsInterfaceLocalMulticast() || dst.IsInterfaceLocalMulticast()) && len(dst) == len(peer.ip)) || (peer.ip.To4() != nil && dst.Equal(getBroadcastAddr(peer.net))) {
					if _, err := peer.Conn.Write(buf); err != nil {
						if a.config.Verbose {
							log.Println("Could not write to peer, skipping")
						}

						continue
					}
				}
			}
			peersLock.Unlock()
		}
	}()

	for {
		select {
		case <-a.ctx.Done():
			return nil
		case id := <-a.ids:
			if a.config.OnSignalerConnect != nil {
				a.config.OnSignalerConnect(id)
			}

			if err := setLinkUp(a.tun.Name()); err != nil {
				return err
			}
		case peer := <-a.adapter.Accept():
			if a.config.OnPeerConnect != nil {
				a.config.OnPeerConnect(peer.ID)
			}

			go func() {
				defer func() {
					if a.config.OnPeerDisconnected != nil {
						a.config.OnPeerDisconnected(peer.ID)
					}

					peersLock.Lock()
					delete(peers, peer.ID)
					peersLock.Unlock()
				}()

				ips := []string{}
				if err := json.Unmarshal([]byte(peer.ID), &ips); err != nil {
					return
				}

				peersLock.Lock()
				for _, rawIP := range ips {
					ip, net, err := net.ParseCIDR(rawIP)
					if err != nil {
						if a.config.Verbose {
							log.Println("Could not parse IP address, skipping")
						}

						continue
					}

					peers[ip.String()] = &peerWithIP{peer, ip, net}
				}
				peersLock.Unlock()

				for {
					buf := make([]byte, a.mtu+ethernetHeaderLength)

					if _, err := peer.Conn.Read(buf); err != nil {
						if a.config.Verbose {
							log.Println("Could not read from peer, stopping")
						}

						return
					}

					if _, err := a.tun.Write(buf); err != nil {
						if a.config.Verbose {
							log.Println("Could not write to TUN device, skipping")
						}

						continue
					}
				}
			}()
		}
	}
}

// See https://go.dev/play/p/Igo6Ct3gx_
func getBroadcastAddr(n *net.IPNet) net.IP {
	ip := make(net.IP, len(n.IP.To4()))

	binary.BigEndian.PutUint32(ip, binary.BigEndian.Uint32(n.IP.To4())|^binary.BigEndian.Uint32(net.IP(n.Mask).To4()))

	return ip
}
