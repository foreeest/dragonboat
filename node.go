// Copyright 2017-2021 Lei Ni (nilei81@gmail.com) and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dragonboat

import (
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/lni/goutils/logutil"

	"github.com/lni/dragonboat/v4/client"
	"github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/internal/fileutil"
	"github.com/lni/dragonboat/v4/internal/logdb"
	"github.com/lni/dragonboat/v4/internal/raft"
	"github.com/lni/dragonboat/v4/internal/rsm"
	"github.com/lni/dragonboat/v4/internal/server"
	"github.com/lni/dragonboat/v4/internal/settings"
	"github.com/lni/dragonboat/v4/internal/transport"
	"github.com/lni/dragonboat/v4/raftio"
	pb "github.com/lni/dragonboat/v4/raftpb"
	sm "github.com/lni/dragonboat/v4/statemachine"
)

var ( //变量声明
	incomingProposalsMaxLen = settings.Soft.IncomingProposalQueueLength
	incomingReadIndexMaxLen = settings.Soft.IncomingReadIndexQueueLength
	syncTaskInterval        = settings.Soft.SyncTaskInterval
	lazyFreeCycle           = settings.Soft.LazyFreeCycle
)

type pipeline interface { //定义了一个pipline接口,interface（接口）是一种类型，它定义了一组方法的集合。一个类型如果拥有接口中定义的所有方法，则称该类型实现了该接口。
	setCloseReady(*node)
	setStepReady(shardID uint64)
	setCommitReady(shardID uint64)
	setApplyReady(shardID uint64)
	setStreamReady(shardID uint64)
	setSaveReady(shardID uint64)
	setRecoverReady(shardID uint64)
}

// INodeRegistry
type logDBMetrics struct {
	busy int32
}

func (l *logDBMetrics) update(busy bool) { //这个函数保证更新后l.busy只有0和1两中可能的取值，非0则置为1
	v := int32(0) //:= 是一个简短变量声明和初始化的操作符
	if busy {
		v = int32(1)
	}
	atomic.StoreInt32(&l.busy, v) //atomic.StoreInt32 是一个函数，它用于原子地将一个 int32 类型的值存储到指定的内存地址.
}

func (l *logDBMetrics) isBusy() bool {
	return atomic.LoadInt32(&l.busy) != 0 //返回busy的值
}

type leaderInfo struct {
	leaderID uint64
	term     uint64
}

type node struct {
	shardInfo    atomic.Value
	leaderInfo   atomic.Value
	nodeRegistry raftio.INodeRegistry //在分布式系统或基于Raft协议的系统中，raftio.INodeRegistry 是一个接口或类型(registry.go)，它用于管理或注册与Raft协议相关的节点（nodes）。

	// ，raftio.INodeRegistry 具有以下功能：
	// type INodeRegistry interface {
	// 	Close() error
	// 	Add(shardID uint64, replicaID uint64, url string)
	// 	Remove(shardID uint64, replicaID uint64)
	// 	RemoveShard(shardID uint64)
	// 	Resolve(shardID uint64, replicaID uint64) (string, string, error)
	// }

	// 节点注册：允许节点在Raft集群中注册自己，以便它们可以参与共识过程。
	// 节点发现：提供机制来发现集群中的其他节点。

	logdb raftio.ILogDB //在分布式系统或基于Raft协议的系统中，raftio.ILogDB 通常代表一个与Raft协议相关的日志数据库接口。这里的 ILogDB 可能是一个定义了操作Raft日志所需方法的接口。

	// 具体来说，raftio.ILogDB 可能包含以下一些方法（尽管这不是一个固定的接口定义，而是基于一般需求和常见模式的推测）：

	// 写入日志：将Raft日志条目写入到持久化存储中。
	// 读取日志：从持久化存储中读取Raft日志条目。
	// 截断日志：删除Raft日志中的旧条目，以释放存储空间或管理日志大小。
	// 获取最后一条日志的索引：查询最后一条已写入日志的索引。
	// 获取特定索引的日志：根据指定的索引获取日志条目。
	// 获取日志条目的快照：生成和提供Raft日志条目的快照，以便快速恢复或同步状态。
	pipeline      pipeline
	getStreamSink func(uint64, uint64) *transport.Sink
	ss            snapshotState
	configChangeC <-chan configChangeRequest //<-chan configChangeRequest：这表示一个只接收的通道。你不能往这个通道里发送数据，但你可以从这个通道里接收数据。箭头 <- 表示这个通道是一个接收通道。接收元素类型为 configChangeRequest,应该是在客户端？
	snapshotC     <-chan rsm.SSRequest
	toApplyQ      *rsm.TaskQueue
	toCommitQ     *rsm.TaskQueue //表示一个指向 rsm.TaskQueue 类型的指针变量
	syncTask      task
	metrics       *logDBMetrics
	stopC         chan struct{} //在Go语言中，chan struct{} 是一个特殊类型的通道，其中传输的元素是空的struct{}类型。因为struct{}不包含任何字段，它的大小是零，所以使用chan struct{}作为通道可以非常高效地传递信号，而不需要传输任何实际的数据。

	// 当你看到stopC chan struct{}这样的声明时，它通常被用作一个信号通道，用于通知接收者某个操作或流程应该停止。接收到空struct{}就停止
	sysEvents            *sysEventListener ////raftio.ISystemEventListener is what? ????server.SystemEvent is what??   //在event.go中159行
	raftEvents           *raftEventListener
	handleSnapshotStatus func(uint64, uint64, bool) //一个用来处理与快照（snapshot）相关的状态或事件的函数
	sendRaftMessage      func(pb.MY_Message)
	//JPF:add
	//My_sendRaftMessage func(pb.MY_Message)
	//JPF:add
	validateTarget        func(string) bool
	sm                    *rsm.StateMachine
	incomingReadIndexes   *readIndexQueue
	incomingProposals     *entryQueue
	snapshotLock          sync.Mutex
	pendingProposals      pendingProposal //等待Proposal
	pendingReadIndexes    pendingReadIndex
	pendingConfigChange   pendingConfigChange
	pendingSnapshot       pendingSnapshot
	pendingLeaderTransfer pendingLeaderTransfer
	pendingRaftLogQuery   pendingRaftLogQuery //等待日志查询
	initializedC          chan struct{}
	p                     raft.Peer
	logReader             *logdb.LogReader
	snapshotter           *snapshotter
	mq                    *server.MessageQueue
	qs                    *quiesceState
	raftAddress           string
	config                config.Config
	currentTick           uint64
	gcTick                uint64
	appliedIndex          uint64
	pushedIndex           uint64
	confirmedIndex        uint64
	tickMillisecond       uint64
	shardID               uint64
	replicaID             uint64
	instanceID            uint64
	initializedFlag       uint64
	closeOnce             sync.Once
	raftMu                sync.Mutex
	new                   bool
	logDBLimited          bool
	rateLimited           bool
	notifyCommit          bool
}

var _ rsm.INode = (*node)(nil)

// 在Go语言中，var _ rsm.INode = (*node)(nil) 这样的代码是一种空赋值（blank identifier assignment），也被称为类型断言（type assertion）或类型断言的空赋值（type assertion to the blank identifier）。但这里并不是在进行常规的类型断言，而是利用Go的编译时类型检查来确保*node类型实现了rsm.INode接口。

// 具体来说，_是一个空标识符，它表示我们不关心这个变量的值。在这个上下文中，(*node)(nil)是一个指向node类型零值的指针，它被转换为rsm.INode类型。由于我们使用了空标识符_来接收这个值，所以这个值实际上并没有被使用。

// 但是，如果*node类型没有实现rsm.INode接口中定义的所有方法，那么这行代码在编译时会报错。这是因为Go语言会在编译时检查接口的实现，确保类型确实实现了接口中定义的所有方法。

// 因此，var _ rsm.INode = (*node)(nil)这行代码的主要作用是作为一个编译时的检查，确保*node类型正确地实现了rsm.INode接口。这是一种在Go语言中常见的模式，用于确保类型实现了某个接口，而不需要显式地创建和使用该类型的实例。
var instanceID uint64

func newNode(peers map[uint64]string,
	initialMember bool,
	config config.Config,
	nhConfig config.NodeHostConfig,
	createSM rsm.ManagedStateMachineFactory,
	snapshotter *snapshotter,
	logReader *logdb.LogReader,
	pipeline pipeline,
	liQueue *leaderInfoQueue,
	getStreamSink func(uint64, uint64) *transport.Sink,
	handleSnapshotStatus func(uint64, uint64, bool),
	sendMessage func(pb.MY_Message),
	nodeRegistry raftio.INodeRegistry,
	pool *sync.Pool,
	ldb raftio.ILogDB,
	metrics *logDBMetrics,
	sysEvents *sysEventListener) (*node, error) { //创建Node的函数，返回了指向node的指针和一个错误码
	notifyCommit := nhConfig.NotifyCommit
	proposals := newEntryQueue(incomingProposalsMaxLen, lazyFreeCycle)
	readIndexes := newReadIndexQueue(incomingReadIndexMaxLen)
	configChangeC := make(chan configChangeRequest, 1)
	//make(chan configChangeRequest, 1) 使用了 make 函数来创建这个通道，并指定了它的缓冲区大小为 1。这意味着在没有协程接收通道中的值之前，你可以发送一个 configChangeRequest 类型的值到 configChangeC 通道中，
	//而不会阻塞发送方。但是，如果你试图在没有接收方的情况下发送第二个值，那么发送操作将会阻塞，直到有接收方从通道中接收一个值，从而释放缓冲区的一个位置
	snapshotC := make(chan rsm.SSRequest, 1)
	stopC := make(chan struct{})
	mq := server.NewMessageQueue(receiveQueueLen,
		false, lazyFreeCycle, nhConfig.MaxReceiveQueueSize)
	rn := &node{ //初始化node对象，对象中的变量名+冒号+赋值
		shardID:               config.ShardID,   //配置
		replicaID:             config.ReplicaID, //每个raft组都有一个shareID,单个raft组里面的每台机器都有一个replicaID??
		raftAddress:           nhConfig.RaftAddress,
		instanceID:            atomic.AddUint64(&instanceID, 1),
		tickMillisecond:       nhConfig.RTTMillisecond, //可能是心跳间隔
		config:                config,
		incomingProposals:     proposals,
		incomingReadIndexes:   readIndexes,
		configChangeC:         configChangeC, //用于通知配置更改的通道?
		snapshotC:             snapshotC,
		pipeline:              pipeline,
		getStreamSink:         getStreamSink,
		handleSnapshotStatus:  handleSnapshotStatus,
		stopC:                 stopC,
		pendingProposals:      newPendingProposal(config, notifyCommit, pool, proposals),
		pendingReadIndexes:    newPendingReadIndex(pool, readIndexes),
		pendingConfigChange:   newPendingConfigChange(configChangeC, notifyCommit),
		pendingSnapshot:       newPendingSnapshot(snapshotC),
		pendingLeaderTransfer: newPendingLeaderTransfer(),
		pendingRaftLogQuery:   newPendingRaftLogQuery(),
		nodeRegistry:          nodeRegistry,
		snapshotter:           snapshotter, //节点注册或发现的机制。
		logReader:             logReader,
		sendRaftMessage:       sendMessage,
		//My_sendRaftMessage:    My_sendMessage,
		mq:             mq,
		logdb:          ldb,                       // 用于存储Raft日志的数据库或存储系统
		syncTask:       newTask(syncTaskInterval), //同步任务的调度器或执行器
		sysEvents:      sysEvents,
		notifyCommit:   notifyCommit,
		metrics:        metrics,
		initializedC:   make(chan struct{}),
		ss:             snapshotState{},
		validateTarget: nhConfig.GetTargetValidator(),
		qs: &quiesceState{
			electionTick: config.ElectionRTT * 2,
			enabled:      config.Quiesce,
			shardID:      config.ShardID,
			replicaID:    config.ReplicaID,
		}, //节点静默（quiesce）状态的结构体或对象，包含与静默相关的配置和状态。
	}
	ds := createSM(config.ShardID, config.ReplicaID, stopC)
	sm := rsm.NewStateMachine(ds, snapshotter, config, rn, snapshotter.fs)
	if notifyCommit {
		rn.toCommitQ = rsm.NewTaskQueue()
	}
	rn.toApplyQ = sm.TaskQ()
	rn.sm = sm
	rn.raftEvents = newRaftEventListener(config.ShardID,
		config.ReplicaID, nhConfig.EnableMetrics, liQueue)
	new, err := rn.startRaft(config, peers, initialMember)
	if err != nil {
		return nil, err
	}
	rn.new = new
	return rn, nil
}

func (n *node) ReplicaID() uint64 {
	return n.replicaID
}

func (n *node) ShardID() uint64 {
	return n.shardID
}

func (n *node) ShouldStop() <-chan struct{} {
	return n.stopC
}

func (n *node) StepReady() {
	n.pipeline.setStepReady(n.shardID)
}

func (n *node) applyReady() {
	n.pipeline.setApplyReady(n.shardID)
}

func (n *node) commitReady() {
	n.pipeline.setCommitReady(n.shardID)
}

// ApplyUpdate 方法负责处理从 Raft 日志中获取的更新，并根据节点的角色和传入参数的状态来执行相应的操作。
func (n *node) ApplyUpdate(e pb.Entry,
	result sm.Result, rejected bool, ignored bool, notifyRead bool) {
	if n.isWitness() {
		return //如果节点是观察者（或称为“只读副本”），则该方法直接返回，不执行任何操作。观察者节点在 Raft 中通常不参与投票和日志复制，它们只是用来提供读服务以减轻领导者的负载。
	}
	if notifyRead {
		n.pendingReadIndexes.applied(e.Index)
	}
	if !ignored {
		if e.Key == 0 {
			plog.Panicf("key is 0")
		}
		n.pendingProposals.applied(e.ClientID, e.SeriesID, e.Key, result, rejected)
	}
}

func (n *node) ApplyConfigChange(cc pb.ConfigChange,
	key uint64, rejected bool) error {
	n.raftMu.Lock()
	// 	n.raftMu.Lock()：在访问或修改与 Raft 相关的共享资源之前，使用互斥锁 n.raftMu 来确保线程安全。
	// defer n.raftMu.Unlock()：使用 defer 关键字确保在函数返回之前，无论是否发生错误或异常，都会释放互斥锁。
	defer n.raftMu.Unlock()
	if !rejected {
		if err := n.applyConfigChange(cc); err != nil {
			return err
		}
	}
	return n.configChangeProcessed(key, rejected)
}

func (n *node) applyConfigChange(cc pb.ConfigChange) error { //节点数量变更
	if err := n.p.ApplyConfigChange(cc); err != nil {
		return err
	}
	switch cc.Type {
	case pb.AddNode, pb.AddNonVoting, pb.AddWitness: //		有投票权节点，无投票权节点，观测节点
		n.nodeRegistry.Add(n.shardID, cc.ReplicaID, cc.Address)
	case pb.RemoveNode:
		if cc.ReplicaID == n.replicaID {
			plog.Infof("%s applied ConfChange Remove for itself", n.id())
			n.nodeRegistry.RemoveShard(n.shardID)
			n.requestRemoval() //请求将自己下线
			n.notifySelfRemove()
		} else {
			n.nodeRegistry.Remove(n.shardID, cc.ReplicaID)
		}
	default:
		plog.Panicf("unknown config change type, %s", cc.Type)
	}
	return nil
}

func (n *node) configChangeProcessed(key uint64, rejected bool) error {
	if n.isWitness() {
		return nil
	}
	if rejected {
		if err := n.p.RejectConfigChange(); err != nil {
			return err
		}
	} else {
		n.notifyConfigChange()
	}
	n.pendingConfigChange.apply(key, rejected)
	return nil
}

func (n *node) processLeaderUpdate(u pb.LeaderUpdate) {
	if u.Term == 0 {
		return
	}
	leaderInfo := &leaderInfo{
		leaderID: u.LeaderID,
		term:     u.Term,
	}
	n.leaderInfo.Store(leaderInfo)
}

func (n *node) processLogQuery(r pb.LogQueryResult) {
	if r.IsEmpty() {
		return
	}
	outOfRange := false
	if r.Error != nil {
		if errors.Is(r.Error, raft.ErrCompacted) {
			outOfRange = true
		} else {
			panic(r.Error)
		}
	}
	n.pendingRaftLogQuery.returned(outOfRange,
		LogRange{FirstIndex: r.FirstIndex, LastIndex: r.LastIndex}, r.Entries)
}

func (n *node) RestoreRemotes(snapshot pb.Snapshot) error {
	if snapshot.Membership.ConfigChangeId == 0 {
		plog.Panicf("invalid ConfChangeId")
	}
	n.raftMu.Lock()
	defer n.raftMu.Unlock()
	for nid, addr := range snapshot.Membership.Addresses {
		n.nodeRegistry.Add(n.shardID, nid, addr)
	}
	for nid, addr := range snapshot.Membership.NonVotings {
		n.nodeRegistry.Add(n.shardID, nid, addr)
	}
	for nid, addr := range snapshot.Membership.Witnesses {
		n.nodeRegistry.Add(n.shardID, nid, addr)
	}
	for nid := range snapshot.Membership.Removed {
		if nid == n.replicaID {
			n.nodeRegistry.RemoveShard(n.shardID)
			n.requestRemoval()
			n.notifySelfRemove()
		}
	}
	plog.Debugf("%s is restoring remotes", n.id())
	if err := n.p.RestoreRemotes(snapshot); err != nil {
		return err
	}
	n.notifyConfigChange()
	return nil
}

func (n *node) startRaft(cfg config.Config,
	peers map[uint64]string, initial bool) (bool, error) {
	newNode, err := n.replayLog(cfg.ShardID, cfg.ReplicaID)
	if err != nil {
		return false, err
	}
	pas := make([]raft.PeerAddress, 0)
	for k, v := range peers {
		pas = append(pas, raft.PeerAddress{ReplicaID: k, Address: v})
	}
	n.p = raft.Launch(cfg, n.logReader, n.raftEvents, pas, initial, newNode)
	return newNode, nil
}

func (n *node) close() {
	n.requestRemoval()
	n.raftEvents.close()
	n.mq.Close()
	n.pendingReadIndexes.close()
	n.pendingProposals.close()
	n.pendingConfigChange.close()
	n.pendingSnapshot.close()
	n.pendingRaftLogQuery.close()
}

func (n *node) stopped() bool {
	select {
	case <-n.stopC:
		return true
	default:
	}
	return false
}

func (n *node) requestRemoval() {
	n.closeOnce.Do(func() {
		close(n.stopC)
	})
	plog.Debugf("%s called requestRemoval()", n.id())
}

func (n *node) concurrentSnapshot() bool {
	return n.sm.Concurrent()
}

func (n *node) supportClientSession() bool {
	return !n.OnDiskStateMachine() && !n.isWitness()
}

func (n *node) isWitness() bool {
	return n.config.IsWitness
}

func (n *node) OnDiskStateMachine() bool {
	return n.sm.OnDiskStateMachine()
}

func (n *node) proposeSession(session *client.Session,
	timeout uint64) (*RequestState, error) {
	if !n.initialized() {
		return nil, ErrShardNotReady
	}
	if n.isWitness() {
		return nil, ErrInvalidOperation
	}
	if !session.ValidForSessionOp(n.shardID) {
		return nil, ErrInvalidSession
	}
	return n.pendingProposals.propose(session, nil, timeout)
}

func (n *node) payloadTooBig(sz int) bool {
	if n.config.MaxInMemLogSize == 0 {
		return false
	}
	return uint64(sz+settings.EntryNonCmdFieldsSize) > n.config.MaxInMemLogSize
}

func (n *node) propose(session *client.Session,
	cmd []byte, timeout uint64) (*RequestState, error) {
	if !n.initialized() {
		return nil, ErrShardNotReady
	}
	if n.isWitness() {
		return nil, ErrInvalidOperation
	}
	if !session.ValidForProposal(n.shardID) {
		return nil, ErrInvalidSession
	}
	if n.payloadTooBig(len(cmd)) {
		return nil, ErrPayloadTooBig
	}
	return n.pendingProposals.propose(session, cmd, timeout)
}

func (n *node) read(timeout uint64) (*RequestState, error) {
	if !n.initialized() {
		return nil, ErrShardNotReady
	}
	if n.isWitness() {
		return nil, ErrInvalidOperation
	}
	rs, err := n.pendingReadIndexes.read(timeout)
	if err == nil {
		rs.node = n
	}
	return rs, err
}

func (n *node) requestLeaderTransfer(replicaID uint64) error {
	if !n.initialized() {
		return ErrShardNotReady
	}
	if n.isWitness() {
		return ErrInvalidOperation
	}
	return n.pendingLeaderTransfer.request(replicaID)
}

func (n *node) requestSnapshot(opt SnapshotOption,
	timeout uint64) (*RequestState, error) {
	if !n.initialized() {
		return nil, ErrShardNotReady
	}
	if n.isWitness() {
		return nil, ErrInvalidOperation
	}
	st := rsm.UserRequested
	if opt.Exported {
		plog.Debugf("%s called export snapshot", n.id())
		st = rsm.Exported
		exist, err := fileutil.Exist(opt.ExportPath, n.snapshotter.fs)
		if err != nil {
			return nil, err
		}
		if !exist {
			return nil, ErrDirNotExist
		}
	} else {
		if len(opt.ExportPath) > 0 {
			plog.Warningf("opt.ExportPath set when not exporting a snapshot")
			opt.ExportPath = ""
		}
	}
	return n.pendingSnapshot.request(st,
		opt.ExportPath,
		opt.OverrideCompactionOverhead,
		opt.CompactionOverhead,
		opt.CompactionIndex,
		timeout)
}

func (n *node) queryRaftLog(firstIndex uint64,
	lastIndex uint64, maxSize uint64) (*RequestState, error) {
	if !n.initialized() {
		return nil, ErrShardNotReady
	}
	if n.isWitness() {
		return nil, ErrInvalidOperation
	}
	return n.pendingRaftLogQuery.add(firstIndex, lastIndex, maxSize)
}

func (n *node) reportIgnoredSnapshotRequest(key uint64) {
	n.pendingSnapshot.apply(key, true, false, 0)
}

func (n *node) requestConfigChange(cct pb.ConfigChangeType,
	replicaID uint64, target string, orderID uint64,
	timeout uint64) (*RequestState, error) {
	if !n.initialized() {
		return nil, ErrShardNotReady
	}
	if n.isWitness() {
		return nil, ErrInvalidOperation
	}
	if cct != pb.RemoveNode && !n.validateTarget(target) {
		return nil, ErrInvalidAddress
	}
	cc := pb.ConfigChange{
		Type:           cct,
		ReplicaID:      replicaID,
		ConfigChangeId: orderID,
		Address:        target,
	}
	return n.pendingConfigChange.request(cc, timeout)
}

func (n *node) requestDeleteNodeWithOrderID(replicaID uint64,
	order uint64, timeout uint64) (*RequestState, error) {
	return n.requestConfigChange(pb.RemoveNode, replicaID, "", order, timeout)
}

func (n *node) requestAddNodeWithOrderID(replicaID uint64,
	target string, order uint64, timeout uint64) (*RequestState, error) {
	return n.requestConfigChange(pb.AddNode, replicaID, target, order, timeout)
}

func (n *node) requestAddNonVotingWithOrderID(replicaID uint64,
	target string, order uint64, timeout uint64) (*RequestState, error) {
	return n.requestConfigChange(pb.AddNonVoting, replicaID, target, order, timeout)
}

func (n *node) requestAddWitnessWithOrderID(replicaID uint64,
	target string, order uint64, timeout uint64) (*RequestState, error) {
	return n.requestConfigChange(pb.AddWitness, replicaID, target, order, timeout)
}

func (n *node) getLeaderID() (uint64, uint64, bool) {
	lv := n.leaderInfo.Load()
	if lv == nil {
		return 0, 0, false
	}
	leaderInfo := lv.(*leaderInfo)
	return leaderInfo.leaderID, leaderInfo.term, leaderInfo.leaderID != raft.NoLeader
}

func (n *node) destroy() error {
	return n.sm.Close()
}

func (n *node) destroyed() bool {
	select {
	case <-n.sm.DestroyedC():
		return true
	default:
	}

	return false
}

func (n *node) offloaded() {
	if n.sm.Offloaded() {
		n.pipeline.setCloseReady(n)
		n.sysEvents.Publish(server.SystemEvent{
			Type:      server.NodeUnloaded,
			ShardID:   n.shardID,
			ReplicaID: n.replicaID,
		})
	}
}

func (n *node) loaded() {
	n.sm.Loaded()
}

func (n *node) pushTask(rec rsm.Task, notify bool) {
	if n.notifyCommit {
		n.toCommitQ.Add(rec)
		if notify {
			n.commitReady()
		}
	} else {
		n.toApplyQ.Add(rec)
		if notify {
			n.applyReady()
		}
	}
}

func (n *node) pushEntries(ents []pb.Entry) {
	if len(ents) == 0 {
		return
	}
	n.pushTask(rsm.Task{Entries: ents}, false)
	n.pushedIndex = ents[len(ents)-1].Index
}

func (n *node) pushStreamSnapshotRequest(shardID uint64, replicaID uint64) {
	n.pushTask(rsm.Task{
		ShardID:   shardID,
		ReplicaID: replicaID,
		Stream:    true,
	}, true)
}

func (n *node) pushTakeSnapshotRequest(req rsm.SSRequest) {
	n.pushTask(rsm.Task{
		Save:      true,
		SSRequest: req,
	}, true)
}

func (n *node) pushSnapshot(ss pb.Snapshot, applied uint64) {
	if pb.IsEmptySnapshot(ss) {
		return
	}
	if ss.Index < n.pushedIndex ||
		ss.Index < n.ss.getIndex() ||
		ss.Index < applied {
		plog.Panicf("out of date snapshot, index %d, pushed %d, applied %d, ss %d",
			ss.Index, n.pushedIndex, applied, n.ss.getIndex())
	}
	n.pushTask(rsm.Task{
		Recover: true,
		Index:   ss.Index,
	}, true)
	n.ss.setIndex(ss.Index)
	n.pushedIndex = ss.Index
}

func (n *node) replayLog(shardID uint64, replicaID uint64) (bool, error) {
	plog.Infof("%s replaying raft logs", n.id())
	ss, err := n.snapshotter.GetSnapshotFromLogDB()
	if err != nil && !n.snapshotter.IsNoSnapshotError(err) {
		return false, errors.Wrapf(err, "%s failed to get latest snapshot", n.id())
	}
	if !pb.IsEmptySnapshot(ss) {
		if err = n.logReader.ApplySnapshot(ss); err != nil {
			return false, errors.Wrapf(err, "%s failed to apply snapshot", n.id())
		}
	}
	rs, err := n.logdb.ReadRaftState(shardID, replicaID, ss.Index)
	if errors.Is(err, raftio.ErrNoSavedLog) {
		return true, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "%s ReadRaftState failed", n.id())
	}
	hasRaftState := !pb.IsEmptyState(rs.State)
	if hasRaftState {
		plog.Infof("%s logdb first entry %d size %d commit %d term %d",
			n.id(), rs.FirstIndex, rs.EntryCount, rs.State.Commit, rs.State.Term)
		n.logReader.SetState(rs.State)
	}
	n.logReader.SetRange(rs.FirstIndex, rs.EntryCount)
	return !(ss.Index > 0 || rs.EntryCount > 0 || hasRaftState), nil
}

func (n *node) saveSnapshotRequired(applied uint64) bool {
	if n.config.SnapshotEntries == 0 {
		return false
	}
	index := n.ss.getIndex()
	if n.pushedIndex <= n.config.SnapshotEntries+index ||
		applied <= n.config.SnapshotEntries+index ||
		applied <= n.config.SnapshotEntries+n.ss.getReqIndex() {
		return false
	}
	if n.isBusySnapshotting() {
		return false
	}
	plog.Debugf("%s requested to create %s", n.id(), n.ssid(applied))
	n.ss.setReqIndex(applied)
	return true
}

func isSoftSnapshotError(err error) bool {
	return errors.Is(err, raft.ErrCompacted) ||
		errors.Is(err, raft.ErrSnapshotOutOfDate)
}

func saveAborted(err error) bool {
	return errors.Is(err, sm.ErrSnapshotStopped) ||
		errors.Is(err, sm.ErrSnapshotAborted)
}

func snapshotCommitAborted(err error) bool {
	return errors.Is(err, errSnapshotOutOfDate)
}

func streamAborted(err error) bool {
	return saveAborted(err) || errors.Is(err, sm.ErrSnapshotStreaming)
}

func openAborted(err error) bool {
	return errors.Is(err, sm.ErrOpenStopped)
}

func recoverAborted(err error) bool {
	return errors.Is(err, sm.ErrSnapshotStopped) ||
		errors.Is(err, raft.ErrSnapshotOutOfDate)
}

func (n *node) save(rec rsm.Task) error {
	index, err := n.doSave(rec.SSRequest)
	if err != nil {
		return err
	}
	n.pendingSnapshot.apply(rec.SSRequest.Key, index == 0, false, index)
	n.sysEvents.Publish(server.SystemEvent{
		Type:      server.SnapshotCreated,
		ShardID:   n.shardID,
		ReplicaID: n.replicaID,
	})
	return nil
}

func (n *node) doSave(req rsm.SSRequest) (uint64, error) {
	n.snapshotLock.Lock()
	defer n.snapshotLock.Unlock()
	if !req.Exported() && n.sm.GetLastApplied() <= n.ss.getIndex() {
		// a snapshot has been pushed to the sm but not applied yet
		// or the snapshot has been applied and there is no further progress
		return 0, nil
	}
	ss, ssenv, err := n.sm.Save(req)
	if err != nil {
		if saveAborted(err) {
			plog.Warningf("%s save snapshot aborted, %v", n.id(), err)
			ssenv.MustRemoveTempDir()
			n.pendingSnapshot.apply(req.Key, false, true, 0)
			return 0, nil
		} else if isSoftSnapshotError(err) {
			// e.g. trying to save a snapshot at the same index twice
			return 0, nil
		}
		return 0, errors.Wrapf(err, "%s save snapshot failed", n.id())
	}
	plog.Infof("%s saved %s, term %d, file count %d",
		n.id(), n.ssid(ss.Index), ss.Term, len(ss.Files))
	if err := n.snapshotter.Commit(ss, req); err != nil {
		if snapshotCommitAborted(err) || saveAborted(err) {
			// saveAborted() will only be true in monkey test
			// commit abort happens when the final dir already exists, probably due to
			// incoming snapshot
			ssenv.MustRemoveTempDir()
			return 0, nil
		}
		return 0, errors.Wrapf(err, "%s commit snapshot failed", n.id())
	}
	if req.Exported() {
		return ss.Index, nil
	}
	if !ss.Validate(n.snapshotter.fs) {
		plog.Panicf("%s generated invalid snapshot %v", n.id(), ss)
	}
	if err = n.logReader.CreateSnapshot(ss); err != nil {
		if isSoftSnapshotError(err) {
			return 0, nil
		}
		return 0, errors.Wrapf(err, "%s create snapshot failed", n.id())
	}
	n.compactLog(req, ss.Index)
	n.ss.setIndex(ss.Index)
	return ss.Index, nil
}

func (n *node) compactLog(req rsm.SSRequest, index uint64) {
	if compactionIndex, ok := n.getCompactionIndex(req, index); ok {
		n.ss.setCompactLogTo(compactionIndex)
	}
}

func (n *node) getCompactionIndex(req rsm.SSRequest, index uint64) (uint64, bool) {
	if req.OverrideCompaction {
		if req.CompactionIndex > 0 {
			if index >= req.CompactionIndex+1 {
				return req.CompactionIndex, true
			}
			return 0, false
		}
		if index > req.CompactionOverhead {
			return index - req.CompactionOverhead, true
		}
		return 0, false
	}
	if index > n.config.CompactionOverhead {
		return index - n.config.CompactionOverhead, true
	}

	return 0, false
}

func (n *node) stream(sink pb.IChunkSink) error {
	if sink != nil {
		plog.Infof("%s requested to stream to %d", n.id(), sink.ToReplicaID())
		if err := n.sm.Stream(sink); err != nil {
			if !streamAborted(err) {
				return errors.Wrapf(err, "%s stream failed", n.id())
			}
		}
	}
	return nil
}

func (n *node) recover(rec rsm.Task) (_ uint64, err error) {
	n.snapshotLock.Lock()
	defer n.snapshotLock.Unlock()
	if rec.Initial && n.OnDiskStateMachine() {
		plog.Debugf("%s on disk SM is beng initialized", n.id())
		idx, err := n.sm.OpenOnDiskStateMachine()
		if err != nil {
			if openAborted(err) {
				plog.Warningf("%s aborted OpenOnDiskStateMachine", n.id())
				return 0, nil
			}
			return 0, errors.Wrapf(err, "%s OpenOnDiskStateMachine failed", n.id())
		}
		if idx > 0 && rec.NewNode {
			plog.Panicf("%s new node at non-zero index %d", n.id(), idx)
		}
	}
	ss, err := n.sm.Recover(rec)
	if err != nil {
		if recoverAborted(err) {
			plog.Warningf("%s aborted recovery", n.id())
			return 0, nil
		}
		return 0, errors.Wrapf(err, "%s recover failed", n.id())
	}
	if !pb.IsEmptySnapshot(ss) {
		defer func() {
			err = firstError(err, ss.Unref())
		}()
		plog.Infof("%s recovered from %s", n.id(), n.ssid(ss.Index))
		if n.OnDiskStateMachine() {
			if err := n.sm.Sync(); err != nil {
				return 0, errors.Wrapf(err, "%s sync failed", n.id())
			}
			if err := n.snapshotter.Shrink(ss.Index); err != nil {
				return 0, errors.Wrapf(err, "%s shrink failed", n.id())
			}
		}
		n.compactLog(rsm.DefaultSSRequest, ss.Index)
	}
	n.sysEvents.Publish(server.SystemEvent{
		Type:      server.SnapshotRecovered,
		ShardID:   n.shardID,
		ReplicaID: n.replicaID,
		Index:     ss.Index,
	})
	return ss.Index, nil
}

func (n *node) streamDone() {
	n.ss.notifySnapshotStatus(false, false, true, false, 0)
	n.applyReady()
}

func (n *node) saveDone() {
	n.ss.notifySnapshotStatus(true, false, false, false, 0)
	n.applyReady()
}

func (n *node) recoverDone(index uint64) {
	if !n.initialized() {
		n.initialSnapshotDone(index)
	} else {
		n.recoverFromSnapshotDone()
	}
}

func (n *node) initialSnapshotDone(index uint64) {
	n.ss.notifySnapshotStatus(false, true, false, true, index)
	n.applyReady()
}

func (n *node) recoverFromSnapshotDone() {
	n.ss.notifySnapshotStatus(false, true, false, false, 0)
	n.applyReady()
}

func (n *node) handleTask(ts []rsm.Task, es []sm.Entry) (rsm.Task, error) {
	return n.sm.Handle(ts, es)
}

func (n *node) removeSnapshotFlagFile(index uint64) error {
	return n.snapshotter.removeFlagFile(index)
}

func (n *node) runSyncTask() {
	if !n.sm.OnDiskStateMachine() {
		return
	}
	if !n.syncTask.timeToRun(n.millisecondSinceStart()) {
		return
	}
	if !n.sm.TaskChanBusy() {
		n.pushTask(rsm.Task{PeriodicSync: true}, true)
	}
}

func (n *node) removeLog() error {
	if n.ss.hasCompactLogTo() {
		compactTo := n.ss.getCompactLogTo()
		if compactTo == 0 {
			panic("racy compact log to value?")
		}
		if err := n.logReader.Compact(compactTo); err != nil {
			if err != raft.ErrCompacted {
				return err
			}
		}
		if err := n.logdb.RemoveEntriesTo(n.shardID,
			n.replicaID, compactTo); err != nil {
			return err
		}
		plog.Infof("%s compacted log up to index %d", n.id(), compactTo)
		n.ss.setCompactedTo(compactTo)
		n.sysEvents.Publish(server.SystemEvent{
			Type:      server.LogCompacted,
			ShardID:   n.shardID,
			ReplicaID: n.replicaID,
			Index:     compactTo,
		})
		if !n.config.DisableAutoCompactions {
			if _, err := n.requestCompaction(); err != nil {
				if err != ErrRejected {
					return errors.Wrapf(err, "%s failed to request compaction", n.id())
				}
			}
		}
	}
	return nil
}

func (n *node) requestCompaction() (*SysOpState, error) {
	if compactTo := n.ss.getCompactedTo(); compactTo > 0 {
		done, err := n.logdb.CompactEntriesTo(n.shardID, n.replicaID, compactTo)
		if err != nil {
			return nil, err
		}
		n.sysEvents.Publish(server.SystemEvent{
			Type:      server.LogDBCompacted,
			ShardID:   n.shardID,
			ReplicaID: n.replicaID,
			Index:     compactTo,
		})
		return &SysOpState{completedC: done}, nil
	}
	return nil, ErrRejected
}

func isFreeOrderMessage(m pb.MY_Message) bool {
	return m.Type == pb.Replicate || m.Type == pb.Ping
}

// JPF add:
// func My_isFreeOrderMessage(m pb.MY_Message) bool {
// 	return m.Type == pb.Replicate || m.Type == pb.Ping
// }

// JPF:add
func (n *node) sendEnterQuiesceMessages() {
	for replicaID := range n.sm.GetMembership().Addresses {
		if replicaID != n.replicaID {
			var to_list []uint64
			to_list = append(to_list, replicaID)
			msg := pb.MY_Message{
				Type: pb.Quiesce,
				From: n.replicaID,
				//To:      replicaID,
				To:      to_list,
				ShardID: n.shardID,
			}
			n.sendRaftMessage(msg)
		}
	}
}

func (n *node) sendMessages(msgs []pb.MY_Message) {
	for _, msg := range msgs {
		if !isFreeOrderMessage(msg) {
			msg.ShardID = n.shardID
			n.sendRaftMessage(msg)
		}
	}
}

func (n *node) sendReplicateMessages(ud pb.Update) {
	for _, msg := range ud.My_Messages {
		if isFreeOrderMessage(msg) {
			msg.ShardID = n.shardID
			n.sendRaftMessage(msg)
		}
	}
	for _, My_msg := range ud.My_Messages {
		if isFreeOrderMessage(My_msg) {
			My_msg.ShardID = n.shardID
			n.sendRaftMessage(My_msg)
		}
	}
}

func (n *node) getUpdate() (pb.Update, bool, error) {
	moreEntries := n.moreEntriesToApply()
	if n.p.HasUpdate(moreEntries) ||
		n.confirmedIndex != n.appliedIndex ||
		n.ss.hasCompactLogTo() || n.ss.hasCompactedTo() {
		if n.appliedIndex < n.confirmedIndex {
			plog.Panicf("applied index moving backwards, %d, now %d",
				n.confirmedIndex, n.appliedIndex)
		}
		ud, err := n.p.GetUpdate(moreEntries, n.appliedIndex)
		if err != nil {
			return pb.Update{}, false, err
		}
		n.confirmedIndex = n.appliedIndex
		return ud, true, nil
	}
	return pb.Update{}, false, nil
}

func (n *node) processDroppedReadIndexes(ud pb.Update) {
	for _, sysctx := range ud.DroppedReadIndexes {
		n.pendingReadIndexes.dropped(sysctx)
	}
}

func (n *node) processDroppedEntries(ud pb.Update) {
	for _, e := range ud.DroppedEntries {
		if e.IsProposal() {
			n.pendingProposals.dropped(e.ClientID, e.SeriesID, e.Key)
		} else if e.Type == pb.ConfigChangeEntry {
			n.pendingConfigChange.dropped(e.Key)
		} else {
			plog.Panicf("unknown entry type %s", e.Type)
		}
	}
}

func (n *node) notifyCommittedEntries() {
	tasks := n.toCommitQ.GetAll()
	for _, t := range tasks {
		for _, e := range t.Entries {
			if e.IsProposal() {
				n.pendingProposals.committed(e.ClientID, e.SeriesID, e.Key)
			} else if e.Type == pb.ConfigChangeEntry {
				n.pendingConfigChange.committed(e.Key)
			} else {
				plog.Panicf("unknown entry type %s", e.Type)
			}
		}
		n.toApplyQ.Add(t)
	}
	if len(tasks) > 0 {
		n.applyReady()
	}
}

func (n *node) processReadyToRead(ud pb.Update) {
	if len(ud.ReadyToReads) > 0 {
		n.pendingReadIndexes.addReady(ud.ReadyToReads)
		n.pendingReadIndexes.applied(ud.LastApplied)
	}
}

func (n *node) processSnapshot(ud pb.Update) error {
	if !pb.IsEmptySnapshot(ud.Snapshot) {
		err := n.logReader.ApplySnapshot(ud.Snapshot)
		if err != nil && !isSoftSnapshotError(err) {
			return errors.Wrapf(err, "%s failed to apply snapshot", n.id())
		}
		plog.Debugf("%s, push snapshot %d", n.id(), ud.Snapshot.Index)
		n.pushSnapshot(ud.Snapshot, ud.LastApplied)
	}
	return nil
}

func (n *node) applyRaftUpdates(ud pb.Update) {
	n.pushEntries(pb.EntriesToApply(ud.CommittedEntries, n.pushedIndex, true))
}

func (n *node) processRaftUpdate(ud pb.Update) error {
	if err := n.logReader.Append(ud.EntriesToSave); err != nil {
		return err
	}
	n.sendMessages(ud.My_Messages)
	if err := n.removeLog(); err != nil {
		return err
	}
	n.runSyncTask()
	if n.saveSnapshotRequired(ud.LastApplied) {
		n.pushTakeSnapshotRequest(rsm.SSRequest{})
	}
	return nil
}

func (n *node) commitRaftUpdate(ud pb.Update) {
	n.raftMu.Lock()
	n.p.Commit(ud)
	n.raftMu.Unlock()
}

func (n *node) moreEntriesToApply() bool {
	return n.toApplyQ.MoreEntryToApply()
}

func (n *node) hasEntryToApply() bool {
	return n.p.HasEntryToApply()
}

func (n *node) updateAppliedIndex() uint64 {
	n.appliedIndex = n.sm.GetLastApplied()
	n.p.NotifyRaftLastApplied(n.appliedIndex)
	return n.appliedIndex
}

func (n *node) stepNode() (pb.Update, bool, error) {
	n.raftMu.Lock()
	defer n.raftMu.Unlock()
	if n.initialized() {
		hasEvent, err := n.handleEvents()
		if err != nil {
			return pb.Update{}, false, err
		}
		if hasEvent {
			if n.qs.newQuiesceState() {
				n.sendEnterQuiesceMessages()
			}
			ud, hasUpdate, err := n.getUpdate()
			if err != nil {
				return pb.Update{}, false, err
			}
			return ud, hasUpdate, nil
		}
	}
	return pb.Update{}, false, nil
}

func (n *node) handleEvents() (bool, error) {
	hasEvent := false
	lastApplied := n.updateAppliedIndex()
	if lastApplied != n.confirmedIndex {
		hasEvent = true
	}
	if n.hasEntryToApply() {
		hasEvent = true
	}
	event, err := n.handleReadIndex()
	if err != nil {
		return false, err
	}
	if event {
		hasEvent = true
	}
	event, err = n.handleReceivedMessages()
	if err != nil {
		return false, err
	}
	if event {
		hasEvent = true
	}
	event, err = n.handleConfigChange()
	if err != nil {
		return false, err
	}
	if event {
		hasEvent = true
	}
	event, err = n.handleProposals()
	if err != nil {
		return false, err
	}
	if event {
		hasEvent = true
	}
	event, err = n.handleLeaderTransfer()
	if err != nil {
		return false, err
	}
	if event {
		hasEvent = true
	}
	if n.handleSnapshot(lastApplied) {
		hasEvent = true
	}
	if n.handleCompaction() {
		hasEvent = true
	}
	event, err = n.handleLogQuery()
	if err != nil {
		return false, err
	}
	if event {
		hasEvent = true
	}
	n.gc()
	if hasEvent {
		n.pendingReadIndexes.applied(lastApplied)
	}
	return hasEvent, nil
}

func (n *node) gc() {
	if n.gcTick != n.currentTick {
		n.pendingProposals.gc()
		n.pendingConfigChange.gc()
		n.pendingSnapshot.gc()
		n.gcTick = n.currentTick
	}
}

func (n *node) handleCompaction() bool {
	return n.ss.hasCompactedTo() || n.ss.hasCompactLogTo()
}

func (n *node) handleLogQuery() (bool, error) {
	if req := n.pendingRaftLogQuery.get(); req != nil {
		if err := n.p.QueryRaftLog(req.logRange.FirstIndex,
			req.logRange.LastIndex, req.maxSize); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (n *node) handleLeaderTransfer() (bool, error) {
	target, ok := n.pendingLeaderTransfer.get()
	if ok {
		if err := n.p.RequestLeaderTransfer(target); err != nil {
			return false, err
		}
	}
	return ok, nil
}

func (n *node) handleSnapshot(lastApplied uint64) bool {
	var req rsm.SSRequest
	select {
	case req = <-n.snapshotC:
	default:
		return false
	}
	if !req.Exported() && lastApplied == n.ss.getReqIndex() {
		n.reportIgnoredSnapshotRequest(req.Key)
		return false
	}
	n.ss.setReqIndex(lastApplied)
	n.pushTakeSnapshotRequest(req)
	return true
}

func (n *node) handleProposals() (bool, error) {
	rateLimited := n.p.RateLimited()
	if n.rateLimited != rateLimited {
		n.rateLimited = rateLimited
		plog.Infof("%s new rate limit state is %t", n.id(), rateLimited)
	}
	logDBBusy := n.logDBBusy()
	if n.logDBLimited != logDBBusy {
		n.logDBLimited = logDBBusy
		plog.Infof("%s new LogDB busy state is %t", n.id(), logDBBusy)
	}
	paused := logDBBusy || n.rateLimited
	if entries := n.incomingProposals.get(paused); len(entries) > 0 {
		if err := n.p.ProposeEntries(entries); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (n *node) handleReadIndex() (bool, error) {
	if reqs := n.incomingReadIndexes.get(); len(reqs) > 0 {
		n.qs.record(pb.ReadIndex)
		ctx := n.pendingReadIndexes.nextCtx()
		n.pendingReadIndexes.add(ctx, reqs)
		if err := n.p.ReadIndex(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (n *node) handleConfigChange() (bool, error) {
	if len(n.configChangeC) == 0 {
		return false, nil
	}
	select {
	case req, ok := <-n.configChangeC:
		if !ok {
			n.configChangeC = nil
		} else {
			n.qs.record(pb.ConfigChangeEvent)
			var cc pb.ConfigChange
			pb.MustUnmarshal(&cc, req.data)
			if err := n.p.ProposeConfigChange(cc, req.key); err != nil {
				return false, err
			}
		}
	default:
		return false, nil
	}
	return true, nil
}

func (n *node) isBusySnapshotting() bool {
	snapshotting := n.ss.recovering()
	if !n.concurrentSnapshot() {
		snapshotting = snapshotting || n.ss.saving()
	}
	return snapshotting && n.sm.TaskChanBusy()
}

func (n *node) recordMessage(m pb.MY_Message) {
	if (m.Type == pb.Heartbeat || m.Type == pb.HeartbeatResp) && m.Hint > 0 {
		n.qs.record(pb.ReadIndex)
	} else {
		n.qs.record(m.Type)
	}
}

func (n *node) handleReceivedMessages() (bool, error) {
	count := uint64(0)
	busy := n.isBusySnapshotting()
	msgs := n.mq.Get()
	for _, m := range msgs {
		if m.Type == pb.LocalTick {
			count++
		} else if m.Type == pb.Replicate && busy {
			continue
		}
		done, err := n.handleMessage(m)
		if err != nil {
			return false, err
		}
		if !done {
			n.recordMessage(m)
			if err := n.p.Handle(m); err != nil {
				return false, err
			}
		}
	}
	if count > n.config.ElectionRTT/2 {
		plog.Warningf("%s had %d LocalTick msgs in one batch", n.id(), count)
	}
	if lazyFreeCycle > 0 {
		for i := range msgs {
			msgs[i].Entries = nil
		}
	}
	return len(msgs) > 0, nil
}

func (n *node) handleMessage(m pb.MY_Message) (bool, error) {
	switch m.Type {
	case pb.LocalTick:
		if err := n.tick(m.Hint); err != nil {
			return false, err
		}
	case pb.Quiesce:
		n.qs.tryEnterQuiesce()
	case pb.SnapshotStatus:
		plog.Debugf("%s got ReportSnapshot from %d, rejected %t",
			n.id(), m.From, m.Reject)
		if err := n.p.ReportSnapshotStatus(m.From, m.Reject); err != nil {
			return false, err
		}
	case pb.Unreachable:
		if err := n.p.ReportUnreachableNode(m.From); err != nil {
			return false, err
		}
	default:
		return false, nil
	}
	return true, nil
}

func (n *node) setInitialStatus(index uint64) {
	if n.initialized() {
		plog.Panicf("setInitialStatus called twice")
	}
	plog.Infof("%s initial index set to %d", n.id(), index)
	n.ss.setIndex(index)
	n.pushedIndex = index
	n.setInitialized()
}

func (n *node) handleSnapshotTask(task rsm.Task) {
	if n.ss.recovering() {
		plog.Panicf("%s recovering from snapshot again on %s",
			n.id(), n.getRaftAddress())
	}
	if task.Recover {
		n.reportRecoverSnapshot(task)
	} else if task.Save {
		if n.ss.saving() {
			plog.Warningf("%s taking snapshot, ignored new snapshot req", n.id())
			n.reportIgnoredSnapshotRequest(task.SSRequest.Key)
			return
		}
		n.reportSaveSnapshot(task)
	} else if task.Stream {
		if !n.canStream() {
			n.reportSnapshotStatus(task.ShardID, task.ReplicaID, true)
			return
		}
		n.reportStreamSnapshot(task)
	} else {
		plog.Panicf("unknown task type %+v", task)
	}
}

func (n *node) reportSnapshotStatus(shardID uint64,
	replicaID uint64, failed bool) {
	n.handleSnapshotStatus(shardID, replicaID, failed)
}

func (n *node) reportStreamSnapshot(rec rsm.Task) {
	n.ss.setStreaming()
	getSinkFn := func() pb.IChunkSink {
		conn := n.getStreamSink(rec.ShardID, rec.ReplicaID)
		if conn == nil {
			plog.Errorf("failed to connect to %s", dn(rec.ShardID, rec.ReplicaID))
			return nil
		}
		return conn
	}
	n.ss.setStreamReq(rec, getSinkFn)
	n.pipeline.setStreamReady(n.shardID)
}

func (n *node) canStream() bool {
	if n.ss.streaming() {
		plog.Warningf("%s ignored task.StreamSnapshot", n.id())
		return false
	}
	if !n.sm.ReadyToStream() {
		plog.Warningf("%s is not ready to stream snapshot", n.id())
		return false
	}
	return true
}

func (n *node) reportSaveSnapshot(rec rsm.Task) {
	n.ss.setSaving()
	n.ss.setSaveReq(rec)
	n.pipeline.setSaveReady(n.shardID)
}

func (n *node) reportRecoverSnapshot(rec rsm.Task) {
	n.ss.setRecovering()
	n.ss.setRecoverReq(rec)
	n.pipeline.setRecoverReady(n.shardID)
}

// returns a boolean flag indicating whether to skip task handling for the
// current node
func (n *node) processStatusTransition() bool {
	if n.processSaveStatus() {
		return true
	}
	if n.processStreamStatus() {
		return true
	}
	if n.processRecoverStatus() {
		return true
	}
	if n.processUninitializedNodeStatus() {
		return true
	}
	return false
}

func (n *node) processUninitializedNodeStatus() bool {
	if !n.initialized() {
		plog.Debugf("%s checking initial snapshot", n.id())
		n.reportRecoverSnapshot(rsm.Task{
			Recover: true,
			Initial: true,
			NewNode: n.new,
		})
		return true
	}
	return false
}

func (n *node) processRecoverStatus() bool {
	if n.ss.recovering() {
		rec, ok := n.ss.getRecoverCompleted()
		if !ok {
			return true
		}
		if rec.Save {
			plog.Panicf("got a completed.SnapshotRequested")
		}
		if rec.Initial {
			plog.Infof("%s initialized using %s", n.id(), n.ssid(rec.Index))
			n.setInitialStatus(rec.Index)
			n.sysEvents.Publish(server.SystemEvent{
				Type:      server.NodeReady,
				ShardID:   n.shardID,
				ReplicaID: n.replicaID,
			})
		}
		n.ss.clearRecovering()
	}
	return false
}

func (n *node) processSaveStatus() bool {
	if n.ss.saving() {
		rec, ok := n.ss.getSaveCompleted()
		if !ok {
			return !n.concurrentSnapshot()
		}
		if rec.Save && !n.initialized() {
			plog.Panicf("%s taking snapshot when uninitialized", n.id())
		}
		n.ss.clearSaving()
	}
	return false
}

func (n *node) processStreamStatus() bool {
	if n.ss.streaming() {
		if !n.OnDiskStateMachine() {
			plog.Panicf("non-on disk sm is streaming snapshot")
		}
		if _, ok := n.ss.getStreamCompleted(); !ok {
			return false
		}
		n.ss.clearStreaming()
	}
	return false
}

func (n *node) tick(tick uint64) error {
	n.currentTick++
	n.qs.tick()
	if n.qs.quiesced() {
		if err := n.p.QuiescedTick(); err != nil {
			return err
		}
	} else {
		if err := n.p.Tick(); err != nil {
			return err
		}
	}
	n.pendingSnapshot.tick(tick)
	n.pendingProposals.tick(tick)
	n.pendingReadIndexes.tick(tick)
	n.pendingConfigChange.tick(tick)
	return nil
}

func (n *node) notifySelfRemove() {
	n.sysEvents.Publish(server.SystemEvent{
		Type:      server.NodeDeleted,
		ShardID:   n.shardID,
		ReplicaID: n.replicaID,
	})
}

func (n *node) notifyConfigChange() {
	m := n.sm.GetMembership()
	if len(m.Addresses) == 0 {
		plog.Panicf("empty nodes %s", n.id())
	}
	_, isNonVoting := m.NonVotings[n.replicaID]
	_, isWitness := m.Witnesses[n.replicaID]
	ci := &ShardInfo{
		ShardID:           n.shardID,
		ReplicaID:         n.replicaID,
		IsNonVoting:       isNonVoting,
		IsWitness:         isWitness,
		ConfigChangeIndex: m.ConfigChangeId,
		Replicas:          m.Addresses,
	}
	n.shardInfo.Store(ci)
	n.sysEvents.Publish(server.SystemEvent{
		Type:      server.MembershipChanged,
		ShardID:   n.shardID,
		ReplicaID: n.replicaID,
	})
}

// 在这段代码中，n.sysEvents.Publish 方法被调用以发布一个系统事件。具体来说，这是关于集群成员关系变更（MembershipChanged）的事件。以下是关于这段代码更详细的解释：

// 系统事件对象：
// 这里创建了一个 server.SystemEvent 类型的对象，并设置了其三个字段：

// Type: 设置为 server.MembershipChanged，这通常是一个预定义的常量，表示这是一个集群成员关系变更的事件。
// ShardID: 设置为 n.shardID，这是当前节点所属的分片ID。在分布式系统中，数据可能被分割成多个分片（shards），每个分片由一个或多个节点管理。
// ReplicaID: 设置为 n.replicaID，这是当前节点的副本ID。在分布式系统中，为了确保高可用性和容错性，通常会复制数据并在多个节点上存储副本。
// 发布系统事件：
// n.sysEvents.Publish 方法被调用以发布这个系统事件。我们可以假设 n.sysEvents 是 node 结构体中的一个成员，它可能是某种事件总线或发布/订阅系统的实例。当调用 Publish 方法时，它会将这个 server.SystemEvent 对象发送给所有订阅了这种类型事件的监听器。

// 整个 notifyConfigChange 方法的目的是在配置变更（可能是添加、删除或修改节点）后，通知集群中的其他组件或系统事件监听器关于该变更的信息。这对于保持集群状态的一致性和允许其他组件根据集群的最新配置进行自适应是非常重要的。
func (n *node) getShardInfo() ShardInfo {
	v := n.shardInfo.Load()
	if v == nil {
		return ShardInfo{
			ShardID:          n.shardID,
			ReplicaID:        n.replicaID,
			Pending:          true,
			StateMachineType: sm.Type(n.sm.Type()),
		}
	}
	info := v.(*ShardInfo)

	leaderID := uint64(0)
	term := uint64(0)
	lv := n.leaderInfo.Load()
	if lv != nil {
		leaderInfo := lv.(*leaderInfo)
		leaderID = leaderInfo.leaderID
		term = leaderInfo.term
	}

	return ShardInfo{
		ShardID:           info.ShardID,
		ReplicaID:         info.ReplicaID,
		LeaderID:          leaderID,
		Term:              term,
		IsNonVoting:       info.IsNonVoting,
		ConfigChangeIndex: info.ConfigChangeIndex,
		Replicas:          info.Replicas,
		StateMachineType:  sm.Type(n.sm.Type()),
	}
}

func (n *node) logDBBusy() bool {
	if n.metrics == nil {
		// only happens in tests
		return false
	}
	return n.metrics.isBusy()
}

func (n *node) id() string {
	return dn(n.shardID, n.replicaID)
}

func (n *node) ssid(index uint64) string {
	return logutil.DescribeSS(n.shardID, n.replicaID, index)
}

func (n *node) isLeader() bool {
	v := n.leaderInfo.Load()
	if v == nil {
		return false
	}
	leaderInfo := v.(*leaderInfo)
	if leaderInfo.term == 0 {
		return false
	}
	return n.replicaID == leaderInfo.leaderID
}

func (n *node) isFollower() bool {
	v := n.leaderInfo.Load()
	if v == nil {
		return false
	}
	leaderInfo := v.(*leaderInfo)
	if leaderInfo.term == 0 {
		return false
	}
	return n.replicaID != leaderInfo.leaderID
}

func (n *node) initialized() bool {
	if atomic.LoadUint64(&n.initializedFlag) != 0 {
		return true
	}
	select {
	case <-n.initializedC:
		atomic.StoreUint64(&n.initializedFlag, 1)
		return true
	default:
	}
	return false
}

func (n *node) setInitialized() {
	close(n.initializedC)
}

func (n *node) millisecondSinceStart() uint64 {
	return n.tickMillisecond * n.currentTick
}

func (n *node) getRaftAddress() string {
	return n.raftAddress
}
