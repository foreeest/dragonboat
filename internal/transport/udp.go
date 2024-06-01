package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/cockroachdb/errors"
	"github.com/foreeest/dragonboat/config"
	"github.com/foreeest/dragonboat/internal/settings"
	//"github.com/lni/goutils/stringutil"
	"os"
	//"strings"
	//"github.com/foreeest/dragonboat/logger"
	"hash/crc32"

	"github.com/foreeest/dragonboat/raftio"
	pb "github.com/foreeest/dragonboat/raftpb"
	"github.com/lni/goutils/syncutil"

	//"io"
	"net"
	"sync"
	"time"
)

type UDP struct {
	stopper        *syncutil.Stopper
	connStopper    *syncutil.Stopper
	requestHandler raftio.MessageHandler
	chunkHandler   raftio.ChunkHandler
	encrypted      bool
	nhConfig       config.NodeHostConfig
}

var _ raftio.ITransport = (*UDP)(nil)
var (
	// ErrBadMessage is the error returned to indicate the incoming message is
	// corrupted.
	ErrBadMessage     = errors.New("invalid message")
	errPoisonReceived = errors.New("poison received")
	magicNumber       = [2]byte{0xAE, 0x7D}
	poisonNumber      = [2]byte{0x0, 0x0}
	payloadBufferSize = settings.SnapshotChunkSize + 1024*128
	//tlsHandshackTimeout = 10 * time.Second
	magicNumberDuration = 1 * time.Second
	headerDuration      = 2 * time.Second
	// readDuration        = 5 * time.Second
	// writeDuration       = 5 * time.Second
	keepAlivePeriod = 10 * time.Second
	perConnBufSize  = settings.Soft.PerConnectionSendBufSize
	recvBufSize     = settings.Soft.PerConnectionRecvBufSize
)

const (
	// UDPTransportName is the name of the tcp transport module.
	UDPTransportName         = "go-udp-transport"
	requestHeaderSize        = 18
	raftType          uint16 = 100
	snapshotType      uint16 = 200
)

type requestHeader struct {
	size   uint64 //大小
	crc    uint32 //校验位
	method uint16 //类型:raft 还是snapshot
}

func (h *requestHeader) encode(buf []byte) []byte {
	if len(buf) < requestHeaderSize {
		panic("input buf too small")
	}
	binary.BigEndian.PutUint16(buf, h.method)
	binary.BigEndian.PutUint64(buf[2:], h.size)
	binary.BigEndian.PutUint32(buf[10:], 0)
	binary.BigEndian.PutUint32(buf[14:], h.crc)
	v := crc32.ChecksumIEEE(buf[:requestHeaderSize])
	binary.BigEndian.PutUint32(buf[10:], v)
	return buf[:requestHeaderSize]
}

func (h *requestHeader) decode(buf []byte) bool {
	if len(buf) < requestHeaderSize {
		return false
	}
	incoming := binary.BigEndian.Uint32(buf[10:])
	binary.BigEndian.PutUint32(buf[10:], 0)
	expected := crc32.ChecksumIEEE(buf[:requestHeaderSize])
	if incoming != expected {
		plog.Errorf("header crc check failed")
		return false
	}
	binary.BigEndian.PutUint32(buf[10:], incoming)
	method := binary.BigEndian.Uint16(buf)
	if method != raftType && method != snapshotType {
		plog.Errorf("invalid method type")
		return false
	}
	h.method = method
	h.size = binary.BigEndian.Uint64(buf[2:])
	h.crc = binary.BigEndian.Uint32(buf[14:])
	return true
}

func readMessage_to_buff(conn net.UDPConn,
	buffer []byte) (int, []byte, error) {

	n, remoteAddr, err := conn.ReadFromUDP(buffer) //con已经是dial过的
	if err != nil {
		fmt.Println("Error reading from UDP:", err)
		//os.Exit(1)
		//continue
		return n, buffer, err
	}
	fmt.Printf("Received %d bytes from %s: %s\n", n, remoteAddr, buffer[:n])
	fmt.Printf("when read_to_buff: crc32.ChecksumIEEE(buffer[len(magicNumber)+requestHeaderSize: n]) : %d\n", crc32.ChecksumIEEE(buffer[len(magicNumber)+requestHeaderSize:n]))
	return n, buffer, err
}
func readMessage(conn net.UDPConn,
	header []byte, rbuf []byte, magicNum []byte, encrypted bool, addr *net.UDPAddr) (requestHeader, []byte, error) {
	var buffer []byte
	buffer = make([]byte, 1500) //数据报最大大小是1500
	//gpt4o:当缓冲区 p 的大小大于从网络连接中读取的数据时，不会发生错误或异常，只是缓冲区 p 的一部分会被使用来存储读取到的数据，其余部分保持不变
	n, buffer, err := readMessage_to_buff(conn, buffer)
	if err != nil {
		return requestHeader{}, nil, err
	}
	if n < len(magicNumber) {
		plog.Errorf("failed to get the header,conn.ReadFromUDP return n <  len(magicNumber)")
	}
	magicNum = buffer[0:len(magicNumber)]
	if bytes.Equal(magicNum, poisonNumber[:]) {
		if err := sendPoisonAck(conn, poisonNumber[:], addr); err != nil {
			plog.Debugf("failed to send poison ack %v", err)
		}
		return requestHeader{}, nil, errPoisonReceived
	}
	if !bytes.Equal(magicNum, magicNumber[:]) {
		return requestHeader{}, nil, ErrBadMessage
	}

	if n < requestHeaderSize+len(magicNumber) {
		plog.Errorf("failed to get the header,conn.ReadFromUDP return n < requestHeaderSize + len(magicNumber) ")
		return requestHeader{}, nil, ErrBadMessage
	}

	header = buffer[len(magicNumber) : len(magicNumber)+requestHeaderSize]
	rheader := requestHeader{}
	if !rheader.decode(header) {
		plog.Errorf("invalid header")
		return requestHeader{}, nil, ErrBadMessage
	}
	var buf []byte
	if rheader.size > uint64(len(rbuf)) {
		buf = make([]byte, rheader.size)
	} else {
		buf = rbuf[:rheader.size]
	}
	buf = buffer[requestHeaderSize+len(magicNum) : n] //这里到n，有没有错？   干！我是傻逼，这里忘记加len(magicNum)
	if !encrypted && crc32.ChecksumIEEE(buf) != rheader.crc {
		fmt.Printf("crc32.ChecksumIEEE(buf):%d\n", crc32.ChecksumIEEE(buf))
		fmt.Printf("rheader.crc:%d\n", rheader.crc)
		plog.Errorf("invalid payload checksum")
		return requestHeader{}, nil, ErrBadMessage
	}
	return rheader, buf, nil
}

func sendPoison(conn net.UDPConn, poison []byte, addr *net.UDPAddr) error {

	if _, err := conn.WriteToUDP(poison, addr); err != nil {
		return err
	}
	return nil
}
func sendPoison_my_use_addr(conn net.UDPConn, poison []byte, addr net.Addr) error {

	if _, err := conn.WriteTo(poison, addr); err != nil {
		return err
	}
	return nil
}

func sendPoisonAck(conn net.UDPConn, poisonAck []byte, addr *net.UDPAddr) error {
	return sendPoison(conn, poisonAck, addr)
}

// 上面的decode 和encode,APPL 头部不改
// FIXME:
// context.Context is ignored
func (t *UDP) getConnection(
	target string) (net.UDPConn, error) {
	udp_Addr, _ := t.get_udp_Addr(target)
	conn, err := net.DialUDP("udp", nil, udp_Addr)
	if err != nil {
		panic("UDP getConnection err")
	}
	return *conn, err
}

func (t *UDP) get_udp_Addr(IPaddress_and_port string) (*net.UDPAddr, error) {
	addr, err := net.ResolveUDPAddr("udp", IPaddress_and_port)
	fmt.Printf("in get_udp_addr,after ResolveUDPAddr: IPaddress_and_port: %s\n", IPaddress_and_port)
	if err != nil {
		fmt.Println(err)
		plog.Errorf("ResolveUDPAddr fail")
		panic("ResolveUDPAddr fail")
		//return nil, err
	}
	//
	return addr, err
}

func (t *UDP) serveConn(conn net.UDPConn, addr *net.UDPAddr) error {
	magicNum := make([]byte, len(magicNumber))
	header := make([]byte, requestHeaderSize)
	tbuf := make([]byte, payloadBufferSize)
	for {

		rheader, buf, err := readMessage(conn, header, tbuf, magicNum, t.encrypted, addr)
		if err != nil {
			return err
		}
		if rheader.method == raftType {
			batch := pb.MessageBatch{}
			if err := batch.Unmarshal(buf); err != nil {
				return nil
			}
			t.requestHandler(batch)
		} else {
			chunk := pb.Chunk{}
			if err := chunk.Unmarshal(buf); err != nil {
				return nil
			}
			if !t.chunkHandler(chunk) {
				plog.Errorf("chunk rejected %s", chunkKey(chunk))
				return nil
			}
		}
	}
}

//	func parseAddress(addr string) (string, string, error) {
//		parts := strings.Split(addr, ":")
//		if len(parts) == 2 {
//			return parts[0], parts[1], nil
//		}
//		return "", "", errors.New("failed to get hostname")
//	}
func (t *UDP) Start() error {
	address := t.nhConfig.GetListenAddress()
	//tlsConfig, err := t.nhConfig.GetServerTLSConfig()
	//不加密了

	// listener, err := t.getConnection(address) //

	// hostname, _, _ := parseAddress(address)
	// if !stringutil.IPV4Regex.MatchString(hostname) {
	// 	fmt.Printf("JPF didn't implement using hostname, please use ip and port,such as 127.0.0.1:10004")
	// 	os.Exit(1)
	// 	return nil
	// }
	// if err != nil {
	// 	return err
	// }
	// // workaround the design bug in golang's net package.
	// // https://github.com/golang/go/issues/9334?ts=2
	// toListen := make([]string, 0) //
	// if stringutil.HostnameRegex.MatchString(hostname) {
	// 	ipList, err := net.LookupIP(hostname)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	added := make(map[string]struct{})
	// 	for _, v := range ipList {
	// 		// ipv6 address is ignored
	// 		if v.To4() == nil {
	// 			continue
	// 		}
	// 		if _, ok := added[string(v)]; !ok {
	// 			toListen = append(toListen, fmt.Sprintf("%s:%s", v, port))
	// 			added[string(v)] = struct{}{}
	// 		}
	// 	}
	// } else if stringutil.IPV4Regex.MatchString(hostname) {
	// 	toListen = append(toListen, address)
	// }
	// if len(toListen) > 1 {
	// 	fmt.Printf("len(toListen) > 1 , but JPF didn't implement ,os.Exit(1),please use IPaddress and Port")
	// 	os.Exit(1)
	// }
	// if len(toListen) < 1 {
	// 	fmt.Printf("len(toListen) < 1 ,Impossible")
	// 	os.Exit(1)
	// }
	// address_and_port_in_ip := toListen[0]
	// addr, _ := t.get_udp_Addr(address_and_port_in_ip)
	addr, _ := t.get_udp_Addr(address)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Println("listen UDP error", err)
		os.Exit(1)
		return err
	}
	// listener, err := netutil.NewStoppableListener(address,
	// 	tlsConfig, t.stopper.ShouldStop())
	// if err != nil {
	// 	return err
	// }
	t.connStopper.RunWorker(func() {
		// sync.WaitGroup's doc mentions that
		// "Note that calls with a positive delta that occur when the counter is
		//  zero must happen before a Wait."
		// It is unclear that whether the stdlib is going complain in future
		// releases when Wait() is called when the counter is zero and Add() with
		// positive delta has never been called.
		<-t.connStopper.ShouldStop()
	})
	t.stopper.RunWorker(func() {
		for {
			// conn, err := listener.Accept()
			// if err != nil {
			// 	if err == netutil.ErrListenerStopped {
			// 		return
			// 	}
			// 	panic(err)
			// }
			var once sync.Once
			connCloseCh := make(chan struct{})
			closeFn := func() {
				once.Do(func() {
					select {
					case connCloseCh <- struct{}{}:
					default:
					}
					if err := conn.Close(); err != nil {
						plog.Errorf("failed to close the connection %v", err)
					}
				})
			}
			t.connStopper.RunWorker(func() {
				select {
				case <-t.stopper.ShouldStop():
				case <-connCloseCh:
				}
				closeFn()
			})
			t.connStopper.RunWorker(func() {

				err := t.serveConn(*conn, addr)
				if err != nil {
					address_2 := t.nhConfig.GetListenAddress()

					addr_2, _ := t.get_udp_Addr(address_2)
					conn_2, err_2 := net.ListenUDP("udp", addr)
					if err_2 != nil {
						fmt.Println("listen UDP error", err)
						os.Exit(1)

					}
					t.serveConn(*conn_2, addr_2)
					closeFn()
				}
			})
		}
	})
	return nil
}

// type connection struct {
// 	conn net.UDPConn
// }

// func newConnection(conn net.UDPConn) *connection { //net.UDPConn
// 	return &connection{conn: conn}
// }

// // // Listen
// func start_udp_Listen(address *net.UDPAddr) (*net.UDPConn, error) {
// 	udp_conn, err := net.ListenUDP("udp", address)
// 	if err != nil {
// 		plog.Errorf("ResolveUDPAddr fail")
// 		return nil, err
// 	}
// 	return udp_conn, err
// }

// func (c *connection) Close() error {
// 	return c.conn.Close()
// }
// func (c *connection) Read(b []byte) (int, error) {
// 	return c.conn.Read(b)
// }

// func (c *connection) Write(b []byte) (int, error) {
// 	return c.conn.Write(b)
// }
// func (c *connection) LocalAddr() net.Addr {
// 	panic("not implemented")
// }

// func (c *connection) RemoteAddr() net.Addr {
// 	panic("not implemented")
// }
// func (c *connection) SetDeadline(t time.Time) error {
// 	return c.conn.SetDeadline(t)
// }

// func (c *connection) SetReadDeadline(t time.Time) error {
// 	return c.conn.SetReadDeadline(t)
// }

// func (c *connection) SetWriteDeadline(t time.Time) error {
// 	return c.conn.SetWriteDeadline(t)
// }

type UDPConnection struct {
	conn      net.UDPConn
	header    []byte
	payload   []byte
	encrypted bool
}

var _ raftio.IConnection = (*UDPConnection)(nil)

// NewTCPConnection creates and returns a new UDPConnection instance.
func NewUDPConnection(UDPconn net.UDPConn, encrypted bool) *UDPConnection {
	return &UDPConnection{
		conn:      UDPconn,
		header:    make([]byte, requestHeaderSize),
		payload:   make([]byte, perConnBufSize),
		encrypted: encrypted,
	}
}

// Close closes the TCPConnection instance.
func (c *UDPConnection) Close() {
	if err := c.conn.Close(); err != nil {
		plog.Errorf("failed to close the connection %v", err)
	}
}

// SendMessageBatch sends a raft message batch to remote node.
func (c *UDPConnection) SendMessageBatch(batch pb.MessageBatch) error {
	header := requestHeader{method: raftType}
	sz := batch.SizeUpperLimit()
	var buf []byte
	if len(c.payload) < sz {
		buf = make([]byte, sz)
	} else {
		buf = c.payload
	}
	buf = pb.MustMarshalTo(&batch, buf)
	return writeMessage(c.conn, header, buf, c.header, c.encrypted)
}

func writeMessage(conn net.UDPConn,
	header requestHeader, buf []byte, headerBuf []byte, encrypted bool) error { //先弃用加密?

	header.size = uint64(len(buf))
	if !encrypted {
		header.crc = crc32.ChecksumIEEE(buf)
		fmt.Printf("header.crc when write: %d\n", header.crc)
	}
	headerBuf = header.encode(headerBuf)
	tt := time.Now().Add(magicNumberDuration).Add(headerDuration)
	if err := conn.SetWriteDeadline(tt); err != nil {
		return err
	}
	if len(buf) > 1480 {
		fmt.Printf("buf must be split, but JPF didn't handle ,exit")
		os.Exit(1)
	}
	// if _, err := conn.Write(magicNumber[:]); err != nil {
	// 	return err
	// }
	// if _, err := conn.Write(headerBuf); err != nil {
	// 	return err
	// }
	// sent := 0
	// bufSize := int(recvBufSize)
	// for sent < len(buf) {
	// 	if sent+bufSize > len(buf) {
	// 		bufSize = len(buf) - sent
	// 	}
	// 	tt = time.Now().Add(writeDuration)
	// 	if err := conn.SetWriteDeadline(tt); err != nil {
	// 		return err
	// 	}
	// 	if _, err := conn.Write(buf[sent : sent+bufSize]); err != nil {
	// 		return err
	// 	}
	// 	sent += bufSize
	// }
	// if sent != len(buf) {
	// 	plog.Panicf("sent %d, buf len %d", sent, len(buf))
	// }
	//var to_send_buff []byte
	merge := append(magicNumber[:], headerBuf...)
	to_send_buff := append(merge, buf...)
	fmt.Printf("header.crc when write: %d\n", header.crc)
	fmt.Printf("when write :crc32.ChecksumIEEE(to_send_buff[len(magicNumber)+requestHeaderSize: len(to_send_buff)]) :%d", crc32.ChecksumIEEE(to_send_buff[len(magicNumber)+requestHeaderSize:len(to_send_buff)]))
	if _, err := conn.Write(to_send_buff); err != nil {
		return err
	}
	return nil
}

// TCPSnapshotConnection is the connection for sending raft snapshot chunks to
// remote nodes.
type UDPSnapshotConnection struct {
	conn      net.UDPConn
	header    []byte
	encrypted bool
}

var _ raftio.ISnapshotConnection = (*UDPSnapshotConnection)(nil)

// NewTCPSnapshotConnection creates and returns a new snapshot connection.
func NewUDPSnapshotConnection(conn net.UDPConn,
	encrypted bool) *UDPSnapshotConnection {
	return &UDPSnapshotConnection{
		conn:      conn,
		header:    make([]byte, requestHeaderSize),
		encrypted: encrypted,
	}
}

// Close closes the snapshot connection.
func (c *UDPSnapshotConnection) Close() {
	defer func() {
		if err := c.conn.Close(); err != nil {
			plog.Debugf("failed to close the connection %v", err)
		}
	}()
	Addr_remote := c.conn.RemoteAddr()
	if err := sendPoison_my_use_addr(c.conn, poisonNumber[:], Addr_remote); err != nil {
		return
	}
	waitPoisonAck(c.conn) ///这里为什么要waitack?
}
func waitPoisonAck(conn net.UDPConn) { ////如何实现，调用listen函数？
	ack := make([]byte, len(poisonNumber))
	tt := time.Now().Add(keepAlivePeriod)
	if err := conn.SetReadDeadline(tt); err != nil {
		return
	}
	Addr_remote := conn.RemoteAddr()
	if err := read_poison_number(conn, ack, Addr_remote); err != nil {
		plog.Errorf("failed to get poison ack %v", err)
		return
	}
}

func read_poison_number(conn net.UDPConn,
	magicNum []byte, addr net.Addr) error { //会不会读进来一条信息但是这条信息是别的分片（线程)的正常信息？
	var buffer []byte
	buffer = make([]byte, 1500) //数据报最大大小是1500
	//gpt4o:当缓冲区 p 的大小大于从网络连接中读取的数据时，不会发生错误或异常，只是缓冲区 p 的一部分会被使用来存储读取到的数据，其余部分保持不变
	n, buffer, _ := readMessage_to_buff(conn, buffer)
	if n < len(magicNumber) {
		plog.Errorf("failed to get the header,conn.ReadFromUDP return n < requestHeaderSize + len(magicNumber)")
		return ErrBadMessage
	}
	copy(magicNum, buffer[0:len(magicNumber)])
	if bytes.Equal(magicNum, poisonNumber[:]) {
		if err := sendPoison_my_use_addr(conn, poisonNumber[:], addr); err != nil { //addr 类型不对
			plog.Debugf("failed to send poison ack %v", err)
		}
		return nil
	}
	return ErrBadMessage

}

// SendChunk sends the specified snapshot chunk to remote node.
func (c *UDPSnapshotConnection) SendChunk(chunk pb.Chunk) error {
	header := requestHeader{method: snapshotType}
	sz := chunk.Size()
	buf := make([]byte, sz)
	buf = pb.MustMarshalTo(&chunk, buf)
	return writeMessage(c.conn, header, buf, c.header, c.encrypted)
}

func NewUDPTransport(nhConfig config.NodeHostConfig,
	requestHandler raftio.MessageHandler,
	chunkHandler raftio.ChunkHandler) raftio.ITransport {
	return &UDP{
		nhConfig:       nhConfig,
		stopper:        syncutil.NewStopper(),
		connStopper:    syncutil.NewStopper(),
		requestHandler: requestHandler,
		chunkHandler:   chunkHandler,
		encrypted:      nhConfig.MutualTLS,
	}
}
func (t *UDP) Close() error {
	t.stopper.Stop()
	t.connStopper.Stop()
	return nil
}
func (t *UDP) GetConnection(ctx context.Context,
	target string) (raftio.IConnection, error) {
	conn, err := t.getConnection(target)
	if err != nil {
		return nil, err
	}
	return NewUDPConnection(conn, t.encrypted), nil
}

func (t *UDP) GetSnapshotConnection(ctx context.Context,
	target string) (raftio.ISnapshotConnection, error) {
	conn, err := t.getConnection(target)
	if err != nil {
		return nil, err
	}
	return NewUDPSnapshotConnection(conn, t.encrypted), nil
}

func (t *UDP) Name() string {
	return UDPTransportName
}
