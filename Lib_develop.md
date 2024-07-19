# Go 语言库使用 #

## 库管理 ##


- 远程与本地

```shell
$ git branch -a # 查看所有分支，包括远程和本地
$ git push origin --delete udp # 删除远程叫udp的分支
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


1. 重新编译
```shell
$ go build
```



## TODO ##

- [ ] 修改与运行流程
- [ ] 如何进行有序的版本管理
- [ ] 如何高效从别人的库创建fork