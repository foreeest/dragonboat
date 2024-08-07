# Go 语言库使用 #

*如何捣腾好第三方库，需要做好版本管理和理解go的找库方式*   

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
最后执行指令即可  
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

4. 如果原来的库依赖原作者库，**不可以**使用如下解决方案，原因暂时不明   

```shell
# replace github.com/lni/dragonboat/v4 => github.com/foreeest/dragonboat v1.0.0
$ go mod tidy
```

## More About GO ##

*go语言开发的一些编译相关的疑问*  

- go import 的是一个网址，那么如果在进行库开发时进行了本地修改，但并未git push到网址上，以及重新拉取下来，修改会生效吗  
通常在库内修改或者`$GOPATH`内修改没有问题，除非go.mod中指定了库的版本，如`v1.0.0`    
若修改无效，可采用replace方案  
```txt
replace github.com/A/xxx => /home/user/projects/xxx
```
即拉取后增加replace语句，然后进行本地开发与测试，而无需频繁地推送；完成开发后，**去掉replace语句**然后进行推送；保险起见是建议都用replace开发     

- what happen when you run `go build`    
找到go.mod，从同目录找一个main函数入口，生成可执行文件；可执行文件名为go.mod的module名，如有github.com等，省略掉    
找到go.mod中需要的库，从`$GOPATH/pkg/mod`中找库找不到就去代理拉取，其中`$GOPATH`一般是`~/go`，可用`$ go env`查看，所以如果直接在这里面找到库进行编辑应该会直接生效，无需**replace**，但是**还没**研究过这样需要注意什么  

- go mod tidy指令   
自动从代码中向go.mod增减依赖库，这里的**库的版本**有讲究，它会下载最新的符合依赖约束的版本；而`go get`指令相较之下可以指定版本  
还会生成go.sum，go在运行代码或者test时会检查go.sum，即若手动修改版本后，如果没有`go mod tidy`或者`go get xxx`会报错   

- go.mod是否影响别人go get?  
**replace**只在本仓库生效
详情可搜一篇csdn博客`Go mod 学习之 replace 篇`   

## More About Git ##

- 如何理解HEAD
当前你在什么分支，如果你不在任何branch就会显示如`* (HEAD detached at 8003649)`，此时无法提交  
`remotes/origin/HEAD -> origin/master`如何理解？HEAD指向的是当前准备修改的地方，不论是在remote还是本地  

- 删除(慎用)

```shell
$ git remote -v
$ git remote remove origin # 整个删
$ git branch -a
$ git push origin --delete remoteBranchName # 删远程
$ git branch -d localBranchName # 删本地
```