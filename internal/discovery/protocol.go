package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"unicode/utf8"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

const (
	Group           = "239.255.42.99"
	Port            = 53318
	MaxDatagram     = 1200
	protocolVersion = 1
)

// No IP or reply address is accepted from JSON: both come from the UDP source.
// This protocol version is independent of the TCP wire version.
type packet struct {
	Protocol string `json:"protocol"`
	Version  int    `json:"v"`
	Type     string `json:"type"`
	Query    string `json:"query"`
	ID       string `json:"id,omitempty"`
	Room     string `json:"room,omitempty"`
	User     string `json:"user,omitempty"`
	Port     int    `json:"port,omitempty"`
}

func queryPacket(id string) packet {
	return packet{Protocol: "lanlobby", Version: protocolVersion, Type: "query", Query: id}
}

func announcement(id string, p wire.Peer) packet {
	q := queryPacket(id)
	q.Type, q.ID, q.Room, q.User, q.Port = "advertisement", p.ID, p.Room, p.User, p.Port
	return q
}

func (p packet) validate() error {
	if p.Protocol != "lanlobby" || p.Version != protocolVersion || !wire.ValidID(p.Query) {
		return errors.New("invalid discovery protocol, version or query ID")
	}
	switch p.Type {
	case "query":
		if p.ID != "" || p.Room != "" || p.User != "" || p.Port != 0 {
			return errors.New("query contains member fields")
		}
	case "advertisement":
		return (wire.Peer{ID: p.ID, Room: p.Room, User: p.User, Port: p.Port}).Validate()
	default:
		return errors.New("invalid discovery packet type")
	}
	return nil
}

func encode(p packet) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	if err == nil && len(b) > MaxDatagram {
		return nil, errors.New("discovery packet too large")
	}
	return b, err
}

func decode(b []byte) (packet, error) {
	var p packet
	if len(b) == 0 || len(b) > MaxDatagram || !utf8.Valid(b) {
		return p, errors.New("invalid discovery datagram length or UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return p, errors.New("expected one discovery object")
	}
	return p, p.validate()
}

func usableSource(src *net.UDPAddr) bool {
	return src != nil && src.Port > 0 && src.Port <= 65535 && src.IP.To4() != nil &&
		!src.IP.IsUnspecified() && !src.IP.IsMulticast() && !src.IP.Equal(net.IPv4bcast)
}

func (p packet) peer(query string, src *net.UDPAddr) (wire.Peer, bool) {
	if p.Type != "advertisement" || p.Query != query || !usableSource(src) || p.validate() != nil {
		return wire.Peer{}, false
	}
	return wire.Peer{ID: p.ID, Room: p.Room, User: p.User, Port: p.Port, Addresses: []string{src.IP.String()}}, true
}
