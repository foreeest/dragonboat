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
$ git push origin v2 # push后第一个参数是远程分支，第二个参数是本地分支；本句和下一句都要输
$ git push origin v2.0.0
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

如果~~妄想~~需要优雅地将原来库import改成自己的，除了暴力字符串匹配，可以在**go.mod**加一句  
```txt
replace github.com/foreeest/dragonboat/v2 => github.com/foreeest/dragonboat/v2 v2.0.0
```
最后执行指令即可  
```shell
$ go mod tidy
```
但这会面临一个问题，即更新了v2分支后还是引用v2.0.0  
不是不能解决，而是我**太菜**，所以我决定**暴力匹配**  
```shell
$ cd dragonboat
$ find . -type f -exec perl -pi -e 's|github.com/lni/dragonboat/v4|github.com/foreeest/dragonboat/v2|g' {} +
$ go mod init github.com/foreeest/dragonboat/v2
$ go mod tidy
```

NOTE: 不是说**replace**不好，而是这个fork场景下replace难以理解、真伪难辨；其他场景下当然可以用；这还有个问题就是暴力字符串会不会**误伤**，譬如想把dragon改成elephant，结果意外得到elephantboat，但如上的串串说实话能误伤的只有go.mod了，但我重新init了       

## 库使用 ##

1. 创建文件夹，并编写代码

```shell
$ mkdir myproject
$ cd myproject
```

2. 获取第三方库

**if** new  
```shell
$ go mod init main
$ go mod tidy
```
**else**    
确保你的`@`前的url和github上你想拉的仓库的go.mod第一行**module**相同，`@`后版本号与github仓库页面左上部分tag相同
```shell
$ go get github.com/foreeest/dragonboat@v1.0.0
```

4. 重新编译，然后可以运行可执行文件

```shell
$ go build
```

5. 如果原来的库依赖原作者库，即是一个fork库，可能需要**replace**，且**replace**建议与此fork库的replace一致    
其原因more about go部分，我建议不要纠缠这个     
```shell
# replace github.com/foreeest/dragonboat/v2 => github.com/foreeest/dragonboat v1.0.0
$ go mod tidy
```

## More About GO ##

*go语言开发的一些编译相关的疑问*  

- go import 的是一个网址，那么如果在进行库开发时进行了本地修改，但并未git push到网址上，以及重新拉取下来，修改会生效吗？  
  
通常在库内修改或者`$GOPATH`内修改没有问题，除非go.mod中指定了库的版本，如`v1.0.0`    
若修改无效，可采用**replace**方案  
```txt
replace github.com/A/xxx => /home/user/projects/xxx
```
即拉取后增加**replace**语句，然后进行本地开发与测试，而无需频繁地推送；完成开发后，可去掉也可不去replace语句然后进行推送；保险起见是建议都用replace开发     
有说用如下也可以的，0.0.0则是占位符；实测至少有些情况不行，可以暂时忽略  
```txt
replace github.com/A/xxx => github.com/B/xxx v0.0.0
```

- what happen when you run `go build`    
找到go.mod，从同目录找一个main函数入口，生成可执行文件；可执行文件名为go.mod的module名，如有github.com等，省略掉    
找到go.mod中需要的库，从`$GOPATH/pkg/mod`中找库找不到就去代理拉取，其中`$GOPATH`一般是`~/go`，可用`$ go env`查看，所以如果直接在这里面找到库进行编辑应该会直接生效，无需**replace**，但是**还没**研究过这样需要注意什么  

- `go mod tidy`指令   
自动从代码中向go.mod增减依赖库，这里的**库的版本**有讲究，它会下载最新的符合依赖约束的版本；而`go get`指令相较之下可以指定版本  
还会生成go.sum，go在运行代码或者test时会检查go.sum，即若手动修改版本后，如果没有`go mod tidy`或者`go get xxx`会报错   

- go.mod是否影响别人go get?  
**replace**只在本仓库生效，所以上传库不删replace应该也行，其他项目要require此库，会看go.mod但不会看其中的**replace**语句  
用replace时可以在require中寻找到想要换掉的，然后replace写完，如果require的是本地路径应该无需`go mod tidy`，如果是版本号应该是需要的   
还有一个**问题**是：譬如我的fork中都是import lni/dragonboat，那如果我的fork版dragonboat的**replace**在其他依赖my fork的项目不生效，那此项目能正常用my fork，而不是一部分my fork dragonboat，一部分lni/dragonboat？  
**不会生效！**所以必须在用my fork/xxx的项目中也加入**replace**，这样此项目也会有替换，但这样我不好说有没有效，建议暴力字符串匹配；所以拉自己的库不是这么简单的事，得考虑自己的库是不是个fork，即没有把所有import名字改成my name   
**replace**进一步了解可搜一篇csdn博客`Go mod 学习之 replace 篇`   

## More About Git ##

- 如何理解HEAD
当前你在什么分支，如果你不在任何branch就会显示如`* (HEAD detached at 8003649)`，此时无法提交  
`remotes/origin/HEAD -> origin/master`如何理解？HEAD指向的是当前准备修改的地方，不论是在remote还是本地  

- 删除(慎用)

```shell
$ git remote -v
$ git remote remove origin # 整个删
$ git branch -a # 查看所有分支，包括远程和本地
$ git push origin --delete remoteBranchName # 删远程
$ git branch -d localBranchName # 删本地
```

- 新建
```shell
$ git init
$ git remote add origin url
# 开发
$ git add .
$ git commit -m "whatever"
$ git push -u origin master # 要有u，不然可能出现奇怪的deatched，u是用于关联的
```

- 拉取
```shell
$ git pull origin v2 # merge可能涉及rebase的选择，自行了解
$ git log # 看本地和remote的HEAD在不在一起
```