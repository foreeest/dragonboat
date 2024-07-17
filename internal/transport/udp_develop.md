# udp开发记录 #

## Dragonboat原版解读 ##

### 调用链 ###   
- **sending**  
transport.go processMessages -> tcp.go SendMessageBatch -> tcp.go writeMessage -> net.go Write  
- **recieving**
tcp.go serveConn -> transport.go handleRequest -> Nodehost.go HandleMessageBatch  

### 代码分析 ###

- **tcp.go**

1. 前260行是最底层代码，包括序列化header和用net.conn收发，即**最底层封装**(其实还封了一层connection类但太简单直接省略)
2. 接下来是raftio.IConnection的实现，包括TCPConnection和TCPSnapshotConnection，包括创建与关闭链接，发送功能，是**第二层封装**
3. raftio.ITransport的实现，TCP类；刚才可能好奇第二层只有发送，因为接收在第三层的函数serveConn()直接调用第一层；这里还向上调用了transport.go的handleRequest进而Nodehost.go HandleMessageBatch；这层是TCP的**第三层封装**
4. tcp.go和transport.go的关系：后者有一个ITransport及实例Transport，这个东西只是跟raftio.ITransport重名而已，实际上是raftio.ITransport的上层封装，可以理解为**第四层封装**，也是顶层封装了上面由nodehost调用；有一个sendQueue，这东西是用来Send和processMessages之间传的；有一个TransportFactory，似乎这个封装了TCP一遍，但没啥实际功效省略这一层，如果换成UDP换这个Factory应该就可以
5. 此代码用到一系列读写函数，这些函数用到了message.go中定义的序列化与反序列化接口；message.go应该是protocal buffer自动生成的  
**总结**：这是raftio/transport.go接口的实现，通过这份代码也可以理解raftio几个Interface的关系；这份tcp.go显然不是TCP协议的实现，而是对在内核中实现的TCP协议的调用，或者说封装  
 
- **transport.go**

1. sq与conn有对应关系吗？是的，核心在send()函数，当第一次发给某个node的时候，调用connectAndProcess()函数获取一个conn，并将此conn和创建的新sq传给processMessages()启动一个循环的线程，因此建立了processMessages()、conn、sq的一对一对应关系
2. create()再代码最后面，负责选择一个Transport模块，Default就是TCPTransport

**总结**：调用tcp.go的两个Interface，被nodehost.go调用  

**TODO**    
$ 2^{n} $ sendqueue Solution是解决什么问题的？具体来说这些send queue跟node是怎么对应？  
几MB或者GB的大包会如何影响我们的优化吗？  

- **现有udp.go**

1. 目前transport.go纯没改，只有create函数那里TCP改成了UDP  
2. 把time duration和deadline之类的删了  
3. 去掉connection的封装了，没毛病，原来这层封装就不是太必要
4. `udp_test.go`没毛病  
5. snapshot应该收发都没处理

## udp.go开发 ##

### 初步测试 ###

- **quetion**
1. 是否需要仍需链接，tcp与udp二者的库的区别是  
2. 可以从测试error追踪，整个看会比较费时间

- **transport test的报错**
1. start() 那里报`in udp.go close udp 127.0.0.1:26001: use of closed network connection`就说已经关了？然后这个报了贼多次是为啥  
2. 还有一个read报的`read udp 127.0.0.1:26001: use of closed network connection` 
3. 还有start开头的`listen UDP error read udp 127.0.0.1:26001: use of closed network connection`     
4. 似乎每次输出的顺序有点差异，这是并发导致的吗？  
5. 跑单步调试warning说`Too many goroutines, only loaded 1024`  

编程参考：https://www.topgoer.com/%E7%BD%91%E7%BB%9C%E7%BC%96%E7%A8%8B/socket%E7%BC%96%E7%A8%8B/UDP%E7%BC%96%E7%A8%8B.html  

- udp与tcp库
tcp用listen调用accept获取一个conn，然后用这个来收发  
那udp直接拿一个listen用来收发，感觉也没问题，就是一个IP对应一个listen  
先关注一下close操作吧，先不考虑不是close引起的报错  

- solving
1. use of closed connection问题是shouldstop停的，这是啥，其次为什么原来就没事   
2. 修改：改了一行closeFn()，但是感觉没什么影响  
3. 目前找到会产生过多goroutine的原因，是Start()的for没被阻塞

### Solution ###

- 设计
*其实就考虑udp库和承接好transport.go即可，即能否在不改transport.go的前提下写出可用的udp.go*  

1. VR的udp用的是同一个端口12345；dragonboat example中一个nodehost也就一个port，三个分别时63001、63002、63003
2. transport.go收信息用Start()，一个for循环接受连接，每个建立的连接有一个goroutine即serveConn()来读
3. transport.go发信息用send()，后者用一个sq将信息传给一个goroutine即processMessages()来写
4. 按照send()的写法，每个key有一个sq，对应一个processMessages()，对应一个conn，连接到Start()的一个serveConn()
而每个**shard**和node映射到唯一的key？(这个有点难确认又得一路追踪，主要跟Registry.Resolve()相关；但见dragonboat多shard例子中并没有多ports) 总的来说，每个shard、node映射到一个key，也即映射到一个sq，也即一个收一个发goroutine；udp的发被transport.go写死了有多个goroutine，udp的收可以只要一个goroutine
5. 按照transport.go的写法来:connectAndProcess原本是tcp的Dial三次握手，这里就udp的Dial绑定端口，processMessage不变，这样原来的每个tcp Connection仍然有一个udp对应发送goroutine；Start()因为ListenUDP不阻塞，我们不用for+Accept来动态增减connection，而是只Listen一次，然后根据ReadFromUdp读到的addr来分发
6. poison还有必要存在吗？
7. 只有一个线程处理会有什么问题吗？就是serveConn这东西，他是某个连接调用的，还是不知道哪个连接调用的应该没有区别吧

- 开发

1. 先改了Start()没有第一个问题了
2. 有个疑问，这样修改，一旦Close了，就整个通信模块就用不了了？  
3. 两个close一个是写的，一个是读的，写的close了读的还读就报错`Error reading from UDP: read udp 127.0.0.1:26001: use of closed network connection`  
4. 可能fmt输出的顺序和plog不一致？
5. 去掉了非error的fmt的输出
6. `n, remoteAddr, err := conn.ReadFromUDP(buffer)` declared and not used 居然是err  