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

var (
	// ErrBadMessage is the error returned to indicate the incoming message is
	// corrupted.
	ErrBadMessage       = errors.New("invalid message")
	errPoisonReceived   = errors.New("poison received")
	magicNumber         = [2]byte{0xAE, 0x7D}
	poisonNumber        = [2]byte{0x0, 0x0}
	payloadBufferSize   = settings.SnapshotChunkSize + 1024*128
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
	UDPTransportName         = "go-UDP-transport"
	requestHeaderSize        = 18
	raftType          uint16 = 100
	snapshotType      uint16 = 200
)

type requestHeader struct {
	size   uint64 //大小
	crc    uint32 //校验位
	method uint16 //类型:raft 还是snapshot
}

// 就是把appl头塞进buffer，在本文件writeMessage调用
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

// what is the difference? see https://pkg.go.dev/net#UDPConn.WriteTo .其实只是一个是字符串，一个是UDPAddr对象而已
func sendPoison(conn *net.UDPConn, poison []byte, addr *net.UDPAddr) error {
	tt := time.Now().Add(magicNumberDuration).Add(magicNumberDuration)
	if err := conn.SetWriteDeadline(tt); err != nil {
		return err
	}
	if _, err := conn.WriteToUDP(poison, addr); err != nil {
		return err
	}
	return nil
}

func sendPoison_my_use_addr(conn *net.UDPConn, poison []byte, addr net.Addr) error {
	tt := time.Now().Add(magicNumberDuration).Add(magicNumberDuration)
	if err := conn.SetWriteDeadline(tt); err != nil {
		return err
	}
	if _, err := conn.WriteTo(poison, addr); err != nil {
		return err
	}
	return nil
}

func sendPoisonAck(conn *net.UDPConn, poisonAck []byte, addr *net.UDPAddr) error {
	return sendPoison(conn, poisonAck, addr)
}

func waitPoisonAck(conn *net.UDPConn) { ////如何实现，调用listen函数？
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
func readMessage_to_buff(conn *net.UDPConn,
	buffer []byte) (int, []byte, error) {
	n, _, err := conn.ReadFromUDP(buffer)
	if err != nil {
		plog.Errorf("Error reading from UDP: %v", err)
		return n, buffer, err
	}
	return n, buffer, err
}
func read_poison_number(conn *net.UDPConn,
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

func writeMessage(conn *net.UDPConn,
	header requestHeader, buf []byte, headerBuf []byte, encrypted bool) error { //先弃用加密?加密在哪里看

	header.size = uint64(len(buf))
	if !encrypted {
		header.crc = crc32.ChecksumIEEE(buf)
		// fmt.Printf("header.crc when write: %d\n", header.crc)
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
	merge := append(magicNumber[:], headerBuf...)
	to_send_buff := append(merge, buf...)
	// fmt.Printf("header.crc when write: %d\n", header.crc)
	// fmt.Printf("when write :crc32.ChecksumIEEE(to_send_buff[len(magicNumber)+requestHeaderSize: len(to_send_buff)]) :%d\n", crc32.ChecksumIEEE(to_send_buff[len(magicNumber)+requestHeaderSize:len(to_send_buff)]))
	if _, err := conn.Write(to_send_buff); err != nil {
		return err
	}
	return nil
}

func readMessage(conn *net.UDPConn,
	header []byte, rbuf []byte, magicNum []byte, encrypted bool, addr *net.UDPAddr) (requestHeader, []byte, error) {
	var buffer []byte           // 先整条信息读出来，再逐步分解
	buffer = make([]byte, 1500) //数据报最大大小是1500
	//gpt4o:当缓冲区 p 的大小大于从网络连接中读取的数据时，不会发生错误或异常，只是缓冲区 p 的一部分会被使用来存储读取到的数据，其余部分保持不变
	n, buffer, err := readMessage_to_buff(conn, buffer) //ReadFromUDP,返回*UDPAddr
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
		// fmt.Printf("crc32.ChecksumIEEE(buf):%d\n", crc32.ChecksumIEEE(buf))
		// fmt.Printf("rheader.crc:%d\n", rheader.crc)
		plog.Errorf("invalid payload checksum")
		return requestHeader{}, nil, ErrBadMessage
	}
	return rheader, buf, nil
}

type UDPConnection struct {
	conn      *net.UDPConn
	header    []byte
	payload   []byte
	encrypted bool
}

var _ raftio.IConnection = (*UDPConnection)(nil)

// NewTCPConnection creates and returns a new UDPConnection instance.
func NewUDPConnection(UDPconn *net.UDPConn, encrypted bool) *UDPConnection {
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
		plog.Errorf("Hi! failed to close the connection %v", err)
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

// TCPSnapshotConnection is the connection for sending raft snapshot chunks to
// remote nodes.
type UDPSnapshotConnection struct {
	conn      *net.UDPConn
	header    []byte
	encrypted bool
}

var _ raftio.ISnapshotConnection = (*UDPSnapshotConnection)(nil)

// NewTCPSnapshotConnection creates and returns a new snapshot connection.
func NewUDPSnapshotConnection(conn *net.UDPConn,
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
			plog.Debugf("hey! failed to close the connection %v", err)
		}
	}()
	Addr_remote := c.conn.RemoteAddr()
	if err := sendPoison_my_use_addr(c.conn, poisonNumber[:], Addr_remote); err != nil {
		return
	}
	waitPoisonAck(c.conn) ///这里为什么要waitack?
}

// SendChunk sends the specified snapshot chunk to remote node.
func (c *UDPSnapshotConnection) SendChunk(chunk pb.Chunk) error {
	header := requestHeader{method: snapshotType}
	sz := chunk.Size()
	buf := make([]byte, sz)
	buf = pb.MustMarshalTo(&chunk, buf)
	return writeMessage(c.conn, header, buf, c.header, c.encrypted)
}

type UDP struct {
	stopper        *syncutil.Stopper
	connStopper    *syncutil.Stopper
	requestHandler raftio.MessageHandler
	chunkHandler   raftio.ChunkHandler
	encrypted      bool
	nhConfig       config.NodeHostConfig
}

var _ raftio.ITransport = (*UDP)(nil)

// 上面的decode 和encode,APPL 头部不改
// FIXME:
// context.Context is ignored
func (t *UDP) getConnection(
	target string) (*net.UDPConn, error) {
	udp_Addr, _ := t.get_udp_Addr(target)
	conn, err := net.DialUDP("udp", nil, udp_Addr)
	if err != nil {
		panic("UDP getConnection err")
	}
	return conn, err
}

func (t *UDP) get_udp_Addr(IPaddress_and_port string) (*net.UDPAddr, error) {
	addr, err := net.ResolveUDPAddr("udp", IPaddress_and_port)
	// fmt.Printf("in get_udp_addr,after ResolveUDPAddr: IPaddress_and_port: %s\n", IPaddress_and_port)
	if err != nil {
		fmt.Println(err)
		plog.Errorf("ResolveUDPAddr fail")
		panic("ResolveUDPAddr fail")
		//return nil, err
	}
	//
	return addr, err
}

func (t *UDP) serveConn(conn *net.UDPConn, addr *net.UDPAddr) error {
	magicNum := make([]byte, len(magicNumber))
	header := make([]byte, requestHeaderSize)
	tbuf := make([]byte, payloadBufferSize)
	for {
		// fmt.Printf("2\n")
		rheader, buf, err := readMessage(conn, header, tbuf, magicNum, t.encrypted, addr)
		if err != nil {
			return err
		}
		// plog.Infof("1 msg recieved")
		if rheader.method == raftType {
			batch := pb.MessageBatch{}
			if err := batch.Unmarshal(buf); err != nil {
				return nil
			}
			t.requestHandler(batch)
			// fmt.Printf("have read a packet and handle!\n")
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

func (t *UDP) Start() error {
	address := t.nhConfig.GetListenAddress()
	addr, _ := t.get_udp_Addr(address) // get函数：从str到UDPConn
	conn, err := net.ListenUDP("udp", addr)
	// plog.Infof("just a test")
	if err != nil {
		fmt.Println("listen UDP error", err)
		os.Exit(1) // 直接退出
		return err
	}
	t.connStopper.RunWorker(func() { // 这白开一个goroutine有啥用呢？
		<-t.connStopper.ShouldStop()
	})
	t.stopper.RunWorker(func() {
		var once sync.Once
		connCloseCh := make(chan struct{})
		closeFn := func() {
			once.Do(func() {
				select {
				case connCloseCh <- struct{}{}:
				default:
				}
				if err := conn.Close(); err != nil {
					plog.Errorf("failed to close the connection when Start() in udp.go %v ...", err)
				} else {
					// fmt.Printf("\nClose safely\n\n") // 没到这步的，不对啊，有这个
					plog.Infof("Close safely\n")
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
			err := t.serveConn(conn, addr)
			plog.Errorf("serveConn err is %v", err)
			closeFn()
		})
	})
	return nil
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
