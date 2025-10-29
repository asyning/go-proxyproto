package proxyproto

import (
	"bufio"
	"errors"
	"fmt"
	"github.com/gobwas/pool/pbufio"
	"github.com/gofrs/uuid/v5"
	"github.com/puzpuzpuz/xsync/v4"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// DefaultReadHeaderTimeout is how long header processing waits for header to
	// be read from the wire, if Listener.ReaderHeaderTimeout is not set.
	// It's kept as a global variable so to make it easier to find and override,
	// e.g. go build -ldflags -X "github.com/pires/go-proxyproto.DefaultReadHeaderTimeout=1s"
	DefaultReadHeaderTimeout = 10 * time.Second

	// ErrInvalidUpstream should be returned when an upstream connection address
	// is not trusted, and therefore is invalid.
	ErrInvalidUpstream = fmt.Errorf("proxyproto: upstream connection address not trusted for PROXY information")
)

// Listener is used to wrap an underlying listener,
// whose connections may be using the HAProxy Proxy Protocol.
// If the connection is using the protocol, the RemoteAddr() will return
// the correct client address. ReadHeaderTimeout will be applied to all
// connections in order to prevent blocking operations. If no ReadHeaderTimeout
// is set, a default of 10s will be used. This can be disabled by setting the
// timeout to < 0.
//
// Only one of Policy or ConnPolicy should be provided. If both are provided then
// a panic would occur during accept.
type Listener struct {
	Listener net.Listener
	// Deprecated: use ConnPolicyFunc instead. This will be removed in future release.
	Policy            PolicyFunc
	ConnPolicy        ConnPolicyFunc
	ValidateHeader    Validator
	ReadHeaderTimeout time.Duration
}

// Conn is used to wrap and underlying connection which
// may be speaking the Proxy Protocol. If it is, the RemoteAddr() will
// return the address of the client instead of the proxy address. Each connection
// will have its own readHeaderTimeout and readDeadline set by the Accept() call.
type Conn struct {
	readDeadline      atomic.Value  `json:"-"` // time.Time
	once              sync.Once     `json:"-"`
	readErr           error         `json:"-"`
	conn              net.Conn      `json:"-"`
	bufReader         *bufio.Reader `json:"-"`
	header            *Header       `json:"-"`
	ProxyHeaderPolicy Policy        `json:"-"`
	Validate          Validator     `json:"-"`
	readHeaderTimeout time.Duration `json:"-"`

	UUID      uuid.UUID `json:"id"`
	manager   *Manager  `json:"-"`
	RemoteAdd string    `json:"remoteAdd"`
	Start     time.Time `json:"start"`
	Addr      string    `json:"Addr"`
}

// Validator receives a header and decides whether it is a valid one
// In case the header is not deemed valid it should return an error.
type Validator func(*Header) error

// ValidateHeader adds given validator for proxy headers to a connection when passed as option to NewConn()
func ValidateHeader(v Validator) func(*Conn) {
	return func(c *Conn) {
		if v != nil {
			c.Validate = v
		}
	}
}

// SetReadHeaderTimeout sets the readHeaderTimeout for a connection when passed as option to NewConn()
func SetReadHeaderTimeout(t time.Duration) func(*Conn) {
	return func(c *Conn) {
		if t >= 0 {
			c.readHeaderTimeout = t
		}
	}
}

// Accept waits for and returns the next valid connection to the listener.
func (p *Listener) Accept() (net.Conn, error) {
	for {
		// Get the underlying connection
		conn, err := p.Listener.Accept()
		if err != nil {
			return nil, err
		}

		proxyHeaderPolicy := USE
		if p.Policy != nil && p.ConnPolicy != nil {
			panic("only one of policy or connpolicy must be provided.")
		}
		if p.Policy != nil || p.ConnPolicy != nil {
			if p.Policy != nil {
				proxyHeaderPolicy, err = p.Policy(conn.RemoteAddr())
			} else {
				proxyHeaderPolicy, err = p.ConnPolicy(ConnPolicyOptions{
					Upstream:   conn.RemoteAddr(),
					Downstream: conn.LocalAddr(),
				})
			}
			if err != nil {
				// can't decide the policy, we can't accept the connection
				conn.Close()

				if errors.Is(err, ErrInvalidUpstream) {
					// keep listening for other connections
					continue
				}

				return nil, err
			}
			// Handle a connection as a regular one
			if proxyHeaderPolicy == SKIP {
				return conn, nil
			}
		}

		newConn := NewConn(
			conn,
			WithPolicy(proxyHeaderPolicy),
			ValidateHeader(p.ValidateHeader),
		)

		newConn.Start = time.Now()
		newConn.Addr = p.Listener.Addr().String()
		newConn.manager = ConnectionManager
		newConn.RemoteAdd = newConn.RemoteAddr().String()
		newConn.UUID = NewUUIDV4()
		ConnectionManager.Join(newConn)
		// If the ReadHeaderTimeout for the listener is unset, use the default timeout.
		if p.ReadHeaderTimeout == 0 {
			p.ReadHeaderTimeout = DefaultReadHeaderTimeout
		}

		// Set the readHeaderTimeout of the new conn to the value of the listener
		newConn.readHeaderTimeout = p.ReadHeaderTimeout

		return newConn, nil
	}
}

// Close closes the underlying listener.
func (p *Listener) Close() error {
	return p.Listener.Close()
}

// Addr returns the underlying listener's network address.
func (p *Listener) Addr() net.Addr {
	return p.Listener.Addr()
}

// NewConn is used to wrap a net.Conn that may be speaking
// the proxy protocol into a proxyproto.Conn
func NewConn(conn net.Conn, opts ...func(*Conn)) *Conn {
	// For v1 the header length is at most 108 bytes.
	// For v2 the header length is at most 52 bytes plus the length of the TLVs.
	// We use 256 bytes to be safe.
	//const bufSize = 256
	//br := bufio.NewReaderSize(conn, bufSize)
	br := pbufio.GetReader(conn, defaultBufSize)

	pConn := &Conn{
		bufReader: br,
		conn:      conn,
	}

	for _, opt := range opts {
		opt(pConn)
	}

	return pConn
}

// Read is check for the proxy protocol header when doing
// the initial scan. If there is an error parsing the header,
// it is returned and the socket is closed.
func (p *Conn) Read(b []byte) (int, error) {
	p.once.Do(func() {
		p.readErr = p.readHeader()
	})
	if p.readErr != nil {
		return 0, p.readErr
	}
	return p.bufReader.Read(b)
}

func (p *Conn) Peek(n int) ([]byte, error) {
	p.once.Do(func() {
		p.readErr = p.readHeader()
	})
	if p.readErr != nil {
		return nil, p.readErr
	}
	return p.bufReader.Peek(n)
}

// Write wraps original conn.Write
func (p *Conn) Write(b []byte) (int, error) {
	return p.conn.Write(b)
}

// Close wraps original conn.Close
func (p *Conn) Close() error {
	if p.bufReader != nil {
		pbufio.PutReader(p.bufReader)
		p.bufReader = nil
	}
	if p.manager != nil {
		p.manager.Leave(p)
	}
	return p.conn.Close()
}

// ProxyHeader returns the proxy protocol header, if any. If an error occurs
// while reading the proxy header, nil is returned.
func (p *Conn) ProxyHeader() *Header {
	p.once.Do(func() { p.readErr = p.readHeader() })
	return p.header
}

// LocalAddr returns the address of the server if the proxy
// protocol is being used, otherwise just returns the address of
// the socket server. In case an error happens on reading the
// proxy header the original LocalAddr is returned, not the one
// from the proxy header even if the proxy header itself is
// syntactically correct.
func (p *Conn) LocalAddr() net.Addr {
	p.once.Do(func() { p.readErr = p.readHeader() })
	if p.header == nil || p.header.Command.IsLocal() || p.readErr != nil {
		return p.conn.LocalAddr()
	}

	return p.header.DestinationAddr
}

// RemoteAddr returns the address of the client if the proxy
// protocol is being used, otherwise just returns the address of
// the socket peer. In case an error happens on reading the
// proxy header the original RemoteAddr is returned, not the one
// from the proxy header even if the proxy header itself is
// syntactically correct.
func (p *Conn) RemoteAddr() net.Addr {
	p.once.Do(func() { p.readErr = p.readHeader() })
	if p.header == nil || p.header.Command.IsLocal() || p.readErr != nil {
		return p.conn.RemoteAddr()
	}

	return p.header.SourceAddr
}

// Raw returns the underlying connection which can be casted to
// a concrete type, allowing access to specialized functions.
//
// Use this ONLY if you know exactly what you are doing.
func (p *Conn) Raw() net.Conn {
	return p.conn
}

// TCPConn returns the underlying TCP connection,
// allowing access to specialized functions.
//
// Use this ONLY if you know exactly what you are doing.
func (p *Conn) TCPConn() (conn *net.TCPConn, ok bool) {
	conn, ok = p.conn.(*net.TCPConn)
	return
}

// UnixConn returns the underlying Unix socket connection,
// allowing access to specialized functions.
//
// Use this ONLY if you know exactly what you are doing.
func (p *Conn) UnixConn() (conn *net.UnixConn, ok bool) {
	conn, ok = p.conn.(*net.UnixConn)
	return
}

// UDPConn returns the underlying UDP connection,
// allowing access to specialized functions.
//
// Use this ONLY if you know exactly what you are doing.
func (p *Conn) UDPConn() (conn *net.UDPConn, ok bool) {
	conn, ok = p.conn.(*net.UDPConn)
	return
}

// SetDeadline wraps original conn.SetDeadline
func (p *Conn) SetDeadline(t time.Time) error {
	p.readDeadline.Store(t)
	return p.conn.SetDeadline(t)
}

// SetReadDeadline wraps original conn.SetReadDeadline
func (p *Conn) SetReadDeadline(t time.Time) error {
	// Set a local var that tells us the desired deadline. This is
	// needed in order to reset the read deadline to the one that is
	// desired by the user, rather than an empty deadline.
	p.readDeadline.Store(t)
	return p.conn.SetReadDeadline(t)
}

// SetWriteDeadline wraps original conn.SetWriteDeadline
func (p *Conn) SetWriteDeadline(t time.Time) error {
	return p.conn.SetWriteDeadline(t)
}

func (p *Conn) readHeader() error {
	// If the connection's readHeaderTimeout is more than 0,
	// push our deadline back to now plus the timeout. This should only
	// run on the connection, as we don't want to override the previous
	// read deadline the user may have used.
	if p.readHeaderTimeout > 0 {
		if err := p.conn.SetReadDeadline(time.Now().Add(p.readHeaderTimeout)); err != nil {
			return err
		}
	}

	header, err := Read(p.bufReader)

	// If the connection's readHeaderTimeout is more than 0, undo the change to the
	// deadline that we made above. Because we retain the readDeadline as part of our
	// SetReadDeadline override, we know the user's desired deadline so we use that.
	// Therefore, we check whether the error is a net.Timeout and if it is, we decide
	// the proxy proto does not exist and set the error accordingly.
	if p.readHeaderTimeout > 0 {
		t := p.readDeadline.Load()
		if t == nil {
			t = time.Time{}
		}
		if err := p.conn.SetReadDeadline(t.(time.Time)); err != nil {
			return err
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			err = ErrNoProxyProtocol
		}
	}

	// For the purpose of this wrapper shamefully stolen from armon/go-proxyproto
	// let's act as if there was no error when PROXY protocol is not present.
	if err == ErrNoProxyProtocol {
		// but not if it is required that the connection has one
		if p.ProxyHeaderPolicy == REQUIRE {
			return err
		}

		return nil
	}

	// proxy protocol header was found
	if err == nil && header != nil {
		switch p.ProxyHeaderPolicy {
		case REJECT:
			// this connection is not allowed to send one
			return ErrSuperfluousProxyHeader
		case USE, REQUIRE:
			if p.Validate != nil {
				err = p.Validate(header)
				if err != nil {
					return err
				}
			}

			p.header = header
		}
	}

	return err
}

// ReadFrom implements the io.ReaderFrom ReadFrom method
func (p *Conn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := p.conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(p.conn, r)
}

// WriteTo implements io.WriterTo
func (p *Conn) WriteTo(w io.Writer) (int64, error) {
	p.once.Do(func() { p.readErr = p.readHeader() })
	if p.readErr != nil {
		return 0, p.readErr
	}
	return p.bufReader.WriteTo(w)
}
func (p *Conn) ID() string {
	return p.UUID.String()
}

const (
	defaultBufSize = 4096
)

type Manager struct {
	connections *xsync.Map[string, *Conn]
}

func (m *Manager) Join(c *Conn) {
	m.connections.Store(c.ID(), c)
}

func (m *Manager) Leave(c *Conn) {
	m.connections.Delete(c.ID())
}

func (m *Manager) Snapshot() []*Conn {
	var connections []*Conn = make([]*Conn, 0)
	m.connections.Range(func(key string, value *Conn) bool {
		connections = append(connections, value)
		return true
	})
	return connections
}

var ConnectionManager = &Manager{
	connections: xsync.NewMap[string, *Conn](),
}
