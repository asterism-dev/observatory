package guestinit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const maxAuthorizedKeys = 1 << 20

// subsystems maps an SSH subsystem name to its handler; sftp goes here.
var subsystems = map[string]func(ssh.Channel){}

// SSHServer is the guest's SSH server: only dev logs in, sessions are spawned into the image.
type SSHServer struct {
	Keys     func() []ssh.PublicKey
	HostKeys []ssh.Signer
	Spawn    func(*exec.Cmd) error
	Env      func() map[string]string
	Shell    func() string
	// Root is the image root as the init sees it, for etc/group and agent sockets under tmp.
	Root Root
}

// AuthorizedKeys returns the seed keys plus the keys in dev's own authorized_keys; key options are not enforced.
func (r Root) AuthorizedKeys(seed []string) []ssh.PublicKey {
	rest := append([]byte(strings.Join(seed, "\n")+"\n"), r.readAuthorizedKeys()...)
	var keys []ssh.PublicKey
	for len(rest) > 0 {
		key, _, _, next, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			break
		}
		keys = append(keys, key)
		rest = next
	}
	return keys
}

// readAuthorizedKeys refuses anything but a regular file, so a FIFO planted by the user cannot hang logins.
func (r Root) readAuthorizedKeys() []byte {
	f, err := os.OpenFile(r.join(devHome+"/.ssh/authorized_keys"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(f, maxAuthorizedKeys))
	return data
}

// Serve accepts connections until l fails.
func (s *SSHServer) Serve(l net.Listener) error {
	cfg := &ssh.ServerConfig{PublicKeyCallback: s.authorize}
	for _, k := range s.HostKeys {
		cfg.AddHostKey(k)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.serveConn(c, cfg)
	}
}

func (s *SSHServer) authorize(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if meta.User() != devName {
		return nil, fmt.Errorf("user %q may not log in", meta.User())
	}
	for _, k := range s.Keys() {
		if bytes.Equal(k.Marshal(), key.Marshal()) {
			return &ssh.Permissions{}, nil
		}
	}
	return nil, errors.New("unknown key")
}

func (s *SSHServer) serveConn(c net.Conn, cfg *ssh.ServerConfig) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	defer sc.Close()
	go serveGlobalRequests(sc, reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go s.serveSession(sc, nc)
		case "direct-tcpip":
			go directTCPIP(nc)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
}

type forwardRequest struct {
	Addr string
	Port uint32
}

// serveGlobalRequests handles remote forwards; like OpenSSH without GatewayPorts they bind loopback only.
func serveGlobalRequests(sc *ssh.ServerConn, reqs <-chan *ssh.Request) {
	listeners := map[uint32]net.Listener{}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for req := range reqs {
		var p forwardRequest
		switch req.Type {
		case "tcpip-forward":
			// Privileged ports stay with root, as in OpenSSH.
			if ssh.Unmarshal(req.Payload, &p) != nil || (p.Port != 0 && p.Port < 1024) || listeners[p.Port] != nil {
				_ = req.Reply(false, nil)
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(p.Port))))
			if err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			port := uint32(l.Addr().(*net.TCPAddr).Port)
			listeners[port] = l
			var reply []byte
			if p.Port == 0 {
				reply = ssh.Marshal(struct{ Port uint32 }{port})
			}
			_ = req.Reply(true, reply)
			go acceptForwards(sc, l, p.Addr, port)
		case "cancel-tcpip-forward":
			var l net.Listener
			if ssh.Unmarshal(req.Payload, &p) == nil {
				l = listeners[p.Port]
			}
			if l != nil {
				l.Close()
				delete(listeners, p.Port)
			}
			_ = req.Reply(l != nil, nil)
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func acceptForwards(sc *ssh.ServerConn, l net.Listener, addr string, port uint32) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			orig := c.RemoteAddr().(*net.TCPAddr)
			payload := ssh.Marshal(struct {
				Addr     string
				Port     uint32
				OrigAddr string
				OrigPort uint32
			}{addr, port, orig.IP.String(), uint32(orig.Port)})
			ch, reqs, err := sc.OpenChannel("forwarded-tcpip", payload)
			if err != nil {
				c.Close()
				return
			}
			go ssh.DiscardRequests(reqs)
			pipe(ch, c)
		}()
	}
}

func directTCPIP(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "bad direct-tcpip request")
		return
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))), 10*time.Second)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	pipe(ch, c)
}

// pipe copies both ways, passing each side's EOF on as a half close.
func pipe(ch ssh.Channel, c net.Conn) {
	defer ch.Close()
	defer c.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(ch, c)
		_ = ch.CloseWrite()
		close(done)
	}()
	_, _ = io.Copy(c, ch)
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
	}
	<-done
}
