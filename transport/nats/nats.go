package nats

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/micro/go-micro/cmd"
	"github.com/micro/go-micro/transport"
	"github.com/nats-io/nats"
)

type ntport struct {
	addrs []string
	opts  transport.Options
}

type ntportClient struct {
	conn *nats.Conn
	addr string
	id   string
	sub  *nats.Subscription
}

type ntportSocket struct {
	conn *nats.Conn
	m    *nats.Msg
	r    chan *nats.Msg

	once  sync.Once
	close chan bool

	sync.Mutex
	bl []*nats.Msg
}

type ntportListener struct {
	conn *nats.Conn
	addr string
	exit chan bool

	sync.RWMutex
	so map[string]*ntportSocket
}

func init() {
	cmd.DefaultTransports["nats"] = NewTransport
}

func (n *ntportClient) Send(m *transport.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return n.conn.PublishRequest(n.addr, n.id, b)
}

func (n *ntportClient) Recv(m *transport.Message) error {
	rsp, err := n.sub.NextMsg(time.Second * 10)
	if err != nil {
		return err
	}

	var mr *transport.Message
	if err := json.Unmarshal(rsp.Data, &mr); err != nil {
		return err
	}

	*m = *mr
	return nil
}

func (n *ntportClient) Close() error {
	n.sub.Unsubscribe()
	n.conn.Close()
	return nil
}

func (n *ntportSocket) Recv(m *transport.Message) error {
	if m == nil {
		return errors.New("message passed in is nil")
	}

	r, ok := <-n.r
	if !ok {
		return io.EOF
	}
	n.Lock()
	if len(n.bl) > 0 {
		select {
		case n.r <- n.bl[0]:
			n.bl = n.bl[1:]
		default:
		}
	}
	n.Unlock()

	if err := json.Unmarshal(r.Data, &m); err != nil {
		return err
	}
	return nil
}

func (n *ntportSocket) Send(m *transport.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return n.conn.Publish(n.m.Reply, b)
}

func (n *ntportSocket) Close() error {
	n.once.Do(func() {
		close(n.close)
	})
	return nil
}

func (n *ntportListener) Addr() string {
	return n.addr
}

func (n *ntportListener) Close() error {
	n.exit <- true
	n.conn.Close()
	return nil
}

func (n *ntportListener) Accept(fn func(transport.Socket)) error {
	s, err := n.conn.SubscribeSync(n.addr)
	if err != nil {
		return err
	}

	var lerr error

	go func() {
		<-n.exit
		lerr = s.Unsubscribe()
	}()

	for {
		m, err := s.NextMsg(time.Minute)
		if err != nil && err == nats.ErrTimeout {
			continue
		} else if err != nil {
			return err
		}

		n.RLock()
		sock, ok := n.so[m.Reply]
		n.RUnlock()

		if !ok {
			var once sync.Once
			sock = &ntportSocket{
				conn:  n.conn,
				once:  once,
				m:     m,
				r:     make(chan *nats.Msg, 1),
				close: make(chan bool),
			}
			n.Lock()
			n.so[m.Reply] = sock
			n.Unlock()

			go func() {
				// TODO: think of a better error response strategy
				defer func() {
					if r := recover(); r != nil {
						sock.Close()
					}
				}()
				fn(sock)
			}()

			go func() {
				<-sock.close
				n.Lock()
				delete(n.so, sock.m.Reply)
				n.Unlock()
			}()
		}

		select {
		case <-sock.close:
			continue
		default:
		}

		sock.Lock()
		sock.bl = append(sock.bl, m)
		select {
		case sock.r <- sock.bl[0]:
			sock.bl = sock.bl[1:]
		default:
		}
		sock.Unlock()

	}
	return lerr
}

func (n *ntport) Dial(addr string, dialOpts ...transport.DialOption) (transport.Client, error) {
	dopts := transport.DialOptions{
		Timeout: transport.DefaultDialTimeout,
	}

	for _, o := range dialOpts {
		o(&dopts)
	}

	opts := nats.DefaultOptions
	opts.Servers = n.addrs
	opts.Secure = n.opts.Secure
	opts.TLSConfig = n.opts.TLSConfig
	opts.Timeout = dopts.Timeout

	// secure might not be set
	if n.opts.TLSConfig != nil {
		opts.Secure = true
	}

	c, err := opts.Connect()
	if err != nil {
		return nil, err
	}

	id := nats.NewInbox()
	sub, err := c.SubscribeSync(id)
	if err != nil {
		return nil, err
	}

	return &ntportClient{
		conn: c,
		addr: addr,
		id:   id,
		sub:  sub,
	}, nil
}

func (n *ntport) Listen(addr string, listenOpts ...transport.ListenOption) (transport.Listener, error) {
	opts := nats.DefaultOptions
	opts.Servers = n.addrs
	opts.Secure = n.opts.Secure
	opts.TLSConfig = n.opts.TLSConfig

	// secure might not be set
	if n.opts.TLSConfig != nil {
		opts.Secure = true
	}

	c, err := opts.Connect()
	if err != nil {
		return nil, err
	}

	return &ntportListener{
		addr: nats.NewInbox(),
		conn: c,
		exit: make(chan bool, 1),
		so:   make(map[string]*ntportSocket),
	}, nil
}

func (n *ntport) String() string {
	return "nats"
}

func NewTransport(addrs []string, opts ...transport.Option) transport.Transport {
	var options transport.Options
	for _, o := range opts {
		o(&options)
	}

	var cAddrs []string

	for _, addr := range addrs {
		if len(addr) == 0 {
			continue
		}
		if !strings.HasPrefix(addr, "nats://") {
			addr = "nats://" + addr
		}
		cAddrs = append(cAddrs, addr)
	}

	if len(cAddrs) == 0 {
		cAddrs = []string{nats.DefaultURL}
	}

	return &ntport{
		addrs: cAddrs,
		opts:  options,
	}
}
