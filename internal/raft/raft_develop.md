# raft上层逻辑修改 #

## Fast Boardcast ##

- 逻辑总述(**需要确保不会引入逻辑错误**)：
**broadcastHeartbeatMessage()**进行心跳，原本for循环发很多一样的东西；优化后只发一条，然后增加一个byte标记发给谁，然后将此byte序列化到Appl头 
**handleLeaderPropose()**进行AppendEntries RPC，这里的send(m)可能是msg或者snapshot，思路与心跳一致

- 其它修改：
**丢包**重传应该不是问题，raft的其它地方的逻辑应该有处理丢包的  
**ebpf的map**问题：因为引入了1个byte来标记谁要发，而且先不做其他wait quorum的优化，所以无需沟通uk的map；但是需要限制节点切换IP地址，然后事先将此地址置于一个map中以方便广播   