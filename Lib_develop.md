# Go 语言库使用 #

## 库管理 ##
目前v2的版本是`My_Message`+udp版；master是udp版    

- 版本管理

**tag(标签)与branch(分支)**: 发布一个tag，此tag对应到一个branch的某个版本的，一般不会再修改。branch是会流动变化的，如果完成了代码的阶段性修改可以发布一个版本，如果随便改改又想上传直接更新到一个branch，无需开一个版本号了  

```shell
$ git clone https://github.com/foreeest/dragonboat
$ cd dragonboat
$ git branch -a # 查看所有本地和远程的仓库分支
$ git checkout v2 # 再输上述指令可以看到*号在v2这里
```
进行开发，完成开发后准备提交
**if** 只是提交暂存修改，并不发布tag  
```shell
$ git add .
$ git commit -m "whatever"
$ git push origin v2 # push后第一个参数是远程分支，第二个参数是本地分支
```
**else if** 想要发布版本号
```shell
$ git add .
$ git commit -m "whatever"
$ git tag -a v2.0.0 -m "whatever" # 版本号改成你想要改成的，要比原先已有的大，不要覆盖
$ git push origin master # push后第一个参数是远程分支，第二个参数是本地分支
$ git push origin v2.0.0
```

- 远程与本地

```shell
$ git branch -a # 查看所有分支，包括远程和本地
$ git push origin --delete udp # 删除远程叫udp的分支
```

- 不同分支
从原作者版本，创建一个新分支：包括创建和修改url的名字为自己的名字  
```shell
$ git branch v2
$ git checkout v2
$ go mod init github.com/foreeest/dragonboat/v2
$ git tag -a v2.0.0 -m "My_Message+udp initial version"
$ git push origin v2
$ git push origin v2.0.0 
```

需要优雅地将原来库import改成自己的，除了暴力字符串匹配，可以在**go.mod**加一句  
```txt
replace github.com/lni/dragonboat/v4 => github.com/foreeest/dragonboat/v2 v2.0.0
```
最后，应该就ok了
```shell
$ go mod tidy
```

## 库使用 ##

1. 创建文件夹，并编写代码
```shell
$ mkdir myproject
$ cd myproject
```
2. 获取第三方库
if new  
```shell
$ go mod init main
$ go mod tidy
```
else  
确保你的`@`前的url和github上你想拉的仓库的go.mod第一行**module**相同，`@`后版本号与github仓库页面左上部分tag相同
```shell
$ go get github.com/foreeest/dragonboat@v1.0.0
```

3. 重新编译，然后可以运行可执行文件

```shell
$ go build
```

4. 如果原来的库依赖原作者库，也可以使用

```shell
# replace github.com/lni/dragonboat/v4 => github.com/foreeest/dragonboat v1.0.0
$ go mod tidy
```