package trojan

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/imgk/shadow/netstack"
	"github.com/imgk/shadow/utils"
)

const (
	HexLen           = 56
	MaxUDPPacketSize = 4096 // Max 65536
)

const (
	Connect   = 1
	Assocaite = 3
)

type Handler struct {
	*tls.Config
	timeout time.Duration
	server  string
	hex     [HexLen + 2]byte
}

func NewHandler(url string, timeout time.Duration) (*Handler, error) {
	server, password, err := ParseUrl(url)
	if err != nil {
		return nil, err
	}

	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return nil, err
	}

	hd := &Handler{
		Config: &tls.Config{
			ServerName:         host,
			ClientSessionCache: tls.NewLRUClientSessionCache(32),
			InsecureSkipVerify: false,
		},
		timeout: timeout,
		server:  addr.String(),
		hex:     [HexLen + 2]byte{},
	}

	hash := sha256.Sum224([]byte(password))
	hex.Encode(hd.hex[:hex.EncodedLen(len(hash))], hash[:])
	hd.hex[HexLen], hd.hex[HexLen+1] = 0x0d, 0x0a

	return hd, nil
}

func (h *Handler) Handle(conn net.Conn, tgt net.Addr) error {
	defer conn.Close()

	target := make([]byte, HexLen+2+1+utils.MaxAddrLen+2)

	n := copy(target, h.hex[:])
	target[HexLen+2] = Connect

	addr, ok := tgt.(utils.Addr)
	if !ok {
		addr, err := utils.ResolveAddrBuffer(tgt, target[HexLen+2+1:])
		if err != nil {
			return fmt.Errorf("resolve addr error: %v", err)
		}
		n = HexLen + 2 + 1 + len(addr)
	} else {
		copy(target[HexLen+2+1:], addr)
		n = HexLen + 2 + 1 + len(addr)
	}
	target[n], target[n+1] = 0x0d, 0x0a

	rc, err := net.Dial("tcp", h.server)
	if err != nil {
		return err
	}
	rc.(*net.TCPConn).SetKeepAlive(true)
	rc = tls.Client(rc, h.Config)
	defer rc.Close()

	if _, err := rc.Write(target[:n+2]); err != nil {
		return fmt.Errorf("write to server %v error: %v", h.server, err)
	}

	l, ok := conn.(DuplexConn)
	if !ok {
		l = NewDuplexConn(conn)
	}

	r, ok := rc.(DuplexConn)
	if !ok {
		r = NewDuplexConn(rc)
	}

	if err := relay(l, r); err != nil {
		if ne, ok := err.(net.Error); ok {
			if ne.Timeout() {
				return nil
			}
		}
		if err == io.ErrClosedPipe || err == io.EOF {
			return nil
		}

		return fmt.Errorf("relay error: %v", err)
	}

	return nil
}

type DuplexConn interface {
	net.Conn
	CloseRead() error
	CloseWrite() error
}

type duplexConn struct {
	net.Conn
}

func NewDuplexConn(conn net.Conn) *duplexConn {
	return &duplexConn{Conn: conn}
}

func (conn *duplexConn) CloseRead() error {
	return conn.SetReadDeadline(time.Now())
}

func (conn *duplexConn) CloseWrite() error {
	return conn.SetWriteDeadline(time.Now())
}

func Copy(w io.Writer, r io.Reader) (n int64, err error) {
	if wt, ok := r.(io.WriterTo); ok {
		return wt.WriteTo(w)
	}
	if rt, ok := w.(io.ReaderFrom); ok {
		return rt.ReadFrom(r)
	}

	b := make([]byte, 4096)
	for {
		nr, er := r.Read(b)
		if nr > 0 {
			nw, ew := w.Write(b[:nr])
			if nw > 0 {
				n += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}

	return n, err
}

func relay(c, rc DuplexConn) error {
	errCh := make(chan error, 1)
	go copyWaitError(c, rc, errCh)

	_, err := Copy(rc, c)
	if err != nil {
		rc.Close()
		c.Close()
	} else {
		rc.CloseWrite()
		c.CloseRead()
	}

	if err != nil {
		<-errCh
		return err
	}

	return <-errCh
}

func copyWaitError(c, rc DuplexConn, errCh chan error) {
	_, err := Copy(c, rc)
	if err != nil {
		c.Close()
		rc.Close()
	} else {
		c.CloseWrite()
		rc.CloseRead()
	}

	errCh <- err
}

func (h *Handler) HandlePacket(conn netstack.PacketConn) error {
	defer conn.Close()

	target := make([]byte, HexLen+2+1+utils.MaxAddrLen+2)

	n := copy(target, h.hex[:])
	target[HexLen+2] = Assocaite

	src := conn.LocalAddr()
	addr, err := utils.ResolveAddrBuffer(src, target[HexLen+2+1:])
	if err != nil {
		return fmt.Errorf("resolve addr error: %v", err)
	}
	n = HexLen + 2 + 1 + len(addr)
	target[n], target[n+1] = 0x0d, 0x0a

	rc, err := net.Dial("tcp", h.server)
	if err != nil {
		return err
	}
	rc.(*net.TCPConn).SetKeepAlive(true)
	rc = tls.Client(rc, h.Config)
	defer rc.Close()

	if _, err := rc.Write(target[:n+2]); err != nil {
		return fmt.Errorf("write to server %v error: %v", h.server, err)
	}

	errCh := make(chan error, 1)
	go copyWithChannel(conn, rc, h.timeout, errCh)

	b := make([]byte, MaxUDPPacketSize)
	for {
		rc.SetDeadline(time.Now().Add(h.timeout))
		raddr, er := utils.ReadAddrBuffer(rc, b)
		if er != nil {
			if ne, ok := er.(net.Error); ok {
				if ne.Timeout() {
					break
				}
			}
			err = fmt.Errorf("read packet error: %v", er)
			break
		}

		n := len(raddr)

		if _, er := io.ReadFull(rc, b[n:n+4]); er != nil {
			if ne, ok := er.(net.Error); ok {
				if ne.Timeout() {
					break
				}
			}
			err = fmt.Errorf("read packet error: %v", er)
			break
		}

		n += (int(b[n])<<8 | int(b[n+1]))

		if _, er := io.ReadFull(rc, b[len(raddr):n]); er != nil {
			if ne, ok := er.(net.Error); ok {
				if ne.Timeout() {
					break
				}
			}
			err = fmt.Errorf("read packet error: %v", er)
			break
		}

		_, er = conn.WriteFrom(b[len(raddr):n], raddr)
		if er != nil {
			err = fmt.Errorf("write packet error: %v", er)
			break
		}
	}

	conn.Close()
	rc.Close()

	if err != nil {
		<-errCh
		return err
	}

	return <-errCh
}

func copyWithChannel(conn netstack.PacketConn, rc net.Conn, timeout time.Duration, errCh chan error) {
	b := make([]byte, MaxUDPPacketSize)
	b[utils.MaxAddrLen+2], b[utils.MaxAddrLen+3] = 0x0d, 0x0a

	for {
		n, tgt, err := conn.ReadTo(b[utils.MaxAddrLen+4:])
		if err != nil {
			if err == io.EOF {
				errCh <- nil
				break
			}
			errCh <- err
			break
		}
		b[utils.MaxAddrLen], b[utils.MaxAddrLen+1] = byte(n>>8), byte(n)

		addr, ok := tgt.(utils.Addr)
		if !ok {
			addr, err = utils.ResolveAddrBuffer(tgt, make([]byte, utils.MaxAddrLen))
			if err != nil {
				errCh <- fmt.Errorf("resolve addr error: %v", err)
				break
			}
		}

		copy(b[utils.MaxAddrLen-len(addr):], addr)

		rc.SetDeadline(time.Now().Add(timeout))
		_, err = rc.Write(b[utils.MaxAddrLen-len(addr) : utils.MaxAddrLen+4+n])
		if err != nil {
			errCh <- err
			break
		}
	}

	return
}