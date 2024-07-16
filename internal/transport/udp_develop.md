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

## udp.go开发 ##

- **quetion**
1. 是否需要仍需链接，tcp与udp二者的库的区别是  
2. 可以从测试error追踪，整个看会比较费时间